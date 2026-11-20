package chat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ProposalState is the lifecycle state of a proposal. Server effects and
// editor effects share one wire name set but not one state machine: a server
// proposal is executed by the service, an editor proposal only authorizes a
// patch the browser applies locally.
type ProposalState string

const (
	// ProposalStatePending waits for the user's decision.
	ProposalStatePending ProposalState = "pending"
	// ProposalStateRejected was declined before it ran; terminal.
	ProposalStateRejected ProposalState = "rejected"
	// ProposalStateExpired passed its expiry before it was decided; terminal.
	ProposalStateExpired ProposalState = "expired"
	// ProposalStateExecuting is being applied by the server. It is the only
	// state that may leave an effect's outcome unknown across a restart.
	ProposalStateExecuting ProposalState = "executing"
	// ProposalStateSucceeded records a server effect that completed; terminal.
	ProposalStateSucceeded ProposalState = "succeeded"
	// ProposalStateFailed records a server effect that did not complete;
	// terminal.
	ProposalStateFailed ProposalState = "failed"
	// ProposalStateUnknown records a server effect whose outcome cannot be
	// known after a restart. Terminal: it is never rerun automatically.
	ProposalStateUnknown ProposalState = "unknown"
	// ProposalStateApproved authorizes an editor patch. The client applies it
	// locally and reports the outcome.
	ProposalStateApproved ProposalState = "approved"
	// ProposalStateApplied records the client's report that it applied the
	// patch. Terminal, and an acknowledgement rather than proof of a write.
	ProposalStateApplied ProposalState = "applied"
	// ProposalStateStale records the client's report that the patch no longer
	// matches the editor. Terminal.
	ProposalStateStale ProposalState = "stale"
)

// ProposalLocation is where a proposal's effect happens.
type ProposalLocation string

const (
	// ProposalLocationServer means the service applies the effect and records
	// the result.
	ProposalLocationServer ProposalLocation = "server"
	// ProposalLocationEditor means approval only authorizes a client-local
	// draft patch; the browser reports applied, stale or failed.
	ProposalLocationEditor ProposalLocation = "editor"
)

// Hunk statuses a client reports for an editor proposal. They are per-hunk so
// a partial application is never presented as a whole-proposal success.
const (
	HunkStatusApplied = "applied"
	HunkStatusStale   = "stale"
	HunkStatusFailed  = "failed"
	HunkStatusSkipped = "skipped"
)

// UnknownOutcomeMessage is the user-safe failure recorded when a proposal was
// executing at restart. The effect may or may not have happened, so the user
// has to check before retrying.
const UnknownOutcomeMessage = "Outcome unknown after restart; check before retrying."

// Valid reports whether s names a known proposal state.
func (s ProposalState) Valid() bool {
	switch s {
	case ProposalStatePending, ProposalStateRejected, ProposalStateExpired,
		ProposalStateExecuting, ProposalStateSucceeded, ProposalStateFailed,
		ProposalStateUnknown, ProposalStateApproved, ProposalStateApplied,
		ProposalStateStale:
		return true
	}
	return false
}

// Terminal reports whether s ends the proposal's life. No edge leaves a
// terminal state, so a decision or an outcome is recorded exactly once.
func (s ProposalState) Terminal() bool {
	switch s {
	case ProposalStateRejected, ProposalStateExpired, ProposalStateSucceeded,
		ProposalStateFailed, ProposalStateUnknown, ProposalStateApplied,
		ProposalStateStale:
		return true
	}
	return false
}

// Valid reports whether l names a known proposal location.
func (l ProposalLocation) Valid() bool {
	switch l {
	case ProposalLocationServer, ProposalLocationEditor:
		return true
	}
	return false
}

// proposalTransitions is the complete set of edges each location allows. The
// two locations never share an edge: a server proposal is never approved by a
// client, and an editor proposal never executes on the server.
var proposalTransitions = map[ProposalLocation]map[ProposalState][]ProposalState{
	ProposalLocationServer: {
		ProposalStatePending: {ProposalStateExecuting, ProposalStateRejected, ProposalStateExpired},
		// Executing -> pending is the RefusedError path: the tool refused before
		// writing anything, so the user may fix the change and approve again.
		ProposalStateExecuting: {ProposalStateSucceeded, ProposalStateFailed, ProposalStateUnknown, ProposalStatePending},
	},
	ProposalLocationEditor: {
		ProposalStatePending:  {ProposalStateApproved, ProposalStateRejected, ProposalStateExpired},
		ProposalStateApproved: {ProposalStateApplied, ProposalStateStale, ProposalStateFailed, ProposalStateExpired},
	},
}

// CanTransitionProposal reports whether a proposal at location loc may move
// from -> to. An unknown location or state allows nothing.
func CanTransitionProposal(loc ProposalLocation, from, to ProposalState) bool {
	for _, allowed := range proposalTransitions[loc][from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// HunkOutcome is one hunk's reported local application result.
type HunkOutcome struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Proposal is one reviewed model mutation. It binds the owner, target, exact
// validated arguments, tool identity, manifest revision and a user-facing
// effect summary, so a decision addresses the reviewed proposal and not a
// fresh unchecked tool call.
//
// Revision is the optimistic-concurrency token: it starts at 1 and every
// stored mutation (a transition or a revision of the arguments) increments it.
// Owner never reaches the wire, because a client is already scoped to its own
// partition.
type Proposal struct {
	ID               string           `json:"id"`
	ConversationID   string           `json:"conversationId"`
	TurnID           string           `json:"turnId,omitempty"`
	CallID           string           `json:"callId,omitempty"`
	Owner            OwnerKey         `json:"-"`
	Tool             string           `json:"tool"`
	ToolFingerprint  string           `json:"toolFingerprint,omitempty"`
	ManifestRevision string           `json:"manifestRevision,omitempty"`
	Location         ProposalLocation `json:"location"`
	Target           string           `json:"target,omitempty"`
	Arguments        json.RawMessage  `json:"arguments"`
	ArgumentsHash    string           `json:"argumentsHash"`
	Summary          string           `json:"summary,omitempty"`
	State            ProposalState    `json:"state"`
	Revision         int              `json:"revision"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
	// ExpiresAt bounds how long a proposal may be decided. The zero value
	// means it never expires.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	// DecidedAt is set when the proposal left pending through a decision.
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
	// Result is the server outcome of a server effect, or the client report
	// of an editor effect.
	Result json.RawMessage `json:"result,omitempty"`
	// Failure is a user-safe message; it never carries a provider error body.
	Failure string `json:"failure,omitempty"`
	// EditorRevision is the editor revision a client report refers to.
	EditorRevision string `json:"editorRevision,omitempty"`
	// Hunks carries per-hunk editor outcomes when partial application is
	// supported.
	Hunks []HunkOutcome `json:"hunks,omitempty"`
	// Warnings are the host's notes about the proposed change (ToolSpec.Warn),
	// shown on the review card and sent to the model with the pending result.
	// They never block the change.
	Warnings []string `json:"warnings,omitempty"`
}

// Validate reports whether p carries the fields every store and client needs:
// an id, a conversation, a known location and state, a tool, a revision of at
// least one and JSON object arguments.
func (p Proposal) Validate() error {
	switch {
	case p.ID == "":
		return fmt.Errorf("chat: proposal without id")
	case p.ConversationID == "":
		return fmt.Errorf("chat: proposal %q without conversation", p.ID)
	case !p.Location.Valid():
		return fmt.Errorf("chat: proposal %q has invalid location %q", p.ID, string(p.Location))
	case p.Tool == "":
		return fmt.Errorf("chat: proposal %q without tool", p.ID)
	case !p.State.Valid():
		return fmt.Errorf("chat: proposal %q has invalid state %q", p.ID, string(p.State))
	case p.Revision < 1:
		return fmt.Errorf("chat: proposal %q has revision %d, want at least 1", p.ID, p.Revision)
	}
	if _, _, err := HashArguments(p.Arguments); err != nil {
		return fmt.Errorf("chat: proposal %q: %w", p.ID, err)
	}
	if p.DecidedAt != nil && p.DecidedAt.IsZero() {
		return fmt.Errorf("chat: proposal %q has a zero decision time", p.ID)
	}
	for _, h := range p.Hunks {
		switch h.Status {
		case HunkStatusApplied, HunkStatusStale, HunkStatusFailed, HunkStatusSkipped:
		default:
			return fmt.Errorf("chat: proposal %q has invalid hunk status %q", p.ID, h.Status)
		}
	}
	return nil
}

// ProposalDraft is one planned mutation a ProposalPlanner wants reviewed.
//
// The executor fills every field from its own configuration and the validated
// arguments; the runtime only canonicalizes Arguments, binds the owner, turn
// and call, and stores the proposal. Summary must be built server-side, never
// from model text, because the user's review decision is about that summary.
type ProposalDraft struct {
	Tool             string
	ToolFingerprint  string
	ManifestRevision string
	Location         ProposalLocation
	Target           string
	Arguments        json.RawMessage
	Summary          string
	// Warnings are stored on the proposal (see Proposal.Warnings).
	Warnings []string
	// OnStored, when set, runs once the proposal is stored, so side records
	// such as audit lines only exist for proposals that do.
	OnStored func(Proposal)
}

// ProposalUpdate carries the optional fields a transition may set. A zero
// field is left alone, so an outcome never clears what it does not describe.
type ProposalUpdate struct {
	Result         json.RawMessage
	Failure        string
	EditorRevision string
	Hunks          []HunkOutcome
	// Decided records the decision time.
	Decided bool
	// ExpiresAt, when non-zero, replaces the deadline. Approving an editor
	// proposal uses it to give the browser a full TTL to report its outcome.
	ExpiresAt time.Time
	// ClearFailure empties Failure before Failure is applied, so an outcome
	// doesn't keep the message of an earlier refusal.
	ClearFailure bool
	// ClearDecided empties DecidedAt: a refused approval puts the proposal
	// back to pending, undecided.
	ClearDecided bool
}

// HashArguments returns the canonical form of a proposal's arguments and the
// hash a decision is bound to.
//
// Arguments must be a JSON object. The canonical form decodes the value (with
// json.Number, so a number keeps its exact literal) and re-encodes it, which
// sorts object keys at every level; the hash is "sha256:<hex>" over those
// bytes. Two spellings of the same arguments therefore hash identically, and a
// decision cannot be replayed against edited arguments without a new revision.
func HashArguments(raw json.RawMessage) (canonical json.RawMessage, hash string, err error) {
	if len(raw) == 0 {
		return nil, "", fmt.Errorf("chat: proposal arguments are empty")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, "", fmt.Errorf("chat: decode proposal arguments: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, "", fmt.Errorf("chat: proposal arguments carry trailing data")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("chat: proposal arguments must be a JSON object")
	}
	canonical, err = json.Marshal(obj)
	if err != nil {
		return nil, "", fmt.Errorf("chat: encode proposal arguments: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return canonical, "sha256:" + hex.EncodeToString(sum[:]), nil
}
