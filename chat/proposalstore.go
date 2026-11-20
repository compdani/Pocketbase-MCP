package chat

import (
	"context"
	"encoding/json"
	"time"
)

// ProposalStore is the persistence port for reviewed mutations. It is
// deliberately separate from Store: proposal support is additive, so an
// adapter that only implements Store keeps compiling and the chat service can
// run read-only without it.
//
// Every method takes the caller's full OwnerKey and compares it; a proposal
// owned by someone else is reported exactly like a missing one. Implementations
// must be safe for concurrent use and every method must be atomic with respect
// to the others: two concurrent TransitionProposal or ReviseProposal calls on
// the same state and revision produce exactly one winner and one ErrConflict,
// so a decision is never applied twice.
type ProposalStore interface {
	// CreateProposal stores a new pending proposal. It validates the owner and
	// the record, requires the conversation to exist and to be the caller's
	// (else ErrNotFound), assigns ID when empty, canonicalizes Arguments and
	// fills ArgumentsHash when empty, and sets State pending, Revision 1 and
	// the timestamps. The owner argument is authoritative: a record's own
	// Owner field is ignored. A transient record may carry CreatedAt;
	// UpdatedAt is always derived. A duplicate ID returns ErrConflict, and an
	// ArgumentsHash that does not match Arguments returns ErrConflict too.
	CreateProposal(ctx context.Context, owner OwnerKey, p Proposal) (Proposal, error)

	// GetProposal returns ErrNotFound when id is missing or owned by someone
	// else.
	GetProposal(ctx context.Context, owner OwnerKey, id string) (Proposal, error)

	// ListProposals returns one conversation's proposals in ascending
	// (CreatedAt, ID) order. An empty states filter means every state. It
	// returns ErrNotFound when the conversation is not the caller's.
	// nextCursor is empty when the page is the last one.
	ListProposals(ctx context.Context, owner OwnerKey, conversationID string, states []ProposalState, page Page) ([]Proposal, string, error)

	// TransitionProposal applies from -> to only when the stored state is
	// exactly from and the stored revision is exactly revision (compare-and-
	// swap). A mismatch returns ErrConflict; an edge the proposal's location
	// does not allow returns ErrInvalidTransition. A pending proposal past its
	// ExpiresAt may only move to expired; every other target returns
	// ErrConflict. A successful transition increments Revision and applies the
	// update's non-zero fields.
	TransitionProposal(ctx context.Context, owner OwnerKey, id string, from ProposalState, revision int, to ProposalState, u ProposalUpdate) (Proposal, error)

	// ReviseProposal replaces a pending proposal's arguments, for hosts that
	// allow editing before execution. It is a compare-and-swap on revision and
	// refuses a proposal that is no longer pending with ErrConflict. The
	// arguments are canonicalized and hashed exactly as CreateProposal does;
	// an argsHash that does not match returns ErrConflict. A successful
	// revision increments Revision.
	// It clears Failure and Warnings, which described the old arguments.
	ReviseProposal(ctx context.Context, owner OwnerKey, id string, revision int, args json.RawMessage, argsHash string) (Proposal, error)

	// ExpireProposals moves every pending proposal and every approved editor
	// proposal whose non-zero ExpiresAt is at or before now to expired, and
	// returns how many it changed. An expired proposal cannot execute.
	ExpireProposals(ctx context.Context, now time.Time) (int, error)

	// RecoverExecuting moves every executing proposal to unknown with
	// UnknownOutcomeMessage, because a crash may have happened on either side
	// of its side effect. It must never rerun anything, so the count only
	// reports how many outcomes need reconciliation.
	RecoverExecuting(ctx context.Context) (int, error)
}
