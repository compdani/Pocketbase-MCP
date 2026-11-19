package pocketbase

// ChatStore is the durable chat.Store, chat.ProposalStore and chat.UploadStore
// for stock PocketBase.
//
// It persists conversations, messages, turns, a bounded per-turn event log,
// reviewed proposals and chat attachments in six private base collections
// provisioned at startup.
// "Private" means nil list/view/create/update/delete rules, which PocketBase
// reserves for superusers, and the collections are never added to any MCP
// manifest, so a normal caller cannot read another caller's chat data through
// the records REST API or through MCP.
//
// Ownership is enforced by every query, never by record ID alone. Each record
// carries the full owner partition as separate fields plus an ownerKey text
// field (chat.OwnerKey.String()); a lookup filters on ownerKey AND the record
// id (or the owning conversation id), so a record owned by somebody else is
// indistinguishable from a missing one.
//
// The concurrency-sensitive invariants are enforced in the database schema,
// not only in Go:
//
//   - a turn carries activeSlot = conversationID while it is non-terminal and
//     "" once terminal, with a UNIQUE partial index on (activeSlot) WHERE
//     activeSlot != '' — so a conversation can never have two active turns;
//   - (ownerKey, conversation, idempotencyKey) is UNIQUE WHERE idempotencyKey
//     != '' — so a lost submission response cannot create a second turn;
//   - (turn, seq) is UNIQUE — so per-turn sequences cannot fork.
//
// Every mutating operation that touches more than one row runs inside
// app.RunInTransaction, which also gives the compare-and-swap transitions and
// the "state and its terminal event are written together" rule a single
// critical section. Retained event windows are trimmed in the same
// transaction that appends the event.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/dbutils"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/compdani/pocketbase-mcp/chat"
)

// DefaultChatCollectionPrefix is the collection-name prefix used when
// ChatStoreOptions.Prefix is empty.
const DefaultChatCollectionPrefix = "_mcpChat"

// Chat store field names. They document the provisioned schema; the object
// schema itself is not part of the chat contract.
const (
	chatFieldOwnerKey       = "ownerKey"
	chatFieldNamespace      = "namespace"
	chatFieldMount          = "mount"
	chatFieldAuthCollection = "authCollectionId"
	chatFieldUserID         = "userId"
	chatFieldTenant         = "tenant"
	chatFieldTitle          = "title"
	chatFieldTarget         = "target"
	chatFieldCreatedAt      = "createdAt"
	chatFieldUpdatedAt      = "updatedAt"
	chatFieldConversation   = "conversation"
	chatFieldTurn           = "turn"
	chatFieldRole           = "role"
	chatFieldParts          = "parts"
	chatFieldState          = "state"
	chatFieldIdempotencyKey = "idempotencyKey"
	chatFieldActiveSlot     = "activeSlot"
	chatFieldProvider       = "provider"
	chatFieldModel          = "model"
	chatFieldEffort         = "effort"
	chatFieldLastSeq        = "lastSeq"
	chatFieldDroppedBefore  = "droppedBefore"
	chatFieldError          = "error"
	chatFieldSeq            = "seq"
	chatFieldType           = "type"
	chatFieldVersion        = "version"
	chatFieldData           = "data"
	chatFieldAt             = "at"

	// Proposal fields. They keep the chat.Proposal names, except that the
	// conversation and turn bindings use their own column names so a proposal
	// row never collides with the turn columns of the other collections.
	chatFieldProposalConversation = "conversationId"
	chatFieldProposalTurn         = "turnId"
	chatFieldCallID               = "callId"
	chatFieldTool                 = "tool"
	chatFieldToolFingerprint      = "toolFingerprint"
	chatFieldManifestRevision     = "manifestRevision"
	chatFieldLocation             = "location"
	chatFieldArguments            = "arguments"
	chatFieldArgumentsHash        = "argumentsHash"
	chatFieldSummary              = "summary"
	chatFieldRevision             = "revision"
	chatFieldExpiresAt            = "expiresAt"
	chatFieldDecidedAt            = "decidedAt"
	chatFieldResult               = "result"
	chatFieldFailure              = "failure"
	chatFieldEditorRevision       = "editorRevision"
	chatFieldHunks                = "hunks"

	// Upload fields. The conversation binding uses its own column name so an
	// upload row never collides with the turn columns of the other
	// collections, and the bytes live in a protected file field so PocketBase
	// stores them in the app's configured filesystem (local or S3).
	chatFieldUploadConversation = "conversationId"
	chatFieldUploadName         = "name"
	chatFieldUploadMIME         = "mime"
	chatFieldUploadKind         = "kind"
	chatFieldUploadSize         = "size"
	chatFieldUploadSHA256       = "sha256"
	chatFieldUploadData         = "data"

	chatDefaultPageLimit     = 50
	chatMaxPageLimit         = 200
	chatCursorSeparator      = "|"
	chatMaxTxAttempts        = 5
	chatInterruptedMessage   = "turn interrupted by server restart"
	chatCollectionNameMaxLen = 100
)

// ChatStoreOptions configures NewChatStore.
type ChatStoreOptions struct {
	// Prefix is prepended to every provisioned collection name. Empty means
	// DefaultChatCollectionPrefix. It must be a safe SQL/collection identifier
	// ([A-Za-z_][A-Za-z0-9_]*) so collection names never need quoting tricks.
	Prefix string
	// MaxEventsPerTurn bounds the retained replay log of one turn. Older
	// events are dropped first, which makes an old cursor report
	// chat.ErrResyncRequired. Zero means chat.DefaultMaxEventsPerTurn.
	MaxEventsPerTurn int
}

// ChatStore is a chat.Store over a PocketBase app. It is safe for concurrent
// use: all multi-row invariants are database constraints, and every
// multi-statement write runs in one serialized app transaction.
type ChatStore struct {
	app    core.App
	prefix string

	conversations string
	messages      string
	turns         string
	events        string
	proposals     string
	uploads       string

	maxEvents int
	// lastUploadMS makes consecutive upload timestamps strictly increasing.
	// PocketBase dates keep millisecond precision, so two uploads stored within
	// one millisecond would otherwise tie and fall back to an arbitrary id
	// order instead of creation order.
	lastUploadMS atomic.Int64
}

var _ chat.Store = (*ChatStore)(nil)
var _ chat.ProposalStore = (*ChatStore)(nil)
var _ chat.UploadStore = (*ChatStore)(nil)
var _ chat.ActiveTurnStore = (*ChatStore)(nil)
var _ chat.RecentMessageStore = (*ChatStore)(nil)

var chatCollectionPrefixPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NewChatStore provisions (idempotently) the six private chat collections and
// returns a store over them. An existing collection with one of the expected
// names is only adopted when it is a private base collection with the exact
// expected field types and indexes; anything else fails with an error naming
// the collection, so startup never silently takes over or alters an
// application's own collection.
//
// Provisioning runs in one transaction: either all six collections exist
// afterwards or none were changed. An installation from a release with only the
// four v1.1 collections simply gains the proposal and upload collections on its
// next start, and a v1.2 install with proposals gains uploads the same way.
func NewChatStore(app core.App, opts ChatStoreOptions) (*ChatStore, error) {
	if app == nil {
		return nil, errors.New("chat store: nil app")
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultChatCollectionPrefix
	}
	if !chatCollectionPrefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("chat store: collection prefix %q must match %s", prefix, chatCollectionPrefixPattern)
	}
	maxEvents := opts.MaxEventsPerTurn
	if maxEvents <= 0 {
		maxEvents = chat.DefaultMaxEventsPerTurn
	}
	s := &ChatStore{
		app:           app,
		prefix:        prefix,
		conversations: prefix + "Conversations",
		messages:      prefix + "Messages",
		turns:         prefix + "Turns",
		events:        prefix + "Events",
		proposals:     prefix + "Proposals",
		uploads:       prefix + "Uploads",
		maxEvents:     maxEvents,
	}
	for _, name := range s.Collections() {
		if len(name) > chatCollectionNameMaxLen {
			return nil, fmt.Errorf("chat store: collection name %q exceeds %d characters; use a shorter prefix", name, chatCollectionNameMaxLen)
		}
	}
	if err := app.RunInTransaction(func(tx core.App) error {
		for _, spec := range s.specs() {
			if _, err := provisionChatCollection(tx, spec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// Collections returns the provisioned collection names in a stable order
// (conversations, messages, turns, events, proposals, uploads).
func (s *ChatStore) Collections() []string {
	return []string{s.conversations, s.messages, s.turns, s.events, s.proposals, s.uploads}
}

// --- provisioning ---

type chatIndexSpec struct {
	name    string
	unique  bool
	columns string
	where   string
}

type chatCollectionSpec struct {
	name    string
	fields  func() []core.Field
	indexes []chatIndexSpec
	// addFields names fields that an older, otherwise compatible collection
	// may be missing. Provisioning adds them in place. Every other field must
	// already exist, so a deliberately broken collection still refuses startup.
	addFields []string
}

func (s chatCollectionSpec) allowsAdd(name string) bool {
	for _, allowed := range s.addFields {
		if allowed == name {
			return true
		}
	}
	return false
}

func chatOwnerTextFields() []core.Field {
	return []core.Field{
		&core.TextField{Name: chatFieldOwnerKey},
		&core.TextField{Name: chatFieldNamespace},
		&core.TextField{Name: chatFieldMount},
		&core.TextField{Name: chatFieldAuthCollection},
		&core.TextField{Name: chatFieldUserID},
		&core.TextField{Name: chatFieldTenant},
	}
}

func (s *ChatStore) specs() []chatCollectionSpec {
	return []chatCollectionSpec{
		{
			name: s.conversations,
			fields: func() []core.Field {
				fields := chatOwnerTextFields()
				return append(fields,
					&core.TextField{Name: chatFieldTitle},
					&core.TextField{Name: chatFieldTarget},
					&core.DateField{Name: chatFieldCreatedAt},
					&core.DateField{Name: chatFieldUpdatedAt},
				)
			},
			indexes: []chatIndexSpec{
				{name: "idx_" + s.conversations + "_owner_created", columns: chatFieldOwnerKey + ", " + chatFieldCreatedAt + ", id"},
			},
		},
		{
			name: s.messages,
			fields: func() []core.Field {
				return []core.Field{
					&core.TextField{Name: chatFieldOwnerKey},
					&core.TextField{Name: chatFieldConversation},
					&core.TextField{Name: chatFieldTurn},
					&core.TextField{Name: chatFieldRole},
					&core.JSONField{Name: chatFieldParts, MaxSize: 4 << 20},
					&core.DateField{Name: chatFieldCreatedAt},
				}
			},
			indexes: []chatIndexSpec{
				{name: "idx_" + s.messages + "_owner_conversation", columns: chatFieldOwnerKey + ", " + chatFieldConversation + ", " + chatFieldCreatedAt + ", id"},
			},
		},
		{
			name: s.turns,
			fields: func() []core.Field {
				fields := chatOwnerTextFields()
				return append(fields,
					&core.TextField{Name: chatFieldConversation},
					&core.TextField{Name: chatFieldState},
					&core.TextField{Name: chatFieldIdempotencyKey},
					&core.TextField{Name: chatFieldActiveSlot},
					&core.TextField{Name: chatFieldProvider},
					&core.TextField{Name: chatFieldModel},
					&core.TextField{Name: chatFieldEffort},
					&core.NumberField{Name: chatFieldLastSeq, OnlyInt: true},
					&core.NumberField{Name: chatFieldDroppedBefore, OnlyInt: true},
					&core.JSONField{Name: chatFieldError, MaxSize: 64 << 10},
					&core.DateField{Name: chatFieldCreatedAt},
					&core.DateField{Name: chatFieldUpdatedAt},
				)
			},
			// v1.3 adds the provider to a turn. Older stores gain it in place.
			addFields: []string{chatFieldProvider, chatFieldEffort},
			indexes: []chatIndexSpec{
				// One active turn per conversation, enforced even if two
				// processes race: the second insert violates the index.
				{name: "idx_" + s.turns + "_active_slot", unique: true, columns: chatFieldActiveSlot, where: chatFieldActiveSlot + " != ''"},
				// One turn per idempotency key within a conversation; a lost
				// response can therefore never create a second turn.
				{name: "idx_" + s.turns + "_idempotency", unique: true, columns: chatFieldOwnerKey + ", " + chatFieldConversation + ", " + chatFieldIdempotencyKey, where: chatFieldIdempotencyKey + " != ''"},
				{name: "idx_" + s.turns + "_owner_conversation", columns: chatFieldOwnerKey + ", " + chatFieldConversation},
			},
		},
		{
			name: s.events,
			fields: func() []core.Field {
				return []core.Field{
					&core.TextField{Name: chatFieldOwnerKey},
					&core.TextField{Name: chatFieldConversation},
					&core.TextField{Name: chatFieldTurn},
					&core.NumberField{Name: chatFieldSeq, OnlyInt: true},
					&core.TextField{Name: chatFieldType},
					&core.TextField{Name: chatFieldVersion},
					&core.JSONField{Name: chatFieldData, MaxSize: 4 << 20},
					&core.DateField{Name: chatFieldAt},
				}
			},
			indexes: []chatIndexSpec{
				// Sequences are per turn and may never fork.
				{name: "idx_" + s.events + "_turn_seq", unique: true, columns: chatFieldTurn + ", " + chatFieldSeq},
				{name: "idx_" + s.events + "_owner_turn", columns: chatFieldOwnerKey + ", " + chatFieldTurn + ", " + chatFieldSeq},
			},
		},
		{
			name: s.proposals,
			fields: func() []core.Field {
				fields := chatOwnerTextFields()
				return append(fields,
					&core.TextField{Name: chatFieldProposalConversation},
					&core.TextField{Name: chatFieldProposalTurn},
					&core.TextField{Name: chatFieldCallID},
					&core.TextField{Name: chatFieldTool},
					&core.TextField{Name: chatFieldToolFingerprint},
					&core.TextField{Name: chatFieldManifestRevision},
					&core.TextField{Name: chatFieldLocation},
					&core.TextField{Name: chatFieldTarget},
					&core.JSONField{Name: chatFieldArguments, MaxSize: 4 << 20},
					&core.TextField{Name: chatFieldArgumentsHash},
					&core.TextField{Name: chatFieldSummary},
					&core.TextField{Name: chatFieldState},
					&core.NumberField{Name: chatFieldRevision, OnlyInt: true},
					&core.DateField{Name: chatFieldExpiresAt},
					&core.DateField{Name: chatFieldDecidedAt},
					&core.JSONField{Name: chatFieldResult, MaxSize: 4 << 20},
					&core.TextField{Name: chatFieldFailure},
					&core.TextField{Name: chatFieldEditorRevision},
					&core.JSONField{Name: chatFieldHunks, MaxSize: 256 << 10},
					&core.DateField{Name: chatFieldCreatedAt},
					&core.DateField{Name: chatFieldUpdatedAt},
				)
			},
			indexes: []chatIndexSpec{
				// The per-conversation list order, and the delete cascade's
				// scan, are both served by this index.
				{name: "idx_" + s.proposals + "_owner_conversation", columns: chatFieldOwnerKey + ", " + chatFieldProposalConversation + ", " + chatFieldCreatedAt + ", id"},
				// The expiry sweep and restart recovery scan by state; the
				// second index covers the deadline filter.
				{name: "idx_" + s.proposals + "_state", columns: chatFieldState},
				{name: "idx_" + s.proposals + "_state_expires", columns: chatFieldState + ", " + chatFieldExpiresAt},
			},
		},
		{
			name: s.uploads,
			fields: func() []core.Field {
				fields := chatOwnerTextFields()
				return append(fields,
					&core.TextField{Name: chatFieldUploadConversation},
					&core.TextField{Name: chatFieldUploadName},
					&core.TextField{Name: chatFieldUploadMIME},
					&core.TextField{Name: chatFieldUploadKind},
					&core.NumberField{Name: chatFieldUploadSize, OnlyInt: true},
					&core.TextField{Name: chatFieldUploadSHA256},
					// The bytes are a protected PocketBase file: the store
					// never forms a public URL or a file token for them. The
					// bound is a safety net only; the HTTP layer enforces the
					// real per-request limit.
					&core.FileField{Name: chatFieldUploadData, MaxSelect: 1, MaxSize: chatMaxUploadFileBytes, Protected: true},
					&core.DateField{Name: chatFieldCreatedAt},
					&core.DateField{Name: chatFieldUpdatedAt},
				)
			},
			indexes: []chatIndexSpec{
				// The newest-first list, the per-conversation count and the
				// delete cascade all scan by (owner, conversation).
				{name: "idx_" + s.uploads + "_owner_conversation", columns: chatFieldOwnerKey + ", " + chatFieldUploadConversation + ", " + chatFieldCreatedAt},
			},
		},
	}
}

// provisionChatCollection creates a collection or upgrades an existing one.
// A compatible existing collection gains any missing fields the current spec
// owns (the v1.2 -> v1.3 turn provider is the first), while type or index
// conflicts still refuse startup.
func provisionChatCollection(app core.App, spec chatCollectionSpec) (bool, error) {
	existing, err := app.FindCollectionByNameOrId(spec.name)
	if err == nil && existing != nil {
		if err := checkChatCollectionCompatible(spec, existing); err != nil {
			return false, err
		}
		added := false
		for _, want := range spec.fields() {
			if existing.Fields.GetByName(want.GetName()) == nil && spec.allowsAdd(want.GetName()) {
				existing.Fields.Add(want)
				added = true
			}
		}
		if added {
			if err := app.Save(existing); err != nil {
				return false, fmt.Errorf("chat store: upgrade collection %q: %w", spec.name, err)
			}
		}
		return added, nil
	}
	col := core.NewBaseCollection(spec.name)
	// Nil API rules are PocketBase's "superusers only" state; they are also
	// what keeps the store out of every normal API and MCP listing.
	for _, f := range spec.fields() {
		col.Fields.Add(f)
	}
	for _, idx := range spec.indexes {
		col.AddIndex(idx.name, idx.unique, idx.columns, idx.where)
	}
	if err := app.Save(col); err != nil {
		return false, fmt.Errorf("chat store: provision collection %q: %w", spec.name, err)
	}
	return true, nil
}

// checkChatCollectionCompatible refuses to adopt a collection that is not
// exactly the private base schema this store owns.
func checkChatCollectionCompatible(spec chatCollectionSpec, existing *core.Collection) error {
	conflict := func(format string, args ...any) error {
		return fmt.Errorf("chat store: conflicting existing schema for collection %q: %s (remove it or move it under a different name before enabling chat)",
			spec.name, fmt.Sprintf(format, args...))
	}
	if !existing.IsBase() {
		return conflict("expected a base collection, got %q", existing.Type)
	}
	if existing.ListRule != nil || existing.ViewRule != nil || existing.CreateRule != nil || existing.UpdateRule != nil || existing.DeleteRule != nil {
		return conflict("API rules must all be nil so the collection is superuser-only")
	}
	for _, want := range spec.fields() {
		got := existing.Fields.GetByName(want.GetName())
		if got == nil {
			// Only explicitly additive fields are upgraded in place; every
			// other missing field is still a conflicting collection.
			if spec.allowsAdd(want.GetName()) {
				continue
			}
			return conflict("missing field %q (%s)", want.GetName(), want.Type())
		}
		if got.Type() != want.Type() {
			return conflict("field %q has type %q, expected %q", want.GetName(), got.Type(), want.Type())
		}
	}
	for _, want := range spec.indexes {
		raw := existing.GetIndex(want.name)
		if raw == "" {
			return conflict("missing index %q (%s)", want.name, want.columns)
		}
		got := dbutils.ParseIndex(raw)
		if got.Unique != want.unique {
			return conflict("index %q unique = %v, expected %v", want.name, got.Unique, want.unique)
		}
		if !chatSameColumns(got.Columns, want.columns) {
			return conflict("index %q has columns %v, expected %s", want.name, chatIndexColumns(got), want.columns)
		}
		if want.where != "" && strings.TrimSpace(got.Where) == "" {
			return conflict("index %q must be a partial index with WHERE %s", want.name, want.where)
		}
	}
	return nil
}

func chatSameColumns(got []dbutils.IndexColumn, want string) bool {
	wantCols := strings.Split(want, ",")
	if len(got) != len(wantCols) {
		return false
	}
	for i, c := range wantCols {
		if !strings.EqualFold(strings.TrimSpace(got[i].Name), strings.TrimSpace(c)) {
			return false
		}
	}
	return true
}

func chatIndexColumns(idx dbutils.Index) []string {
	out := make([]string, 0, len(idx.Columns))
	for _, c := range idx.Columns {
		out = append(out, c.Name)
	}
	return out
}

// --- chat.Store implementation ---

func chatNow() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

// chatTime normalizes a caller- or store-supplied timestamp to the precision
// the date fields store, so a value read back from the database is exactly the
// value the API returned. Zero stays zero for the caller to default.
func chatTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Millisecond)
}

func chatDateParam(t time.Time) string { return t.UTC().Format(types.DefaultDateLayout) }

func chatOwnerParams(owner chat.OwnerKey) dbx.Params {
	return dbx.Params{chatFieldOwnerKey: owner.String()}
}

func chatSetOwner(rec *core.Record, owner chat.OwnerKey) {
	rec.Set(chatFieldOwnerKey, owner.String())
	rec.Set(chatFieldNamespace, owner.Namespace)
	rec.Set(chatFieldMount, owner.Mount)
	rec.Set(chatFieldAuthCollection, owner.AuthCollectionID)
	rec.Set(chatFieldUserID, owner.UserID)
	rec.Set(chatFieldTenant, owner.Tenant)
}

func chatOwnerFrom(rec *core.Record) chat.OwnerKey {
	return chat.OwnerKey{
		Namespace:        rec.GetString(chatFieldNamespace),
		Mount:            rec.GetString(chatFieldMount),
		AuthCollectionID: rec.GetString(chatFieldAuthCollection),
		UserID:           rec.GetString(chatFieldUserID),
		Tenant:           rec.GetString(chatFieldTenant),
	}
}

// chatRawJSON reads a json field as raw JSON. PocketBase hands json fields
// back as types.JSONRaw; the other cases keep the helper safe for records that
// were built in memory rather than scanned.
func chatRawJSON(rec *core.Record, name string) json.RawMessage {
	switch v := rec.GetRaw(name).(type) {
	case nil:
		return nil
	case types.JSONRaw:
		if len(v) == 0 {
			return nil
		}
		return append(json.RawMessage(nil), v...)
	case []byte:
		if len(v) == 0 {
			return nil
		}
		return append(json.RawMessage(nil), v...)
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return append(json.RawMessage(nil), v...)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return raw
	}
}

func chatSetJSON(rec *core.Record, name string, raw json.RawMessage) {
	if len(raw) == 0 {
		rec.Set(name, nil)
		return
	}
	rec.Set(name, types.JSONRaw(raw))
}

func chatConversationFrom(rec *core.Record) chat.Conversation {
	return chat.Conversation{
		ID:        rec.Id,
		Owner:     chatOwnerFrom(rec),
		Title:     rec.GetString(chatFieldTitle),
		Target:    rec.GetString(chatFieldTarget),
		CreatedAt: rec.GetDateTime(chatFieldCreatedAt).Time(),
		UpdatedAt: rec.GetDateTime(chatFieldUpdatedAt).Time(),
	}
}

func chatMessageFrom(rec *core.Record) chat.Message {
	m := chat.Message{
		ID:             rec.Id,
		ConversationID: rec.GetString(chatFieldConversation),
		TurnID:         rec.GetString(chatFieldTurn),
		Role:           rec.GetString(chatFieldRole),
		CreatedAt:      rec.GetDateTime(chatFieldCreatedAt).Time(),
	}
	if raw := chatRawJSON(rec, chatFieldParts); len(raw) > 0 {
		var parts []chat.Part
		if err := json.Unmarshal(raw, &parts); err == nil {
			m.Parts = parts
		}
	}
	return m
}

func chatTurnFrom(rec *core.Record) chat.Turn {
	t := chat.Turn{
		ID:             rec.Id,
		ConversationID: rec.GetString(chatFieldConversation),
		Owner:          chatOwnerFrom(rec),
		State:          chat.TurnState(rec.GetString(chatFieldState)),
		IdempotencyKey: rec.GetString(chatFieldIdempotencyKey),
		Provider:       rec.GetString(chatFieldProvider),
		Model:          rec.GetString(chatFieldModel),
		Effort:         rec.GetString(chatFieldEffort),
		CreatedAt:      rec.GetDateTime(chatFieldCreatedAt).Time(),
		UpdatedAt:      rec.GetDateTime(chatFieldUpdatedAt).Time(),
	}
	if raw := chatRawJSON(rec, chatFieldError); len(raw) > 0 && string(raw) != "null" {
		var failure chat.TurnFailed
		if err := json.Unmarshal(raw, &failure); err == nil {
			t.Error = &failure
		}
	}
	return t
}

func chatEventFrom(rec *core.Record) chat.Event {
	return chat.Event{
		Version:        rec.GetString(chatFieldVersion),
		Seq:            rec.GetInt64(chatFieldSeq),
		ConversationID: rec.GetString(chatFieldConversation),
		TurnID:         rec.GetString(chatFieldTurn),
		Type:           chat.EventType(rec.GetString(chatFieldType)),
		Data:           chatRawJSON(rec, chatFieldData),
		At:             rec.GetDateTime(chatFieldAt).Time(),
	}
}

// chatFindOwned resolves a record only when it belongs to owner. A record that
// exists under another owner is reported exactly like a missing one.
func chatFindOwned(app core.App, collection string, owner chat.OwnerKey, id string) (*core.Record, error) {
	params := chatOwnerParams(owner)
	params["id"] = id
	rec, err := app.FindFirstRecordByFilter(collection, "id = {:id} && "+chatFieldOwnerKey+" = {:ownerKey}", params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, chat.ErrNotFound
		}
		return nil, err
	}
	return rec, nil
}

// chatFirst returns ErrNotFound when the filter matches nothing.
func chatFirst(app core.App, collection, filter string, params dbx.Params) (*core.Record, error) {
	rec, err := app.FindFirstRecordByFilter(collection, filter, params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, chat.ErrNotFound
		}
		return nil, err
	}
	return rec, nil
}

func chatCollection(app core.App, name string) (*core.Collection, error) {
	col, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		return nil, fmt.Errorf("chat store: collection %q is unavailable: %w", name, err)
	}
	if col == nil {
		return nil, fmt.Errorf("chat store: collection %q is unavailable", name)
	}
	return col, nil
}

// chatUniqueViolation reports a database uniqueness failure. PocketBase wraps
// driver errors, so the stable signal is the message.
//
// Only a uniqueness violation may be retried or mapped to chat.ErrConflict:
// SQLite (the stock adapter's dialect) phrases NOT NULL and CHECK failures with
// the same "constraint failed" prefix, so matching that generic wording would
// misreport a validation failure as a lost race. The not-null, foreign-key and
// check wordings are therefore excluded first, and the SQLite extended result
// codes for the two uniqueness constraints (1555 primary key, 2067 unique) are
// accepted too.
func chatUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, notUnique := range []string{
		"not null constraint failed",
		"not-null constraint",
		"foreign key constraint failed",
		"check constraint failed",
	} {
		if strings.Contains(msg, notUnique) {
			return false
		}
	}
	for _, unique := range []string{
		"unique constraint failed", // SQLite wording
		"(1555)",                   // SQLITE_CONSTRAINT_PRIMARYKEY
		"(2067)",                   // SQLITE_CONSTRAINT_UNIQUE
		"unique constraint",        // generic driver wording
		"value must be unique",     // PocketBase's normalized index error
	} {
		if strings.Contains(msg, unique) {
			return true
		}
	}
	return false
}

// chatRunTx runs fn in one app transaction, retrying a small number of times
// when a unique constraint (a stolen sequence number or a competing terminal
// event) aborted it. PocketBase serializes app transactions on a single
// connection, so a retry is only needed when another writer committed first.
func (s *ChatStore) chatRunTx(fn func(core.App) error) error {
	var err error
	for attempt := 0; attempt < chatMaxTxAttempts; attempt++ {
		err = s.app.RunInTransaction(fn)
		if err == nil || !chatUniqueViolation(err) {
			return err
		}
	}
	return err
}

func (s *ChatStore) CreateConversation(ctx context.Context, c chat.Conversation) (chat.Conversation, error) {
	if err := c.Owner.Validate(); err != nil {
		return chat.Conversation{}, err
	}
	col, err := chatCollection(s.app, s.conversations)
	if err != nil {
		return chat.Conversation{}, err
	}
	rec := core.NewRecord(col)
	if c.ID != "" {
		rec.Set(core.FieldNameId, c.ID)
	}
	chatSetOwner(rec, c.Owner)
	rec.Set(chatFieldTitle, c.Title)
	rec.Set(chatFieldTarget, c.Target)
	created := chatTime(c.CreatedAt)
	if created.IsZero() {
		created = chatNow()
	}
	updated := chatTime(c.UpdatedAt)
	if updated.IsZero() {
		updated = created
	}
	rec.Set(chatFieldCreatedAt, created)
	rec.Set(chatFieldUpdatedAt, updated)
	if err := s.app.SaveWithContext(ctx, rec); err != nil {
		if chatUniqueViolation(err) {
			return chat.Conversation{}, chat.ErrConflict
		}
		return chat.Conversation{}, err
	}
	return chatConversationFrom(rec), nil
}

func (s *ChatStore) GetConversation(_ context.Context, owner chat.OwnerKey, id string) (chat.Conversation, error) {
	rec, err := chatFindOwned(s.app, s.conversations, owner, id)
	if err != nil {
		return chat.Conversation{}, err
	}
	return chatConversationFrom(rec), nil
}

func (s *ChatStore) ListConversations(_ context.Context, owner chat.OwnerKey, target string, page chat.Page) ([]chat.Conversation, string, error) {
	limit := chatPageLimit(page)
	after, err := chatDecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter := chatFieldOwnerKey + " = {:ownerKey}"
	params := chatOwnerParams(owner)
	if target != "" {
		filter += " && " + chatFieldTarget + " = {:target}"
		params[chatFieldTarget] = target
	}
	if after != nil {
		filter += " && (" + chatFieldCreatedAt + " < {:at} || (" + chatFieldCreatedAt + " = {:at} && id < {:id}))"
		params["at"] = chatDateParam(after.at)
		params["id"] = after.id
	}
	recs, err := s.app.FindRecordsByFilter(s.conversations, filter, "-"+chatFieldCreatedAt+",-id", limit+1, 0, params)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(recs) > limit {
		recs = recs[:limit]
		last := recs[limit-1]
		next = chatEncodeCursor(last.GetDateTime(chatFieldCreatedAt).Time(), last.Id)
	}
	out := make([]chat.Conversation, 0, len(recs))
	for _, rec := range recs {
		out = append(out, chatConversationFrom(rec))
	}
	return out, next, nil
}

func (s *ChatStore) RenameConversation(ctx context.Context, owner chat.OwnerKey, id, title string) (chat.Conversation, error) {
	var out chat.Conversation
	err := s.app.RunInTransaction(func(tx core.App) error {
		rec, err := chatFindOwned(tx, s.conversations, owner, id)
		if err != nil {
			return err
		}
		rec.Set(chatFieldTitle, title)
		rec.Set(chatFieldUpdatedAt, chatNow())
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = chatConversationFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Conversation{}, err
	}
	return out, nil
}

func (s *ChatStore) DeleteConversation(ctx context.Context, owner chat.OwnerKey, id string) error {
	return s.chatRunTx(func(tx core.App) error {
		rec, err := chatFindOwned(tx, s.conversations, owner, id)
		if err != nil {
			return err
		}
		params := chatOwnerParams(owner)
		params[chatFieldConversation] = id
		if _, err := chatFirst(tx, s.turns,
			chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldActiveSlot+" != ''",
			params); err == nil {
			return chat.ErrActiveTurn
		} else if !errors.Is(err, chat.ErrNotFound) {
			return err
		}
		// An executing proposal may still produce a server effect, so its
		// conversation cannot disappear underneath it. An active turn wins the
		// refusal above because it is the reason the conversation is busy.
		proposalParams := chatOwnerParams(owner)
		proposalParams[chatFieldProposalConversation] = id
		proposalParams[chatFieldState] = string(chat.ProposalStateExecuting)
		if _, err := chatFirst(tx, s.proposals,
			chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldProposalConversation+" = {:conversationId} && "+chatFieldState+" = {:state}",
			proposalParams); err == nil {
			return chat.ErrConflict
		} else if !errors.Is(err, chat.ErrNotFound) {
			return err
		}
		for _, rows := range []struct{ collection, conversation string }{
			{s.proposals, chatFieldProposalConversation},
			{s.events, chatFieldConversation},
			{s.messages, chatFieldConversation},
			{s.turns, chatFieldConversation},
		} {
			if err := chatDeleteConversationRows(tx, rows.collection, rows.conversation, owner, id); err != nil {
				return err
			}
		}
		// Uploads are removed one record at a time, not with a bulk DELETE:
		// PocketBase deletes a record's files as part of the record delete, so
		// a bulk statement would remove the rows and leave the bytes behind.
		// Only this conversation's upload records are touched, so no other
		// collection's files can be affected.
		if err := chatDeleteConversationUploads(ctx, tx, s.uploads, owner, id); err != nil {
			return err
		}
		return tx.DeleteWithContext(ctx, rec)
	})
}

// chatDeleteConversationRows removes every row of one collection belonging to
// one conversation of one owner. The table name and the conversation column
// come from the provisioned schema (never from user input); the values are
// bound parameters.
func chatDeleteConversationRows(app core.App, collection, conversationColumn string, owner chat.OwnerKey, conversationID string) error {
	_, err := app.NonconcurrentDB().
		NewQuery("DELETE FROM {{" + collection + "}} WHERE [[ownerKey]] = {:ownerKey} AND [[" + conversationColumn + "]] = {:conversation}").
		Bind(dbx.Params{chatFieldOwnerKey: owner.String(), chatFieldConversation: conversationID}).
		Execute()
	if err != nil {
		return fmt.Errorf("chat store: delete %s rows: %w", collection, err)
	}
	return nil
}

func (s *ChatStore) AppendMessage(ctx context.Context, owner chat.OwnerKey, m chat.Message) (chat.Message, error) {
	var out chat.Message
	err := s.app.RunInTransaction(func(tx core.App) error {
		if _, err := chatFindOwned(tx, s.conversations, owner, m.ConversationID); err != nil {
			return err
		}
		if m.TurnID != "" {
			turn, err := chatFindOwned(tx, s.turns, owner, m.TurnID)
			if err != nil {
				return err
			}
			if turn.GetString(chatFieldConversation) != m.ConversationID {
				return chat.ErrNotFound
			}
		}
		col, err := chatCollection(tx, s.messages)
		if err != nil {
			return err
		}
		rec := core.NewRecord(col)
		if m.ID != "" {
			rec.Set(core.FieldNameId, m.ID)
		}
		chatSetOwner(rec, owner)
		rec.Set(chatFieldConversation, m.ConversationID)
		rec.Set(chatFieldTurn, m.TurnID)
		rec.Set(chatFieldRole, m.Role)
		if len(m.Parts) > 0 {
			raw, err := json.Marshal(m.Parts)
			if err != nil {
				return fmt.Errorf("chat store: marshal message parts: %w", err)
			}
			chatSetJSON(rec, chatFieldParts, raw)
		} else {
			chatSetJSON(rec, chatFieldParts, nil)
		}
		created := chatTime(m.CreatedAt)
		if created.IsZero() {
			created = chatNow()
		}
		rec.Set(chatFieldCreatedAt, created)
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = chatMessageFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Message{}, err
	}
	return out, nil
}

// ActiveTurn implements chat.ActiveTurnStore through the indexed active slot,
// without reading the transcript.
func (s *ChatStore) ActiveTurn(_ context.Context, owner chat.OwnerKey, conversationID string) (chat.Turn, bool, error) {
	if _, err := chatFindOwned(s.app, s.conversations, owner, conversationID); err != nil {
		return chat.Turn{}, false, err
	}
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = conversationID
	rec, err := chatFirst(s.app, s.turns,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldActiveSlot+" != ''",
		params)
	if errors.Is(err, chat.ErrNotFound) {
		return chat.Turn{}, false, nil
	}
	if err != nil {
		return chat.Turn{}, false, err
	}
	return chatTurnFrom(rec), true, nil
}

// RecentMessages implements chat.RecentMessageStore: the newest limit messages,
// read newest first and returned in ascending order.
func (s *ChatStore) RecentMessages(_ context.Context, owner chat.OwnerKey, conversationID string, limit int) ([]chat.Message, error) {
	if _, err := chatFindOwned(s.app, s.conversations, owner, conversationID); err != nil {
		return nil, err
	}
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = conversationID
	recs, err := s.app.FindRecordsByFilter(s.messages,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation}",
		"-"+chatFieldCreatedAt+",-id", limit, 0, params)
	if err != nil {
		return nil, err
	}
	out := make([]chat.Message, len(recs))
	for i, rec := range recs {
		out[len(recs)-1-i] = chatMessageFrom(rec)
	}
	return out, nil
}

func (s *ChatStore) ListMessages(_ context.Context, owner chat.OwnerKey, conversationID string, page chat.Page) ([]chat.Message, string, error) {
	if _, err := chatFindOwned(s.app, s.conversations, owner, conversationID); err != nil {
		return nil, "", err
	}
	limit := chatPageLimit(page)
	after, err := chatDecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter := chatFieldOwnerKey + " = {:ownerKey} && " + chatFieldConversation + " = {:conversation}"
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = conversationID
	if after != nil {
		filter += " && (" + chatFieldCreatedAt + " > {:at} || (" + chatFieldCreatedAt + " = {:at} && id > {:id}))"
		params["at"] = chatDateParam(after.at)
		params["id"] = after.id
	}
	recs, err := s.app.FindRecordsByFilter(s.messages, filter, chatFieldCreatedAt+",id", limit+1, 0, params)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(recs) > limit {
		recs = recs[:limit]
		last := recs[limit-1]
		next = chatEncodeCursor(last.GetDateTime(chatFieldCreatedAt).Time(), last.Id)
	}
	out := make([]chat.Message, 0, len(recs))
	for _, rec := range recs {
		out = append(out, chatMessageFrom(rec))
	}
	return out, next, nil
}

func (s *ChatStore) CreateTurn(ctx context.Context, owner chat.OwnerKey, t chat.Turn) (chat.Turn, bool, error) {
	var (
		out     chat.Turn
		created bool
	)
	err := s.app.RunInTransaction(func(tx core.App) error {
		if _, err := chatFindOwned(tx, s.conversations, owner, t.ConversationID); err != nil {
			return err
		}
		params := chatOwnerParams(owner)
		params[chatFieldConversation] = t.ConversationID

		if t.IdempotencyKey != "" {
			idem := dbx.Params{}
			for k, v := range params {
				idem[k] = v
			}
			idem[chatFieldIdempotencyKey] = t.IdempotencyKey
			existing, err := chatFirst(tx, s.turns,
				chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldIdempotencyKey+" = {:idempotencyKey}",
				idem)
			switch {
			case err == nil:
				out, created = chatTurnFrom(existing), false
				return nil
			case errors.Is(err, chat.ErrNotFound):
			default:
				return err
			}
		}
		if _, err := chatFirst(tx, s.turns,
			chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldActiveSlot+" != ''",
			params); err == nil {
			return chat.ErrActiveTurn
		} else if !errors.Is(err, chat.ErrNotFound) {
			return err
		}

		col, err := chatCollection(tx, s.turns)
		if err != nil {
			return err
		}
		rec := core.NewRecord(col)
		if t.ID != "" {
			rec.Set(core.FieldNameId, t.ID)
		}
		chatSetOwner(rec, owner)
		rec.Set(chatFieldConversation, t.ConversationID)
		rec.Set(chatFieldState, string(chat.TurnStateQueued))
		rec.Set(chatFieldIdempotencyKey, t.IdempotencyKey)
		rec.Set(chatFieldActiveSlot, t.ConversationID)
		rec.Set(chatFieldProvider, t.Provider)
		rec.Set(chatFieldModel, t.Model)
		rec.Set(chatFieldEffort, t.Effort)
		rec.Set(chatFieldLastSeq, int64(0))
		rec.Set(chatFieldDroppedBefore, int64(0))
		chatSetJSON(rec, chatFieldError, nil)
		createdAt := chatTime(t.CreatedAt)
		if createdAt.IsZero() {
			createdAt = chatNow()
		}
		rec.Set(chatFieldCreatedAt, createdAt)
		rec.Set(chatFieldUpdatedAt, createdAt)
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			// A unique violation means another writer claimed the active
			// slot or the idempotency key first.
			if chatUniqueViolation(err) {
				return chatConflictForCreateTurn(tx, s, owner, t, &out, &created)
			}
			return err
		}
		out, created = chatTurnFrom(rec), true
		return nil
	})
	if err != nil {
		return chat.Turn{}, false, err
	}
	return out, created, nil
}

// chatConflictForCreateTurn resolves the loser of a CreateTurn race from the
// committed winner: the same idempotency key returns that turn (created false),
// otherwise the conversation is now busy.
func chatConflictForCreateTurn(tx core.App, s *ChatStore, owner chat.OwnerKey, t chat.Turn, out *chat.Turn, created *bool) error {
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = t.ConversationID
	if t.IdempotencyKey != "" {
		idem := dbx.Params{}
		for k, v := range params {
			idem[k] = v
		}
		idem[chatFieldIdempotencyKey] = t.IdempotencyKey
		if existing, err := chatFirst(tx, s.turns,
			chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldIdempotencyKey+" = {:idempotencyKey}",
			idem); err == nil {
			*out, *created = chatTurnFrom(existing), false
			return nil
		}
	}
	if _, err := chatFirst(tx, s.turns,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldActiveSlot+" != ''",
		params); err == nil {
		return chat.ErrActiveTurn
	}
	return chat.ErrConflict
}

func (s *ChatStore) GetTurn(_ context.Context, owner chat.OwnerKey, id string) (chat.Turn, error) {
	rec, err := chatFindOwned(s.app, s.turns, owner, id)
	if err != nil {
		return chat.Turn{}, err
	}
	return chatTurnFrom(rec), nil
}

// FindTurnByIdempotencyKey implements chat.IdempotentTurnStore.
func (s *ChatStore) FindTurnByIdempotencyKey(_ context.Context, owner chat.OwnerKey, conversationID, key string) (chat.Turn, bool, error) {
	if key == "" {
		return chat.Turn{}, false, nil
	}
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = conversationID
	params[chatFieldIdempotencyKey] = key
	rec, err := chatFirst(s.app, s.turns,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldIdempotencyKey+" = {:idempotencyKey}", params)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			return chat.Turn{}, false, nil
		}
		return chat.Turn{}, false, err
	}
	return chatTurnFrom(rec), true, nil
}

func (s *ChatStore) TransitionTurn(ctx context.Context, owner chat.OwnerKey, id string, from, to chat.TurnState, failure *chat.TurnFailed) (chat.Turn, error) {
	var out chat.Turn
	err := s.app.RunInTransaction(func(tx core.App) error {
		rec, err := chatFindOwned(tx, s.turns, owner, id)
		if err != nil {
			return err
		}
		if chat.TurnState(rec.GetString(chatFieldState)) != from {
			return chat.ErrConflict
		}
		if !chat.CanTransition(from, to) {
			return chat.ErrInvalidTransition
		}
		if to.Terminal() {
			return chat.ErrInvalidTransition
		}
		if failure != nil {
			raw, err := json.Marshal(failure)
			if err != nil {
				return fmt.Errorf("chat store: marshal turn failure: %w", err)
			}
			chatSetJSON(rec, chatFieldError, raw)
		}
		rec.Set(chatFieldState, string(to))
		rec.Set(chatFieldUpdatedAt, chatNow())
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = chatTurnFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Turn{}, err
	}
	return out, nil
}

func (s *ChatStore) FinishTurn(ctx context.Context, owner chat.OwnerKey, id string, from, to chat.TurnState, ev chat.Event) (chat.Turn, chat.Event, error) {
	type result struct {
		turn  chat.Turn
		event chat.Event
	}
	var res result
	err := s.chatRunTx(func(tx core.App) error {
		rec, err := chatFindOwned(tx, s.turns, owner, id)
		if err != nil {
			return err
		}
		state := chat.TurnState(rec.GetString(chatFieldState))
		if state != from {
			return chat.ErrConflict
		}
		if !to.Terminal() || !chat.CanTransition(from, to) {
			return chat.ErrInvalidTransition
		}
		want, ok := chatTerminalEventFor(to)
		if !ok || ev.Type != want {
			return chat.ErrInvalidTransition
		}
		if ev.TurnID != "" && ev.TurnID != id {
			return chat.ErrConflict
		}
		// A terminal state is only ever stored together with its terminal
		// event, so a non-terminal stored state implies no terminal event.
		if state.Terminal() {
			return chat.ErrConflict
		}
		failure, err := chatTerminalFailure(to, ev)
		if err != nil {
			return err
		}
		ev.TurnID = id
		if ev.ConversationID == "" {
			ev.ConversationID = rec.GetString(chatFieldConversation)
		}
		stored, err := s.appendEventTx(tx, owner, rec, ev)
		if err != nil {
			return err
		}
		rec.Set(chatFieldState, string(to))
		rec.Set(chatFieldUpdatedAt, chatNow())
		rec.Set(chatFieldActiveSlot, "")
		if failure != nil {
			raw, err := json.Marshal(failure)
			if err != nil {
				return fmt.Errorf("chat store: marshal turn failure: %w", err)
			}
			chatSetJSON(rec, chatFieldError, raw)
		} else {
			chatSetJSON(rec, chatFieldError, nil)
		}
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		res.turn, res.event = chatTurnFrom(rec), stored
		return nil
	})
	if err != nil {
		return chat.Turn{}, chat.Event{}, err
	}
	return res.turn, res.event, nil
}

func (s *ChatStore) AppendEvent(ctx context.Context, owner chat.OwnerKey, e chat.Event) (chat.Event, error) {
	var out chat.Event
	err := s.chatRunTx(func(tx core.App) error {
		rec, err := chatFindOwned(tx, s.turns, owner, e.TurnID)
		if err != nil {
			return err
		}
		// Terminal event types belong to FinishTurn, which stores them
		// together with the matching terminal state.
		if e.Type.Terminal() {
			return chat.ErrInvalidTransition
		}
		if chat.TurnState(rec.GetString(chatFieldState)).Terminal() {
			return chat.ErrConflict
		}
		stored, err := s.appendEventTx(tx, owner, rec, e)
		if err != nil {
			return err
		}
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = stored
		return nil
	})
	if err != nil {
		return chat.Event{}, err
	}
	return out, nil
}

// appendEventTx assigns the next per-turn sequence, stores the event, trims the
// retained window and updates the turn's sequence bookkeeping. The caller saves
// the turn inside the same transaction.
func (s *ChatStore) appendEventTx(tx core.App, owner chat.OwnerKey, turnRec *core.Record, e chat.Event) (chat.Event, error) {
	if !e.Type.Valid() {
		return chat.Event{}, fmt.Errorf("chat: invalid event type %q: %w", string(e.Type), chat.ErrConflict)
	}
	if e.Version == "" {
		e.Version = chat.ProtocolVersion
	}
	if e.ConversationID == "" {
		e.ConversationID = turnRec.GetString(chatFieldConversation)
	}
	if e.TurnID == "" {
		e.TurnID = turnRec.Id
	}
	seq := turnRec.GetInt64(chatFieldLastSeq) + 1
	e.Seq = seq
	e.At = chatNow()

	col, err := chatCollection(tx, s.events)
	if err != nil {
		return chat.Event{}, err
	}
	rec := core.NewRecord(col)
	rec.Set(chatFieldOwnerKey, owner.String())
	rec.Set(chatFieldConversation, e.ConversationID)
	rec.Set(chatFieldTurn, e.TurnID)
	rec.Set(chatFieldSeq, seq)
	rec.Set(chatFieldType, string(e.Type))
	rec.Set(chatFieldVersion, e.Version)
	rec.Set(chatFieldAt, e.At)
	chatSetJSON(rec, chatFieldData, e.Data)
	if err := tx.Save(rec); err != nil {
		return chat.Event{}, err
	}
	turnRec.Set(chatFieldLastSeq, seq)

	// Retention: keep at most maxEvents, dropping the oldest first. The
	// highest dropped sequence is recorded on the turn so a replay cursor
	// older than the window reports ErrResyncRequired instead of silently
	// skipping events.
	if cutoff := seq - int64(s.maxEvents); cutoff > turnRec.GetInt64(chatFieldDroppedBefore) {
		if err := chatDeleteEventsThrough(tx, s.events, owner, e.TurnID, cutoff); err != nil {
			return chat.Event{}, err
		}
		turnRec.Set(chatFieldDroppedBefore, cutoff)
	}
	return chatEventFrom(rec), nil
}

func chatDeleteEventsThrough(app core.App, collection string, owner chat.OwnerKey, turnID string, through int64) error {
	_, err := app.NonconcurrentDB().
		NewQuery("DELETE FROM {{" + collection + "}} WHERE [[ownerKey]] = {:ownerKey} AND [[turn]] = {:turn} AND [[seq]] <= {:seq}").
		Bind(dbx.Params{chatFieldOwnerKey: owner.String(), chatFieldTurn: turnID, chatFieldSeq: through}).
		Execute()
	if err != nil {
		return fmt.Errorf("chat store: trim %s: %w", collection, err)
	}
	return nil
}

func (s *ChatStore) EventsAfter(_ context.Context, owner chat.OwnerKey, turnID string, after int64) ([]chat.Event, error) {
	rec, err := chatFindOwned(s.app, s.turns, owner, turnID)
	if err != nil {
		return nil, err
	}
	if dropped := rec.GetInt64(chatFieldDroppedBefore); dropped > 0 && after < dropped {
		return nil, chat.ErrResyncRequired
	}
	filter := chatFieldOwnerKey + " = {:ownerKey} && " + chatFieldTurn + " = {:turn} && " + chatFieldSeq + " > {:after}"
	params := chatOwnerParams(owner)
	params[chatFieldTurn] = turnID
	params["after"] = after
	recs, err := s.app.FindRecordsByFilter(s.events, filter, chatFieldSeq, 0, 0, params)
	if err != nil {
		return nil, err
	}
	out := make([]chat.Event, 0, len(recs))
	for _, eventRec := range recs {
		out = append(out, chatEventFrom(eventRec))
	}
	return out, nil
}

func (s *ChatStore) InterruptRunning(ctx context.Context) (int, error) {
	count := 0
	err := s.chatRunTx(func(tx core.App) error {
		count = 0
		recs, err := tx.FindRecordsByFilter(s.turns, chatFieldActiveSlot+" != ''", "", 0, 0)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if chat.TurnState(rec.GetString(chatFieldState)).Terminal() {
				// Defensive: a terminal turn must never keep the conversation
				// claimed. It already has its terminal event, so only the
				// stale claim is cleared and no work is rerun.
				if rec.GetString(chatFieldActiveSlot) != "" {
					rec.Set(chatFieldActiveSlot, "")
					if err := tx.SaveWithContext(ctx, rec); err != nil {
						return err
					}
				}
				continue
			}
			failure := chat.TurnFailed{Code: chat.FailureInterrupted, Message: chatInterruptedMessage}
			raw, err := json.Marshal(failure)
			if err != nil {
				return fmt.Errorf("chat store: marshal interrupt failure: %w", err)
			}
			owner := chatOwnerFrom(rec)
			ev := chat.Event{
				Version:        chat.ProtocolVersion,
				ConversationID: rec.GetString(chatFieldConversation),
				TurnID:         rec.Id,
				Type:           chat.EventTurnFailed,
				Data:           raw,
			}
			if _, err := s.appendEventTx(tx, owner, rec, ev); err != nil {
				return err
			}
			rec.Set(chatFieldState, string(chat.TurnStateInterrupted))
			rec.Set(chatFieldUpdatedAt, chatNow())
			rec.Set(chatFieldActiveSlot, "")
			chatSetJSON(rec, chatFieldError, raw)
			if err := tx.SaveWithContext(ctx, rec); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// --- terminal state helpers (mirrors of the chat package's unexported ones) ---

func chatTerminalEventFor(s chat.TurnState) (chat.EventType, bool) {
	switch s {
	case chat.TurnStateCompleted:
		return chat.EventTurnCompleted, true
	case chat.TurnStateCancelled:
		return chat.EventTurnCancelled, true
	case chat.TurnStateFailed, chat.TurnStateInterrupted:
		return chat.EventTurnFailed, true
	}
	return "", false
}

func chatTerminalFailure(to chat.TurnState, ev chat.Event) (*chat.TurnFailed, error) {
	switch to {
	case chat.TurnStateFailed, chat.TurnStateInterrupted:
		var f chat.TurnFailed
		if len(ev.Data) == 0 {
			return nil, fmt.Errorf("chat: %s event without data: %w", ev.Type, chat.ErrConflict)
		}
		if err := json.Unmarshal(ev.Data, &f); err != nil {
			return nil, fmt.Errorf("chat: decode %s data: %w", ev.Type, chat.ErrConflict)
		}
		return &f, nil
	}
	return nil, nil
}

// --- paging cursors (same wire shape as chat.MemStore) ---

type chatCursor struct {
	at time.Time
	id string
}

func chatPageLimit(p chat.Page) int {
	switch {
	case p.Limit <= 0:
		return chatDefaultPageLimit
	case p.Limit > chatMaxPageLimit:
		return chatMaxPageLimit
	default:
		return p.Limit
	}
}

func chatEncodeCursor(at time.Time, id string) string {
	raw := strconv.FormatInt(at.UnixNano(), 10) + chatCursorSeparator + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func chatDecodeCursor(cursor string) (*chatCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("chat: invalid page cursor: %w", err)
	}
	nanos, id, ok := strings.Cut(string(raw), chatCursorSeparator)
	if !ok {
		return nil, errors.New("chat: invalid page cursor")
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("chat: invalid page cursor: %w", err)
	}
	return &chatCursor{at: time.Unix(0, n).UTC(), id: id}, nil
}
