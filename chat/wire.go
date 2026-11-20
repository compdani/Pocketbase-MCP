// Package chat defines the versioned wire contract, ownership keys, turn state
// machine, storage port, runner and HTTP API for the MCP chat surface.
//
// The package is deliberately self-contained: it imports only the Go standard
// library and its own provider port, and knows nothing about PocketBase or any
// vendor's model API. That keeps the same contract usable by a browser client
// (through the JSON schema documented here) and by any server adapter. http.go
// serves that contract directly with net/http.
package chat

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ProtocolVersion is the version carried on every event envelope. Clients and
// servers refuse envelopes whose version they do not implement.
const ProtocolVersion = "1"

// EventType is the type of one entry in a turn's event stream.
type EventType string

const (
	EventTurnQueued      EventType = "turn.queued"
	EventTurnStarted     EventType = "turn.started"
	EventTextDelta       EventType = "text.delta"
	EventToolStarted     EventType = "tool.started"
	EventToolCompleted   EventType = "tool.completed"
	EventToolFailed      EventType = "tool.failed"
	EventProposalCreated EventType = "proposal.created"
	EventProposalUpdated EventType = "proposal.updated"
	EventUsageUpdated    EventType = "usage.updated"
	EventTurnCompleted   EventType = "turn.completed"
	EventTurnFailed      EventType = "turn.failed"
	EventTurnCancelled   EventType = "turn.cancelled"
)

// eventTypes is the closed set of protocol v1 event types.
var eventTypes = map[EventType]struct{}{
	EventTurnQueued:      {},
	EventTurnStarted:     {},
	EventTextDelta:       {},
	EventToolStarted:     {},
	EventToolCompleted:   {},
	EventToolFailed:      {},
	EventProposalCreated: {},
	EventProposalUpdated: {},
	EventUsageUpdated:    {},
	EventTurnCompleted:   {},
	EventTurnFailed:      {},
	EventTurnCancelled:   {},
}

// Terminal reports whether t ends a turn's stream. Every turn carries exactly
// one terminal event.
func (t EventType) Terminal() bool {
	switch t {
	case EventTurnCompleted, EventTurnFailed, EventTurnCancelled:
		return true
	}
	return false
}

// Valid reports whether t is a protocol v1 event type.
func (t EventType) Valid() bool {
	_, ok := eventTypes[t]
	return ok
}

// Event is the single envelope for every stream entry. Seq is per turn,
// monotonically increasing and starts at 1. At is assigned by the store.
type Event struct {
	Version        string          `json:"v"`
	Seq            int64           `json:"seq"`
	ConversationID string          `json:"conversationId"`
	TurnID         string          `json:"turnId"`
	Type           EventType       `json:"type"`
	Data           json.RawMessage `json:"data,omitempty"`
	At             time.Time       `json:"at"`
}

// TextDelta is the data of a text.delta event.
type TextDelta struct {
	Text string `json:"text"`
}

// ToolStarted is the data of a tool.started event.
type ToolStarted struct {
	CallID    string          `json:"callId"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolCompleted is the data of a tool.completed event.
type ToolCompleted struct {
	CallID string          `json:"callId"`
	Tool   string          `json:"tool"`
	Result json.RawMessage `json:"result,omitempty"`
}

// ToolFailed is the data of a tool.failed event.
type ToolFailed struct {
	CallID  string `json:"callId"`
	Tool    string `json:"tool"`
	Message string `json:"message"`
}

// Usage is the data of a usage.updated event. Usage is optional where provider
// support is absent.
type Usage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

// TurnFailed is the data of a turn.failed event, and the recorded failure of a
// turn in a non-completed terminal state.
type TurnFailed struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TurnCancelled is the data of a turn.cancelled event.
type TurnCancelled struct {
	Reason string `json:"reason"`
}

// NewEvent builds an envelope for one typed payload. Sequence and time are left
// zero for the store to assign; callers must not guess them, because only the
// store can keep them monotonic and per-turn.
func NewEvent(conv, turn string, t EventType, data any) (Event, error) {
	if !t.Valid() {
		return Event{}, fmt.Errorf("chat: invalid event type %q", string(t))
	}
	e := Event{
		Version:        ProtocolVersion,
		ConversationID: conv,
		TurnID:         turn,
		Type:           t,
	}
	if data == nil {
		return e, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("chat: marshal %s data: %w", t, err)
	}
	e.Data = raw
	return e, nil
}

// WriteSSE writes e as one Server-Sent Events frame. The whole envelope is the
// data line so a client never has to reconstruct an event from several lines;
// JSON string encoding means the payload contains no raw newline.
func (e Event) WriteSSE(w io.Writer) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("chat: marshal event %d: %w", e.Seq, err)
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, body); err != nil {
		return fmt.Errorf("chat: write event %d: %w", e.Seq, err)
	}
	return nil
}

// WriteHeartbeat writes a non-persisted SSE comment. It carries no id and does
// not advance any sequence.
func WriteHeartbeat(w io.Writer) error {
	if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
		return fmt.Errorf("chat: write heartbeat: %w", err)
	}
	return nil
}

// ProposalView is the wire form of one proposal. It is the Proposal record
// itself: Proposal hides Owner from JSON, so a client never receives another
// partition's identity and needs no separate view struct that could drift from
// the stored contract.
type ProposalView = Proposal

// ProposalEventData is the data of a proposal.created or proposal.updated
// event. Both carry the whole proposal, so a client reduces them with the same
// code instead of reconstructing a decision from a partial payload.
type ProposalEventData struct {
	Proposal ProposalView `json:"proposal"`
}

// Conversation is one caller-owned thread. Target is an optional opaque
// reference to the resource the thread is about; it is never an ownership
// substitute.
type Conversation struct {
	ID        string    `json:"id"`
	Owner     OwnerKey  `json:"-"`
	Title     string    `json:"title"`
	Target    string    `json:"target,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Message roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	// RoleNotice marks a framework-generated transcript note, such as the
	// outcome of a decided proposal. It is never authored by the user or the
	// model: a client renders it as a system note, and providerMessages sends
	// it to the model as a user-role message whose text says it is generated.
	RoleNotice = "notice"
)

// Message is one canonical transcript entry. Parts carry the ordered content.
type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	TurnID         string    `json:"turnId,omitempty"`
	Role           string    `json:"role"`
	Parts          []Part    `json:"parts"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Part types.
const (
	PartText            = "text"
	PartToolCall        = "tool_call"
	PartToolResult      = "tool_result"
	PartProposalOutcome = "proposal_outcome"
	// PartAttachment references one stored upload by id. The bytes live in the
	// upload store, never in the transcript: a message records what the user
	// attached and the runner resolves it again for every provider request.
	PartAttachment = "attachment"
	// PartContext is the page context a user message was sent with, kept
	// when the handler's StoreContext is on. Result holds the JSON object. It
	// is never sent to the model as part of the history: Setup decides what
	// of it the model sees.
	PartContext = "context"
)

// Part is one piece of message content: text, a tool invocation, its result, an
// attachment reference or the recorded outcome of a proposal decision.
//
// A proposal_outcome part carries ProposalID and State, and reuses Tool for the
// proposal's tool name, Text for the user-facing summary, and Result for the
// stored server result. IsError marks a failed or unknown outcome.
//
// An attachment part carries UploadID, Name and MIME. The stored name and type
// are the ones the server assigned at upload time, so a user message still says
// what was attached after the upload itself expires or is deleted.
type Part struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	CallID     string          `json:"callId,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
	ProposalID string          `json:"proposalId,omitempty"`
	State      string          `json:"state,omitempty"`
	UploadID   string          `json:"uploadId,omitempty"`
	Name       string          `json:"name,omitempty"`
	MIME       string          `json:"mime,omitempty"`
}

// Turn is one unit of work in a conversation. IdempotencyKey deduplicates
// submissions whose response was lost.
type Turn struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	Owner          OwnerKey  `json:"-"`
	State          TurnState `json:"state"`
	IdempotencyKey string    `json:"idempotencyKey,omitempty"`
	// Provider is the catalog id that served the turn. Empty means the legacy
	// single-provider path or a turn stored before the catalog existed.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Effort is the reasoning effort the turn asked for. Empty means the
	// model's default.
	Effort    string      `json:"effort,omitempty"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
	Error     *TurnFailed `json:"error,omitempty"`
}
