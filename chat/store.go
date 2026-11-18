package chat

import (
	"context"
	"errors"
)

// Store errors. Adapters must return these values (wrapped or direct) so the
// HTTP layer can map them to responses without guessing.
var (
	// ErrNotFound reports a missing record. A record that exists but belongs to
	// another owner is reported exactly the same way, so existence never leaks.
	ErrNotFound = errors.New("chat: not found")

	// ErrConflict reports a lost compare-and-swap or an event that may not be
	// appended (after a terminal event, or a second terminal event).
	ErrConflict = errors.New("chat: conflict")

	// ErrActiveTurn reports that a conversation already has a non-terminal turn.
	ErrActiveTurn = errors.New("chat: conversation already has an active turn")

	// ErrResyncRequired reports a replay cursor older than the events still
	// retained. The client must reload canonical state instead of replaying.
	ErrResyncRequired = errors.New("chat: resync required")
)

// Page is a bounded, cursor-based list request. An empty Cursor starts at the
// beginning; an empty Limit means the store's default.
type Page struct {
	Limit  int
	Cursor string
}

// Store is the persistence port for the chat service. Every method takes the
// caller's full OwnerKey and every lookup compares it; adapters must never
// resolve a record by ID alone.
//
// Implementations must be safe for concurrent use. CreateTurn, TransitionTurn,
// AppendEvent and FinishTurn are the concurrency-sensitive operations: each
// must behave atomically with respect to the others.
//
// A turn ends through exactly one of three doors: FinishTurn, InterruptRunning,
// or a restart recovery adapter that behaves like FinishTurn. TransitionTurn
// refuses terminal target states and AppendEvent refuses terminal event types,
// so a turn can never become terminal without its terminal event, or gain a
// terminal event without the matching state.
type Store interface {
	CreateConversation(ctx context.Context, c Conversation) (Conversation, error)

	// GetConversation returns ErrNotFound when id is missing or owned by
	// someone else.
	GetConversation(ctx context.Context, owner OwnerKey, id string) (Conversation, error)

	// ListConversations returns caller-owned conversations, newest first by
	// (CreatedAt, ID). An empty target means every target. nextCursor is empty
	// when the page is the last one.
	ListConversations(ctx context.Context, owner OwnerKey, target string, page Page) ([]Conversation, string, error)

	RenameConversation(ctx context.Context, owner OwnerKey, id, title string) (Conversation, error)

	// DeleteConversation refuses with ErrActiveTurn while a non-terminal turn
	// exists.
	DeleteConversation(ctx context.Context, owner OwnerKey, id string) error

	AppendMessage(ctx context.Context, owner OwnerKey, m Message) (Message, error)

	// ListMessages returns one conversation's transcript in ascending creation
	// order. It returns ErrNotFound when the conversation is not the caller's.
	ListMessages(ctx context.Context, owner OwnerKey, conversationID string, page Page) ([]Message, string, error)

	// CreateTurn atomically claims the conversation's single active turn. If a
	// turn with the same (owner, conversationID, IdempotencyKey) already
	// exists, that turn is returned with created false. Otherwise the method
	// returns ErrActiveTurn when the conversation already has a non-terminal
	// turn, and inserts a queued turn under that claim.
	CreateTurn(ctx context.Context, owner OwnerKey, t Turn) (Turn, bool, error)

	GetTurn(ctx context.Context, owner OwnerKey, id string) (Turn, error)

	// TransitionTurn applies from -> to only when the stored state is exactly
	// from (compare-and-swap). A state mismatch returns ErrConflict; a
	// disallowed edge, or a terminal target state, returns
	// ErrInvalidTransition. failure is recorded on the turn (and may be nil).
	//
	// Because a terminal target is refused, a turn can only end through
	// FinishTurn (or InterruptRunning for restart recovery).
	TransitionTurn(ctx context.Context, owner OwnerKey, id string, from, to TurnState, failure *TurnFailed) (Turn, error)

	// FinishTurn atomically ends a turn: it checks the compare-and-swap (the
	// stored state must be exactly from, else ErrConflict), that from -> to is
	// allowed and to is terminal (else ErrInvalidTransition), and that ev.Type
	// is the terminal event type of to (completed<->turn.completed,
	// cancelled<->turn.cancelled, failed or interrupted<->turn.failed, else
	// ErrInvalidTransition). It records the turn's failure from the event data
	// for failed and interrupted turns, stores the state and appends the event
	// in the same critical section, so a turn is never terminal without its
	// terminal event.
	FinishTurn(ctx context.Context, owner OwnerKey, id string, from, to TurnState, ev Event) (Turn, Event, error)

	// AppendEvent assigns the next per-turn Seq (starting at 1) and At, and
	// returns the stored envelope. It returns ErrConflict for an invalid event,
	// or for an event after a terminal event. It refuses terminal event types
	// with ErrInvalidTransition: only FinishTurn may store those.
	AppendEvent(ctx context.Context, owner OwnerKey, e Event) (Event, error)

	// EventsAfter returns the retained events of one turn with Seq > after, in
	// ascending order. It returns ErrResyncRequired when after is older than the
	// retained window, and ErrNotFound when the turn is not the caller's.
	EventsAfter(ctx context.Context, owner OwnerKey, turnID string, after int64) ([]Event, error)

	// InterruptRunning ends every queued or running turn as interrupted (for
	// restart recovery) and gives each exactly one terminal event. It must not
	// rerun any work. The count is the number of turns interrupted.
	InterruptRunning(ctx context.Context) (int, error)
}

// IdempotentTurnStore is an optional lookup that lets a submission return the
// turn a previous retry already created before it resolves a provider or
// validates attachments. Stores that index the idempotency key should
// implement it; the runner and HTTP handler use it when available.
type IdempotentTurnStore interface {
	FindTurnByIdempotencyKey(ctx context.Context, owner OwnerKey, conversationID, key string) (Turn, bool, error)
}

// ActiveTurnStore is an optional fast path for finding a conversation's one
// non-terminal turn without reading its transcript. Stores that can index the
// active turn (all bundled stores do) should implement it.
type ActiveTurnStore interface {
	// ActiveTurn returns the conversation's non-terminal turn, with found false
	// when there is none. It returns ErrNotFound when the conversation is
	// missing or not the caller's.
	ActiveTurn(ctx context.Context, owner OwnerKey, conversationID string) (turn Turn, found bool, err error)
}

// RecentMessageStore is an optional fast path for reading only the end of a
// transcript, which is all the model ever receives.
type RecentMessageStore interface {
	// RecentMessages returns the conversation's last limit messages in
	// ascending creation order (the same order as ListMessages). It returns
	// ErrNotFound when the conversation is missing or not the caller's.
	RecentMessages(ctx context.Context, owner OwnerKey, conversationID string, limit int) ([]Message, error)
}
