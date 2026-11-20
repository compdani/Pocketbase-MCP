package chat

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// ToolExecutor is the chat runtime's port to the tool layer.
//
// An executor is built for one caller and is already bound to that caller's
// identity by its constructor: the runtime passes a tool name and arguments,
// never an owner or an access token. Authorization, argument validation and
// the approval policy therefore live behind this interface, and a turn can
// neither widen nor bypass them.
//
// Tools must return the definitions currently available to the caller, because
// the available set can change with permissions between turns.
type ToolExecutor interface {
	Tools(ctx context.Context) ([]provider.ToolDef, error)
	Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

// ProposalPlanner is implemented by executors whose write tools need review.
//
// Plan classifies one call before it runs. A nil draft and nil error means run
// it now through Call. A non-nil draft means store a proposal instead of
// running it. An error (a ToolError for user-safe text) fails the call.
//
// The runtime asks the executor, never the model: an executor decides from its
// own configuration and the caller's current permissions which calls need
// review, so a model cannot widen what runs unreviewed.
type ProposalPlanner interface {
	Plan(ctx context.Context, name string, args json.RawMessage) (*ProposalDraft, error)
}

// ProposalExecutor runs an approved server proposal for the caller it is bound
// to.
//
// An executor is built for one authenticated request, so approving a proposal
// always runs it as the caller who approved it, never as the caller who
// planned it and never as a service identity.
type ProposalExecutor interface {
	// Check reports whether p could still run now: same tool, current
	// fingerprint, current manifest revision, still listed for this caller,
	// arguments still valid.
	// A non-nil error is user-safe (ToolError) and means stale/denied: nothing
	// ran.
	Check(ctx context.Context, p Proposal) error

	// Execute runs it. A ToolError means it definitely did not apply (failed).
	// An error wrapping ErrOutcomeUnknown means it may have applied (timeout or
	// lost response after dispatch). Any other error is treated as unknown too.
	Execute(ctx context.Context, p Proposal) (json.RawMessage, error)

	// Revise validates edited arguments for p's tool and returns the canonical
	// arguments and hash (used only when argument edits are enabled).
	Revise(ctx context.Context, p Proposal, args json.RawMessage) (json.RawMessage, string, error)
}

// ErrOutcomeUnknown marks a proposal execution that may have applied. A caller
// wraps it when a dispatch was made but no definite answer came back, so the
// proposal is recorded as unknown and never rerun automatically.
var ErrOutcomeUnknown = errors.New("chat: outcome unknown")

// ToolError is a user-safe tool failure: its Message may be shown to the model
// (as the tool result) and in the tool.failed event. Any other error is
// reported as the generic "Tool failed." so internal details, paths, tokens or
// driver text never reach the model or the browser.
type ToolError struct {
	Message string
}

// Error implements error.
func (e ToolError) Error() string { return e.Message }

// toolFailureMessage extracts the user-safe message of a ToolError, whether it
// was returned by value or by pointer. ok is false for every other error.
func toolFailureMessage(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var value ToolError
	if errors.As(err, &value) {
		if value.Message == "" {
			return toolGenericFailure, true
		}
		return value.Message, true
	}
	var refused RefusedError
	if errors.As(err, &refused) {
		if refused.Message == "" {
			return toolGenericFailure, true
		}
		return refused.Message, true
	}
	var pointer *ToolError
	if errors.As(err, &pointer) && pointer != nil {
		if pointer.Message == "" {
			return toolGenericFailure, true
		}
		return pointer.Message, true
	}
	return "", false
}

// toolGenericFailure is the only text an unexpected tool error contributes to
// the transcript, the event stream or the model context.
const toolGenericFailure = "Tool failed."

// RefusedError reports that a tool refused an approved change before writing
// anything, for a reason the user can fix, such as a validation error from the
// host's own handler. A server proposal whose execution returns it goes back
// to pending at a new revision with Message as its Failure, so the user can
// edit the arguments and approve again. Anywhere else it is a plain tool
// failure. Message is shown to the user and the model.
type RefusedError struct {
	Message string
}

// Error implements error.
func (e RefusedError) Error() string { return e.Message }
