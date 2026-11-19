package chat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MemStoreOptions configures a MemStore.
type MemStoreOptions struct {
	// MaxEventsPerTurn bounds the replay log of one turn. Older events are
	// dropped first, which makes an old cursor report ErrResyncRequired.
	// Zero means DefaultMaxEventsPerTurn.
	MaxEventsPerTurn int
}

// DefaultMaxEventsPerTurn is the retention bound used when options leave it
// unset.
const DefaultMaxEventsPerTurn = 1000

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
	idLength         = 15
	idAlphabet       = "0123456789abcdefghijklmnopqrstuvwxyz"
)

type convRecord struct {
	conversation Conversation
	messages     []Message
	turnIDs      []string
	proposalIDs  []string
	uploadIDs    []string
	// summary is the conversation's summary; nil when it has none. It goes
	// with the conversation when that is deleted.
	summary *Summary
}

type uploadRecord struct {
	upload Upload
	data   []byte
}

type turnRecord struct {
	turn Turn
	// events is the retained replay window, ascending by Seq.
	events []Event
	// lastSeq is the last assigned sequence, retained or not.
	lastSeq int64
	// droppedBefore is the last Seq evicted by retention, 0 if none.
	droppedBefore int64
	// terminal is set once the turn's one terminal event is stored.
	terminal bool
}

// MemStore is an in-memory Store, ProposalStore and UploadStore for tests and
// single-instance use. It is process-local and lost on restart; durable
// adapters implement the same ports. One mutex serializes every operation,
// which makes CreateTurn's active-turn claim and every proposal compare-and-swap
// race-safe.
type MemStore struct {
	mu        sync.Mutex
	maxEvents int
	convs     map[string]*convRecord
	turns     map[string]*turnRecord
	proposals map[string]*Proposal
	uploads   map[string]*uploadRecord
}

var _ Store = (*MemStore)(nil)
var _ SummaryStore = (*MemStore)(nil)
var _ ProposalStore = (*MemStore)(nil)
var _ ActiveTurnStore = (*MemStore)(nil)
var _ RecentMessageStore = (*MemStore)(nil)
var _ UploadStore = (*MemStore)(nil)

// NewMemStore returns an empty store.
func NewMemStore(opts MemStoreOptions) *MemStore {
	max := opts.MaxEventsPerTurn
	if max <= 0 {
		max = DefaultMaxEventsPerTurn
	}
	return &MemStore{
		maxEvents: max,
		convs:     make(map[string]*convRecord),
		turns:     make(map[string]*turnRecord),
		proposals: make(map[string]*Proposal),
		uploads:   make(map[string]*uploadRecord),
	}
}

func now() time.Time { return time.Now().UTC() }

// newID returns a random 15-character lowercase alphanumeric identifier.
func newID() string {
	buf := make([]byte, idLength)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail on supported platforms; a panic is better
		// than silently issuing colliding identifiers.
		panic("chat: crypto/rand failed: " + err.Error())
	}
	for i, b := range buf {
		buf[i] = idAlphabet[int(b)%len(idAlphabet)]
	}
	return string(buf)
}

func (s *MemStore) newUniqueID(taken func(string) bool) string {
	for {
		id := newID()
		if !taken(id) {
			return id
		}
	}
}

func (s *MemStore) conversationLocked(id string) (*convRecord, error) {
	rec, ok := s.convs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return rec, nil
}

func (s *MemStore) ownedConversationLocked(owner OwnerKey, id string) (*convRecord, error) {
	rec, err := s.conversationLocked(id)
	if err != nil {
		return nil, err
	}
	if !rec.conversation.Owner.Equal(owner) {
		return nil, ErrNotFound
	}
	return rec, nil
}

func (s *MemStore) ownedTurnLocked(owner OwnerKey, id string) (*turnRecord, error) {
	rec, ok := s.turns[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !rec.turn.Owner.Equal(owner) {
		return nil, ErrNotFound
	}
	return rec, nil
}

func (s *MemStore) ownedProposalLocked(owner OwnerKey, id string) (*Proposal, error) {
	p, ok := s.proposals[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !p.Owner.Equal(owner) {
		return nil, ErrNotFound
	}
	return p, nil
}

// activeTurnLocked reports the conversation's non-terminal turn, if any.
func (s *MemStore) activeTurnLocked(c *convRecord) *turnRecord {
	for _, id := range c.turnIDs {
		rec, ok := s.turns[id]
		if !ok {
			continue
		}
		if !rec.turn.State.Terminal() {
			return rec
		}
	}
	return nil
}

// CreateConversation stores a new conversation, assigning ID and timestamps
// when the caller left them zero.
func (s *MemStore) CreateConversation(_ context.Context, c Conversation) (Conversation, error) {
	if err := c.Owner.Validate(); err != nil {
		return Conversation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if c.ID == "" {
		c.ID = s.newUniqueID(func(id string) bool { _, ok := s.convs[id]; return ok })
	} else if _, ok := s.convs[c.ID]; ok {
		return Conversation{}, ErrConflict
	}
	t := now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = t
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	s.convs[c.ID] = &convRecord{conversation: cloneConversation(c)}
	return cloneConversation(c), nil
}

// GetConversation returns one caller-owned conversation.
func (s *MemStore) GetConversation(_ context.Context, owner OwnerKey, id string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, id)
	if err != nil {
		return Conversation{}, err
	}
	return cloneConversation(rec.conversation), nil
}

// ListConversations returns caller-owned conversations newest first.
func (s *MemStore) ListConversations(_ context.Context, owner OwnerKey, target string, page Page) ([]Conversation, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items := make([]Conversation, 0, len(s.convs))
	for _, rec := range s.convs {
		c := rec.conversation
		if !c.Owner.Equal(owner) {
			continue
		}
		if target != "" && c.Target != target {
			continue
		}
		items = append(items, cloneConversation(c))
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID > items[j].ID
	})

	limit := pageLimit(page)
	after, err := decodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	out := make([]Conversation, 0, limit)
	var next string
	for _, c := range items {
		if after != nil && !(c.CreatedAt.Before(after.at) || (c.CreatedAt.Equal(after.at) && c.ID < after.id)) {
			continue
		}
		if len(out) == limit {
			next = encodeCursor(out[len(out)-1].CreatedAt, out[len(out)-1].ID)
			break
		}
		out = append(out, c)
	}
	return out, next, nil
}

// RenameConversation changes a conversation's title.
func (s *MemStore) RenameConversation(_ context.Context, owner OwnerKey, id, title string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, id)
	if err != nil {
		return Conversation{}, err
	}
	rec.conversation.Title = title
	rec.conversation.UpdatedAt = now()
	return cloneConversation(rec.conversation), nil
}

// DeleteConversation removes a conversation and its data, refusing while a
// non-terminal turn exists or while one of its proposals is executing. An
// executing proposal may still produce a server effect, so its conversation
// cannot disappear underneath it.
func (s *MemStore) DeleteConversation(_ context.Context, owner OwnerKey, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, id)
	if err != nil {
		return err
	}
	if s.activeTurnLocked(rec) != nil {
		return ErrActiveTurn
	}
	for _, pid := range rec.proposalIDs {
		if p, ok := s.proposals[pid]; ok && p.State == ProposalStateExecuting {
			return ErrConflict
		}
	}
	for _, pid := range rec.proposalIDs {
		delete(s.proposals, pid)
	}
	for _, uid := range rec.uploadIDs {
		delete(s.uploads, uid)
	}
	for _, tid := range rec.turnIDs {
		delete(s.turns, tid)
	}
	delete(s.convs, id)
	return nil
}

// AppendMessage stores one transcript entry.
func (s *MemStore) AppendMessage(_ context.Context, owner OwnerKey, m Message) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, m.ConversationID)
	if err != nil {
		return Message{}, err
	}
	if m.TurnID != "" {
		t, ok := s.turns[m.TurnID]
		if !ok || !t.turn.Owner.Equal(owner) || t.turn.ConversationID != m.ConversationID {
			return Message{}, ErrNotFound
		}
	}
	if m.ID == "" {
		m.ID = s.newUniqueID(func(id string) bool { return s.messageIDTaken(id) })
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now()
	}
	stored := cloneMessage(m)
	rec.messages = append(rec.messages, stored)
	return cloneMessage(stored), nil
}

func (s *MemStore) messageIDTaken(id string) bool {
	for _, rec := range s.convs {
		for _, m := range rec.messages {
			if m.ID == id {
				return true
			}
		}
	}
	return false
}

// ListMessages returns a conversation transcript oldest first.
// ActiveTurn implements ActiveTurnStore.
func (s *MemStore) ActiveTurn(_ context.Context, owner OwnerKey, conversationID string) (Turn, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, conversationID)
	if err != nil {
		return Turn{}, false, err
	}
	for _, id := range rec.turnIDs {
		if t, ok := s.turns[id]; ok && !t.turn.State.Terminal() {
			return t.turn, true, nil
		}
	}
	return Turn{}, false, nil
}

// FindTurnByIdempotencyKey implements IdempotentTurnStore.
func (s *MemStore) FindTurnByIdempotencyKey(_ context.Context, owner OwnerKey, conversationID, key string) (Turn, bool, error) {
	if key == "" {
		return Turn{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, conversationID)
	if err != nil {
		return Turn{}, false, err
	}
	for _, id := range rec.turnIDs {
		if t, ok := s.turns[id]; ok && t.turn.IdempotencyKey == key && t.turn.Owner.Equal(owner) {
			return cloneTurn(t.turn), true, nil
		}
	}
	return Turn{}, false, nil
}

// RecentMessages implements RecentMessageStore.
func (s *MemStore) RecentMessages(_ context.Context, owner OwnerKey, conversationID string, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, conversationID)
	if err != nil {
		return nil, err
	}
	items := make([]Message, len(rec.messages))
	for i, m := range rec.messages {
		items[i] = cloneMessage(m)
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
	if limit > 0 && len(items) > limit {
		items = items[len(items)-limit:]
	}
	return items, nil
}

func (s *MemStore) ListMessages(_ context.Context, owner OwnerKey, conversationID string, page Page) ([]Message, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, conversationID)
	if err != nil {
		return nil, "", err
	}
	items := make([]Message, len(rec.messages))
	for i, m := range rec.messages {
		items[i] = cloneMessage(m)
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})

	limit := pageLimit(page)
	after, err := decodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	out := make([]Message, 0, limit)
	var next string
	for _, m := range items {
		if after != nil && !(m.CreatedAt.After(after.at) || (m.CreatedAt.Equal(after.at) && m.ID > after.id)) {
			continue
		}
		if len(out) == limit {
			next = encodeCursor(out[len(out)-1].CreatedAt, out[len(out)-1].ID)
			break
		}
		out = append(out, m)
	}
	return out, next, nil
}

// CreateTurn claims the conversation's active turn or returns the turn already
// created under the same idempotency key.
func (s *MemStore) CreateTurn(_ context.Context, owner OwnerKey, t Turn) (Turn, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conv, err := s.ownedConversationLocked(owner, t.ConversationID)
	if err != nil {
		return Turn{}, false, err
	}
	if t.IdempotencyKey != "" {
		for _, id := range conv.turnIDs {
			existing, ok := s.turns[id]
			if !ok {
				continue
			}
			if existing.turn.IdempotencyKey == t.IdempotencyKey && existing.turn.Owner.Equal(owner) {
				return cloneTurn(existing.turn), false, nil
			}
		}
	}
	if s.activeTurnLocked(conv) != nil {
		return Turn{}, false, ErrActiveTurn
	}

	if t.ID == "" {
		t.ID = s.newUniqueID(func(id string) bool { _, ok := s.turns[id]; return ok })
	} else if _, ok := s.turns[t.ID]; ok {
		return Turn{}, false, ErrConflict
	}
	// The claim always inserts queued work; a caller cannot skip the state
	// machine by supplying another state.
	t.State = TurnStateQueued
	t.Owner = owner
	ts := now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = ts
	}
	t.UpdatedAt = t.CreatedAt
	t.Error = nil

	rec := &turnRecord{turn: cloneTurn(t)}
	s.turns[t.ID] = rec
	conv.turnIDs = append(conv.turnIDs, t.ID)
	// A new turn is conversation activity: the list's "updated" time follows it.
	conv.conversation.UpdatedAt = t.CreatedAt
	return cloneTurn(t), true, nil
}

// GetTurn returns one caller-owned turn.
func (s *MemStore) GetTurn(_ context.Context, owner OwnerKey, id string) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedTurnLocked(owner, id)
	if err != nil {
		return Turn{}, err
	}
	return cloneTurn(rec.turn), nil
}

// TransitionTurn applies a compare-and-swap state change. A terminal target is
// refused, because a terminal state must be stored together with its terminal
// event by FinishTurn.
func (s *MemStore) TransitionTurn(_ context.Context, owner OwnerKey, id string, from, to TurnState, failure *TurnFailed) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedTurnLocked(owner, id)
	if err != nil {
		return Turn{}, err
	}
	if rec.turn.State != from {
		return Turn{}, ErrConflict
	}
	if !CanTransition(from, to) {
		return Turn{}, ErrInvalidTransition
	}
	if to.Terminal() {
		return Turn{}, ErrInvalidTransition
	}
	rec.turn.State = to
	rec.turn.UpdatedAt = now()
	if failure != nil {
		rec.turn.Error = cloneFailure(failure)
	}
	return cloneTurn(rec.turn), nil
}

// FinishTurn ends a turn and appends its terminal event in one critical
// section, so the state and the event can never disagree.
func (s *MemStore) FinishTurn(_ context.Context, owner OwnerKey, id string, from, to TurnState, ev Event) (Turn, Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.ownedTurnLocked(owner, id)
	if err != nil {
		return Turn{}, Event{}, err
	}
	if rec.turn.State != from {
		return Turn{}, Event{}, ErrConflict
	}
	if !to.Terminal() || !CanTransition(from, to) {
		return Turn{}, Event{}, ErrInvalidTransition
	}
	want, ok := terminalEventFor(to)
	if !ok || ev.Type != want {
		return Turn{}, Event{}, ErrInvalidTransition
	}
	if ev.TurnID != "" && ev.TurnID != id {
		return Turn{}, Event{}, ErrConflict
	}
	ev.TurnID = id
	if ev.ConversationID == "" {
		ev.ConversationID = rec.turn.ConversationID
	}
	if rec.terminal {
		return Turn{}, Event{}, ErrConflict
	}

	failure, err := terminalFailure(to, ev)
	if err != nil {
		return Turn{}, Event{}, err
	}
	stored, err := s.appendEventLocked(rec, ev)
	if err != nil {
		return Turn{}, Event{}, err
	}
	rec.turn.State = to
	rec.turn.UpdatedAt = now()
	rec.turn.Error = failure
	return cloneTurn(rec.turn), stored, nil
}

// terminalFailure decodes the failure a failed or interrupted turn records.
// A completed or cancelled turn never carries a failure.
func terminalFailure(to TurnState, ev Event) (*TurnFailed, error) {
	switch to {
	case TurnStateFailed, TurnStateInterrupted:
		var f TurnFailed
		if len(ev.Data) == 0 {
			return nil, fmt.Errorf("chat: %s event without data: %w", ev.Type, ErrConflict)
		}
		if err := json.Unmarshal(ev.Data, &f); err != nil {
			return nil, fmt.Errorf("chat: decode %s data: %w", ev.Type, ErrConflict)
		}
		return &f, nil
	}
	return nil, nil
}

// AppendEvent assigns the next sequence and timestamp and stores the event.
// Terminal event types belong to FinishTurn and are refused here.
func (s *MemStore) AppendEvent(_ context.Context, owner OwnerKey, e Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedTurnLocked(owner, e.TurnID)
	if err != nil {
		return Event{}, err
	}
	if e.Type.Terminal() {
		return Event{}, ErrInvalidTransition
	}
	return s.appendEventLocked(rec, e)
}

func (s *MemStore) appendEventLocked(rec *turnRecord, e Event) (Event, error) {
	if !e.Type.Valid() {
		return Event{}, fmt.Errorf("chat: invalid event type %q: %w", string(e.Type), ErrConflict)
	}
	if rec.terminal {
		return Event{}, ErrConflict
	}
	if e.Version == "" {
		e.Version = ProtocolVersion
	}
	if e.ConversationID == "" {
		e.ConversationID = rec.turn.ConversationID
	}
	if e.TurnID == "" {
		e.TurnID = rec.turn.ID
	}
	rec.lastSeq++
	e.Seq = rec.lastSeq
	e.At = now()

	rec.events = append(rec.events, cloneEvent(e))
	if e.Type.Terminal() {
		rec.terminal = true
	}
	if over := len(rec.events) - s.maxEvents; over > 0 {
		rec.droppedBefore = rec.events[over-1].Seq
		rec.events = append([]Event(nil), rec.events[over:]...)
	}
	return cloneEvent(e), nil
}

// EventsAfter replays retained events with Seq > after.
func (s *MemStore) EventsAfter(_ context.Context, owner OwnerKey, turnID string, after int64) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedTurnLocked(owner, turnID)
	if err != nil {
		return nil, err
	}
	if rec.droppedBefore > 0 && after < rec.droppedBefore {
		return nil, ErrResyncRequired
	}
	out := make([]Event, 0, len(rec.events))
	for _, e := range rec.events {
		if e.Seq > after {
			out = append(out, cloneEvent(e))
		}
	}
	return out, nil
}

// InterruptRunning ends every queued or running turn as interrupted and stores
// one turn.failed terminal event for each, so no stream is left without an
// outcome. No work is rerun.
func (s *MemStore) InterruptRunning(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, rec := range s.turns {
		if rec.turn.State.Terminal() {
			continue
		}
		rec.turn.State = TurnStateInterrupted
		rec.turn.UpdatedAt = now()
		rec.turn.Error = &TurnFailed{Code: "interrupted", Message: "turn interrupted by server restart"}
		// A turn that already stored a terminal event keeps that one event;
		// appending another would give the stream two outcomes.
		if !rec.terminal {
			if _, err := s.appendEventLocked(rec, Event{
				Type: EventTurnFailed,
				Data: mustJSON(TurnFailed{Code: "interrupted", Message: "turn interrupted by server restart"}),
			}); err != nil {
				return n, err
			}
		}
		n++
	}
	return n, nil
}

// CreateProposal stores a new pending proposal, assigning ID, state, revision
// and timestamps. Arguments are canonicalized so the stored bytes are exactly
// what ArgumentsHash covers.
func (s *MemStore) CreateProposal(_ context.Context, owner OwnerKey, p Proposal) (Proposal, error) {
	if err := owner.Validate(); err != nil {
		return Proposal{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	conv, err := s.ownedConversationLocked(owner, p.ConversationID)
	if err != nil {
		return Proposal{}, err
	}
	canonical, hash, err := HashArguments(p.Arguments)
	if err != nil {
		return Proposal{}, err
	}
	if p.ArgumentsHash != "" && p.ArgumentsHash != hash {
		return Proposal{}, fmt.Errorf("chat: proposal arguments hash does not match: %w", ErrConflict)
	}
	p.Arguments = canonical
	p.ArgumentsHash = hash
	// The argument key is the caller's, never the record's: a proposal cannot
	// be moved into another partition by setting Owner.
	p.Owner = owner
	p.State = ProposalStatePending
	p.Revision = 1
	p.DecidedAt = nil
	p.Result = nil
	p.Failure = ""
	ts := now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = ts
	}
	p.UpdatedAt = p.CreatedAt
	if p.ID == "" {
		p.ID = s.newUniqueID(func(id string) bool { _, ok := s.proposals[id]; return ok })
	} else if _, ok := s.proposals[p.ID]; ok {
		return Proposal{}, ErrConflict
	}
	if err := p.Validate(); err != nil {
		return Proposal{}, err
	}
	stored := cloneProposal(p)
	s.proposals[p.ID] = &stored
	conv.proposalIDs = append(conv.proposalIDs, p.ID)
	return cloneProposal(p), nil
}

// GetProposal returns one caller-owned proposal.
func (s *MemStore) GetProposal(_ context.Context, owner OwnerKey, id string) (Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.ownedProposalLocked(owner, id)
	if err != nil {
		return Proposal{}, err
	}
	return cloneProposal(*p), nil
}

// ListProposals returns one conversation's proposals oldest first, optionally
// filtered to a set of states.
func (s *MemStore) ListProposals(_ context.Context, owner OwnerKey, conversationID string, states []ProposalState, page Page) ([]Proposal, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conv, err := s.ownedConversationLocked(owner, conversationID)
	if err != nil {
		return nil, "", err
	}
	filter := make(map[ProposalState]struct{}, len(states))
	for _, st := range states {
		filter[st] = struct{}{}
	}
	items := make([]Proposal, 0, len(conv.proposalIDs))
	for _, id := range conv.proposalIDs {
		rec, ok := s.proposals[id]
		if !ok {
			continue
		}
		if len(filter) > 0 {
			if _, ok := filter[rec.State]; !ok {
				continue
			}
		}
		items = append(items, cloneProposal(*rec))
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})

	limit := pageLimit(page)
	after, err := decodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	out := make([]Proposal, 0, limit)
	var next string
	for _, p := range items {
		if after != nil && !(p.CreatedAt.After(after.at) || (p.CreatedAt.Equal(after.at) && p.ID > after.id)) {
			continue
		}
		if len(out) == limit {
			next = encodeCursor(out[len(out)-1].CreatedAt, out[len(out)-1].ID)
			break
		}
		out = append(out, p)
	}
	return out, next, nil
}

// TransitionProposal applies a compare-and-swap state change and records the
// update's fields. Every successful mutation increments Revision, so a caller
// that holds a revision holds a complete decision token.
func (s *MemStore) TransitionProposal(_ context.Context, owner OwnerKey, id string, from ProposalState, revision int, to ProposalState, u ProposalUpdate) (Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedProposalLocked(owner, id)
	if err != nil {
		return Proposal{}, err
	}
	if rec.State != from || rec.Revision != revision {
		return Proposal{}, ErrConflict
	}
	if !CanTransitionProposal(rec.Location, from, to) {
		return Proposal{}, ErrInvalidTransition
	}
	ts := now()
	if rec.State == ProposalStatePending && expiredAt(rec.ExpiresAt, ts) && to != ProposalStateExpired {
		return Proposal{}, ErrConflict
	}
	rec.State = to
	rec.Revision++
	rec.UpdatedAt = ts
	if u.Result != nil {
		rec.Result = cloneRaw(u.Result)
	}
	if u.Failure != "" {
		rec.Failure = u.Failure
	}
	if u.EditorRevision != "" {
		rec.EditorRevision = u.EditorRevision
	}
	if u.Hunks != nil {
		rec.Hunks = cloneHunks(u.Hunks)
	}
	if u.Decided {
		decided := ts
		rec.DecidedAt = &decided
	}
	if !u.ExpiresAt.IsZero() {
		rec.ExpiresAt = u.ExpiresAt.UTC()
	}
	return cloneProposal(*rec), nil
}

// ReviseProposal replaces a pending proposal's arguments under a revision
// compare-and-swap, for hosts that allow editing before a decision.
func (s *MemStore) ReviseProposal(_ context.Context, owner OwnerKey, id string, revision int, args json.RawMessage, argsHash string) (Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedProposalLocked(owner, id)
	if err != nil {
		return Proposal{}, err
	}
	if rec.State != ProposalStatePending || rec.Revision != revision {
		return Proposal{}, ErrConflict
	}
	canonical, hash, err := HashArguments(args)
	if err != nil {
		return Proposal{}, err
	}
	if argsHash != "" && argsHash != hash {
		return Proposal{}, fmt.Errorf("chat: proposal arguments hash does not match: %w", ErrConflict)
	}
	rec.Arguments = canonical
	rec.ArgumentsHash = hash
	rec.Revision++
	rec.UpdatedAt = now()
	return cloneProposal(*rec), nil
}

// ExpireProposals ends every undecided proposal whose expiry has passed.
func (s *MemStore) ExpireProposals(_ context.Context, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, rec := range s.proposals {
		if rec.ExpiresAt.IsZero() || rec.ExpiresAt.After(at) {
			continue
		}
		if rec.State != ProposalStatePending && rec.State != ProposalStateApproved {
			continue
		}
		rec.State = ProposalStateExpired
		rec.Revision++
		rec.UpdatedAt = at.UTC()
		n++
	}
	return n, nil
}

// RecoverExecuting marks every proposal that was executing at restart as
// unknown. The effect may have happened, so nothing is rerun and the user has
// to reconcile it.
func (s *MemStore) RecoverExecuting(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, rec := range s.proposals {
		if rec.State != ProposalStateExecuting {
			continue
		}
		rec.State = ProposalStateUnknown
		rec.Revision++
		rec.UpdatedAt = now()
		rec.Failure = UnknownOutcomeMessage
		n++
	}
	return n, nil
}

// expiredAt reports whether a non-zero expiry is at or before at.
func expiredAt(expires, at time.Time) bool {
	return !expires.IsZero() && !expires.After(at)
}

// --- uploads ---

// ownedUploadLocked returns one caller-owned upload and the conversation it
// belongs to. A record that survives its conversation, or whose owner differs,
// is reported as missing.
func (s *MemStore) ownedUploadLocked(owner OwnerKey, id string) (*uploadRecord, *convRecord, error) {
	rec, ok := s.uploads[id]
	if !ok || !rec.upload.Owner.Equal(owner) {
		return nil, nil, ErrNotFound
	}
	conv, err := s.ownedConversationLocked(owner, rec.upload.ConversationID)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	return rec, conv, nil
}

// CreateUpload stores one attachment, assigning its id, timestamp, size and
// SHA-256 and copying the bytes it was given.
func (s *MemStore) CreateUpload(_ context.Context, owner OwnerKey, u Upload, data []byte) (Upload, error) {
	if err := owner.Validate(); err != nil {
		return Upload{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	conv, err := s.ownedConversationLocked(owner, u.ConversationID)
	if err != nil {
		return Upload{}, err
	}
	if u.ID == "" {
		u.ID = s.newUniqueID(func(id string) bool { _, ok := s.uploads[id]; return ok })
	} else if _, ok := s.uploads[u.ID]; ok {
		return Upload{}, ErrConflict
	}
	sum := sha256.Sum256(data)
	u.Owner = owner
	u.Size = int64(len(data))
	u.SHA256 = uploadSHA256Prefix + hex.EncodeToString(sum[:])
	u.CreatedAt = now()
	stored := cloneUpload(u)
	s.uploads[u.ID] = &uploadRecord{upload: stored, data: append([]byte(nil), data...)}
	conv.uploadIDs = append(conv.uploadIDs, u.ID)
	return cloneUpload(u), nil
}

// GetUpload returns one caller-owned upload's metadata.
func (s *MemStore) GetUpload(_ context.Context, owner OwnerKey, id string) (Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, _, err := s.ownedUploadLocked(owner, id)
	if err != nil {
		return Upload{}, err
	}
	return cloneUpload(rec.upload), nil
}

// ReadUpload returns one caller-owned upload and a copy of its bytes.
func (s *MemStore) ReadUpload(_ context.Context, owner OwnerKey, id string) (Upload, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, _, err := s.ownedUploadLocked(owner, id)
	if err != nil {
		return Upload{}, nil, err
	}
	return cloneUpload(rec.upload), append([]byte(nil), rec.data...), nil
}

// ListUploads returns one conversation's uploads, newest first.
func (s *MemStore) ListUploads(_ context.Context, owner OwnerKey, conversationID string, page Page) ([]Upload, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conv, err := s.ownedConversationLocked(owner, conversationID)
	if err != nil {
		return nil, "", err
	}
	items := make([]Upload, 0, len(conv.uploadIDs))
	for _, id := range conv.uploadIDs {
		rec, ok := s.uploads[id]
		if !ok {
			continue
		}
		items = append(items, cloneUpload(rec.upload))
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID > items[j].ID
	})

	limit := pageLimit(page)
	after, err := decodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	out := make([]Upload, 0, limit)
	var next string
	for _, u := range items {
		if after != nil && !(u.CreatedAt.Before(after.at) || (u.CreatedAt.Equal(after.at) && u.ID < after.id)) {
			continue
		}
		if len(out) == limit {
			next = encodeCursor(out[len(out)-1].CreatedAt, out[len(out)-1].ID)
			break
		}
		out = append(out, u)
	}
	return out, next, nil
}

// CountUploads returns how many uploads one conversation holds.
func (s *MemStore) CountUploads(_ context.Context, owner OwnerKey, conversationID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conv, err := s.ownedConversationLocked(owner, conversationID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range conv.uploadIDs {
		if _, ok := s.uploads[id]; ok {
			n++
		}
	}
	return n, nil
}

// --- helpers ---

type cursorKey struct {
	at time.Time
	id string
}

func pageLimit(p Page) int {
	switch {
	case p.Limit <= 0:
		return defaultPageLimit
	case p.Limit > maxPageLimit:
		return maxPageLimit
	default:
		return p.Limit
	}
}

func encodeCursor(at time.Time, id string) string {
	raw := strconv.FormatInt(at.UnixNano(), 10) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (*cursorKey, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("chat: invalid page cursor: %w", err)
	}
	nanos, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil, fmt.Errorf("chat: invalid page cursor")
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("chat: invalid page cursor: %w", err)
	}
	return &cursorKey{at: time.Unix(0, n).UTC(), id: id}, nil
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("chat: marshal literal payload: " + err.Error())
	}
	return raw
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func cloneConversation(c Conversation) Conversation { return c }

func cloneUpload(u Upload) Upload { return u }

func cloneFailure(f *TurnFailed) *TurnFailed {
	if f == nil {
		return nil
	}
	cp := *f
	return &cp
}

func cloneTurn(t Turn) Turn {
	t.Error = cloneFailure(t.Error)
	return t
}

func cloneProposal(p Proposal) Proposal {
	p.Arguments = cloneRaw(p.Arguments)
	p.Result = cloneRaw(p.Result)
	p.Hunks = cloneHunks(p.Hunks)
	if p.DecidedAt != nil {
		at := *p.DecidedAt
		p.DecidedAt = &at
	}
	return p
}

func cloneHunks(hunks []HunkOutcome) []HunkOutcome {
	if hunks == nil {
		return nil
	}
	out := make([]HunkOutcome, len(hunks))
	copy(out, hunks)
	return out
}

func clonePart(p Part) Part {
	p.Arguments = cloneRaw(p.Arguments)
	p.Result = cloneRaw(p.Result)
	return p
}

func cloneMessage(m Message) Message {
	if m.Parts != nil {
		parts := make([]Part, len(m.Parts))
		for i, p := range m.Parts {
			parts[i] = clonePart(p)
		}
		m.Parts = parts
	}
	return m
}

func cloneEvent(e Event) Event {
	e.Data = cloneRaw(e.Data)
	return e
}

// LatestSummary implements SummaryStore.
func (s *MemStore) LatestSummary(_ context.Context, owner OwnerKey, conversationID string) (Summary, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, conversationID)
	if err != nil {
		return Summary{}, false, err
	}
	if rec.summary == nil {
		return Summary{}, false, nil
	}
	return *rec.summary, true, nil
}

// PutSummary implements SummaryStore.
func (s *MemStore) PutSummary(_ context.Context, owner OwnerKey, sum Summary, prevThrough string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.ownedConversationLocked(owner, sum.ConversationID)
	if err != nil {
		return false, err
	}
	current := ""
	if rec.summary != nil {
		current = rec.summary.ThroughMessageID
	}
	if current != prevThrough {
		return false, nil
	}
	stored := sum
	rec.summary = &stored
	return true, nil
}
