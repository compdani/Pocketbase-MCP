package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// defaultDecisionTimeout bounds one approved proposal's execution when
// HandlerConfig.DecisionTimeout leaves it unset.
const defaultDecisionTimeout = 30 * time.Second

// maxNoticeResultBytes bounds the result JSON copied into a proposal_outcome
// notice, so one large tool result cannot crowd the model's context.
const maxNoticeResultBytes = 2000

// proposalView is the response of GET {prefix}/proposals/{id}.
type proposalView struct {
	Proposal Proposal `json:"proposal"`
}

// decisionView is the response of a successful decision. Replayed is set when
// the decision had already been applied, so a retried request returns the stored
// outcome instead of running anything again.
type decisionView struct {
	Proposal Proposal `json:"proposal"`
	Replayed bool     `json:"replayed,omitempty"`
}

// proposalErrorView is an error response that also carries the authoritative
// stored proposal, so a client can render the state a conflict revealed without
// a second request.
type proposalErrorView struct {
	Error    errorDetail `json:"error"`
	Proposal *Proposal   `json:"proposal,omitempty"`
}

// decisionRequest is the body of POST {prefix}/proposals/{id}/decision.
//
// Revision is the proposal's optimistic-concurrency token, required so a
// decision addresses exactly the version the user reviewed. Arguments is
// optional and only honoured when the host allows argument edits.
type decisionRequest struct {
	Decision  string          `json:"decision"`
	Revision  *int            `json:"revision"`
	Arguments json.RawMessage `json:"arguments"`
}

const (
	decisionApprove = "approve"
	decisionReject  = "reject"
)

// getProposal returns one caller-owned proposal.
func (h *handler) getProposal(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	p, ok := h.loadProposal(w, r, caller, r.PathValue("id"))
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, proposalView{Proposal: p})
}

// loadProposal reads one caller-owned proposal, writing the response on
// failure. Another owner's proposal is indistinguishable from a missing one,
// and a proposal whose conversation target is no longer allowed is hidden the
// same way, so access revocation reaches existing proposals too.
func (h *handler) loadProposal(w http.ResponseWriter, r *http.Request, caller Caller, id string) (Proposal, bool) {
	p, err := h.cfg.Proposals.GetProposal(r.Context(), caller.Owner, id)
	if err != nil {
		h.storeError(w, err)
		return Proposal{}, false
	}
	if _, ok := h.gateConversation(w, r, caller, p.ConversationID); !ok {
		return Proposal{}, false
	}
	return p, true
}

// decideProposal approves or rejects one stored proposal.
//
// Approval executes a server proposal exactly once: the compare-and-swap to
// executing picks a single winner, the caller is re-authenticated and the tool,
// manifest revision and arguments are rechecked, and the outcome is recorded.
//
// Approval of an editor proposal runs nothing: it authorizes the browser to
// apply the draft patch locally, and the client reports the result through the
// outcome route.
func (h *handler) decideProposal(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	var req decisionRequest
	if !h.decodeBody(w, r, false, &req) {
		return
	}
	decision := strings.ToLower(strings.TrimSpace(req.Decision))
	if decision != decisionApprove && decision != decisionReject {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}
	if req.Revision == nil || *req.Revision < 1 {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}
	edited := len(bytes.TrimSpace(req.Arguments)) > 0 && string(bytes.TrimSpace(req.Arguments)) != "null"

	id := r.PathValue("id")
	stored, ok := h.loadProposal(w, r, caller, id)
	if !ok {
		return
	}
	switch stored.Location {
	case ProposalLocationServer, ProposalLocationEditor:
	default:
		// A location this release does not know cannot be decided here.
		h.writeError(w, http.StatusBadRequest, CodeUnsupported, msgProposalUnsupported)
		return
	}
	if stored.State != ProposalStatePending {
		h.replayDecision(w, stored, decision)
		return
	}
	if proposalPastExpiry(stored, time.Now().UTC()) {
		h.expireProposal(w, r, caller, stored, decision)
		return
	}
	if *req.Revision != stored.Revision {
		h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalStale, stored)
		return
	}

	if decision == decisionReject {
		h.rejectProposal(w, r, caller, stored)
		return
	}
	if stored.Location == ProposalLocationEditor {
		h.approveEditorProposal(w, r, caller, stored, req.Arguments, edited)
		return
	}
	h.approveProposal(w, r, caller, stored, req.Arguments, edited)
}

// expireProposal applies the one edge an overdue pending proposal allows and
// answers with it. Including the decision is illegal there, so the response is
// a conflict that carries the stored expired proposal.
func (h *handler) expireProposal(w http.ResponseWriter, r *http.Request, caller Caller, p Proposal, decision string) {
	expired, err := h.cfg.Proposals.TransitionProposal(r.Context(), caller.Owner, p.ID,
		ProposalStatePending, p.Revision, ProposalStateExpired, ProposalUpdate{Decided: true})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			h.resolveConflict(w, r, caller, p.ID, decision)
			return
		}
		h.storeError(w, err)
		return
	}
	h.notifyProposal(r.Context(), expired)
	h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalExpired, expired)
}

// resolveConflict re-reads a proposal after a lost compare-and-swap and answers
// from its stored state: a replay when the decision already applied, an expired
// conflict when the deadline passed, and a stale conflict otherwise.
func (h *handler) resolveConflict(w http.ResponseWriter, r *http.Request, caller Caller, id, decision string) {
	latest, ok := h.loadProposal(w, r, caller, id)
	if !ok {
		return
	}
	if latest.State != ProposalStatePending {
		h.replayDecision(w, latest, decision)
		return
	}
	if proposalPastExpiry(latest, time.Now().UTC()) {
		// The deadline passed between the decision's read and its write. The
		// expired state is authoritative even if this transition also loses.
		if expired, err := h.cfg.Proposals.TransitionProposal(r.Context(), caller.Owner, id,
			ProposalStatePending, latest.Revision, ProposalStateExpired, ProposalUpdate{Decided: true}); err == nil {
			h.notifyProposal(r.Context(), expired)
			h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalExpired, expired)
			return
		}
	}
	h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalStale, latest)
}

// proposalPastExpiry reports whether p's non-zero expiry has passed at now.
func proposalPastExpiry(p Proposal, now time.Time) bool {
	return !p.ExpiresAt.IsZero() && !p.ExpiresAt.After(now)
}

// replayDecision answers a decision that addressed a proposal which is no
// longer pending. The same decision applied twice is idempotent and returns the
// stored state; a different one is a conflict.
func (h *handler) replayDecision(w http.ResponseWriter, p Proposal, decision string) {
	if decisionApplied(p, decision) {
		h.writeJSON(w, http.StatusOK, decisionView{Proposal: p, Replayed: true})
		return
	}
	h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalDecided, p)
}

// decisionApplied reports whether p is in a state decision produced. A server
// approval ends in an execution state; an editor approval ends in approved and
// then in whatever the browser reported, so all four are the same decision.
func decisionApplied(p Proposal, decision string) bool {
	if decision == decisionReject {
		return p.State == ProposalStateRejected
	}
	if p.Location == ProposalLocationEditor {
		switch p.State {
		case ProposalStateApproved, ProposalStateApplied, ProposalStateStale, ProposalStateFailed:
			return true
		}
		return false
	}
	switch p.State {
	case ProposalStateExecuting, ProposalStateSucceeded, ProposalStateFailed, ProposalStateUnknown:
		return true
	}
	return false
}

// writeProposalError writes one error response that also carries the stored
// proposal, so a client can correct its view from the same reply.
func (h *handler) writeProposalError(w http.ResponseWriter, status int, code, message string, p Proposal) {
	h.writeJSON(w, status, proposalErrorView{
		Error:    errorDetail{Code: code, Message: message},
		Proposal: &p,
	})
}

// rejectProposal records a rejection. Nothing runs, and the transcript gains a
// notice so the next turn's model knows the change was declined.
func (h *handler) rejectProposal(w http.ResponseWriter, r *http.Request, caller Caller, p Proposal) {
	rejected, err := h.cfg.Proposals.TransitionProposal(r.Context(), caller.Owner, p.ID,
		ProposalStatePending, p.Revision, ProposalStateRejected, ProposalUpdate{Decided: true})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			h.resolveConflict(w, r, caller, p.ID, decisionReject)
			return
		}
		h.storeError(w, err)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	h.notifyProposal(ctx, rejected)
	h.appendOutcomeNotice(ctx, rejected)
	h.writeJSON(w, http.StatusOK, decisionView{Proposal: rejected})
}

// reviseAndCheck is the part of an approval both locations share: apply an
// optional argument edit as a new revision, then recheck that the proposal can
// still be offered to this caller. A stale proposal stays pending (the user can
// still reject it) and nothing runs. ok is false once a response was written.
func (h *handler) reviseAndCheck(w http.ResponseWriter, r *http.Request, caller Caller, p Proposal, editedArgs json.RawMessage, edited bool) (ProposalExecutor, Proposal, bool) {
	ctx := r.Context()
	executor, ok := caller.Tools.(ProposalExecutor)
	if !ok {
		h.writeError(w, http.StatusBadRequest, CodeUnsupported, msgProposalUnsupported)
		return nil, p, false
	}
	if edited {
		if !h.cfg.AllowArgumentEdits {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgArgumentEditsOff)
			return nil, p, false
		}
		canonical, hash, err := executor.Revise(ctx, p, editedArgs)
		if err != nil {
			h.writeError(w, http.StatusUnprocessableEntity, CodeInvalidArguments, toolFailureText(err, msgInvalidArguments))
			return nil, p, false
		}
		revised, err := h.cfg.Proposals.ReviseProposal(ctx, caller.Owner, p.ID, p.Revision, canonical, hash)
		if err != nil {
			if errors.Is(err, ErrConflict) {
				h.resolveConflict(w, r, caller, p.ID, decisionApprove)
			} else {
				h.storeError(w, err)
			}
			return nil, p, false
		}
		h.notifyProposal(ctx, revised)
		p = revised
	}
	if err := executor.Check(ctx, p); err != nil {
		h.writeProposalError(w, http.StatusConflict, CodeStale, toolFailureText(err, msgProposalStale), p)
		return nil, p, false
	}
	return executor, p, true
}

// approveProposal validates and executes one server proposal.
//
// The stored proposal is deliberately never trusted as an authorization: the
// executor re-authenticates the approving caller, rechecks the tool definition,
// the manifest revision and the arguments, and only then does the transition to
// executing decide who may run it.
func (h *handler) approveProposal(w http.ResponseWriter, r *http.Request, caller Caller, p Proposal, editedArgs json.RawMessage, edited bool) {
	ctx := r.Context()
	executor, p, ok := h.reviseAndCheck(w, r, caller, p, editedArgs, edited)
	if !ok {
		return
	}

	// The compare-and-swap to executing is the single point that authorizes an
	// effect: exactly one concurrent decision wins it and only the winner runs
	// the tool. A loser returns the stored state instead of retrying.
	executing, err := h.cfg.Proposals.TransitionProposal(ctx, caller.Owner, p.ID,
		ProposalStatePending, p.Revision, ProposalStateExecuting, ProposalUpdate{Decided: true})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			h.resolveConflict(w, r, caller, p.ID, decisionApprove)
			return
		}
		h.storeError(w, err)
		return
	}

	// From here the effect may happen, so every store write is detached from
	// the client: a closed browser tab must not leave the proposal executing
	// with no recorded outcome.
	recordCtx := context.WithoutCancel(ctx)
	h.notifyProposal(recordCtx, executing)

	execCtx, cancel := context.WithTimeout(recordCtx, h.cfg.DecisionTimeout)
	defer cancel()
	result, execErr := executor.Execute(execCtx, executing)

	var refused RefusedError
	if errors.As(execErr, &refused) {
		// Nothing was written: the proposal is pending again, carrying the
		// reason, and the user can fix and approve it again.
		msg, _ := toolFailureMessage(execErr)
		pending, err := h.cfg.Proposals.TransitionProposal(recordCtx, caller.Owner, executing.ID,
			ProposalStateExecuting, executing.Revision, ProposalStatePending, ProposalUpdate{Failure: msg, ClearDecided: true})
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, CodeInternal, msgInternal)
			return
		}
		h.notifyProposal(recordCtx, pending)
		h.writeProposalError(w, http.StatusUnprocessableEntity, CodeRefused, msg, pending)
		return
	}

	final, err := h.recordOutcome(recordCtx, caller, executing, result, execErr)
	if err != nil {
		// The client learns nothing about a stored execution, but the proposal
		// is already executing: restart recovery turns it into unknown later.
		h.writeError(w, http.StatusInternalServerError, CodeInternal, msgInternal)
		return
	}
	h.notifyProposal(recordCtx, final)
	h.appendOutcomeNotice(recordCtx, final)
	h.writeJSON(w, http.StatusOK, decisionView{Proposal: final})
}

// approveEditorProposal approves one editor proposal. Nothing runs here: the
// approval only authorizes the browser to apply the draft patch locally.
//
// The same rechecks a server approval does still apply. The approving caller is
// re-authenticated, the tool definition and manifest revision are compared with
// what the user reviewed, and the arguments are validated again. A stale
// proposal stays pending, so the user can reject it.
func (h *handler) approveEditorProposal(w http.ResponseWriter, r *http.Request, caller Caller, p Proposal, editedArgs json.RawMessage, edited bool) {
	ctx := r.Context()
	_, p, ok := h.reviseAndCheck(w, r, caller, p, editedArgs, edited)
	if !ok {
		return
	}

	// Detach from the request: the approval and its transcript notice are the
	// durable record the browser applies against, so a disconnected tab must
	// not lose them.
	ctx = context.WithoutCancel(ctx)
	// The browser applies the patch after approval, so its report gets the
	// same window the reviewer had, counted from now.
	update := ProposalUpdate{Decided: true}
	if !p.ExpiresAt.IsZero() {
		update.ExpiresAt = time.Now().UTC().Add(p.ExpiresAt.Sub(p.CreatedAt))
	}
	approved, err := h.cfg.Proposals.TransitionProposal(ctx, caller.Owner, p.ID,
		ProposalStatePending, p.Revision, ProposalStateApproved, update)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			h.resolveConflict(w, r, caller, p.ID, decisionApprove)
			return
		}
		h.storeError(w, err)
		return
	}
	h.notifyProposal(ctx, approved)
	h.appendOutcomeNotice(ctx, approved)
	h.writeJSON(w, http.StatusOK, decisionView{Proposal: approved})
}

// recordOutcome stores the terminal state of an executed proposal. A ToolError
// is a definite non-application; anything else may have applied.
func (h *handler) recordOutcome(ctx context.Context, caller Caller, executing Proposal, result json.RawMessage, execErr error) (Proposal, error) {
	from, revision, id := ProposalStateExecuting, executing.Revision, executing.ID
	if execErr == nil {
		if len(result) == 0 {
			result = json.RawMessage("null")
		}
		if !json.Valid(result) {
			// The wire contract requires JSON; an executor that returned
			// something else must not make the stored proposal unmarshalable.
			result, _ = json.Marshal(string(result))
		}
		return h.cfg.Proposals.TransitionProposal(ctx, caller.Owner, id, from, revision,
			ProposalStateSucceeded, ProposalUpdate{Result: result, ClearFailure: true})
	}
	if msg, definite := toolFailureMessage(execErr); definite {
		return h.cfg.Proposals.TransitionProposal(ctx, caller.Owner, id, from, revision,
			ProposalStateFailed, ProposalUpdate{Failure: msg, ClearFailure: true})
	}
	return h.cfg.Proposals.TransitionProposal(ctx, caller.Owner, id, from, revision,
		ProposalStateUnknown, ProposalUpdate{Failure: UnknownOutcomeMessage})
}

// notifyProposal appends proposal.updated to the proposal's turn so a live
// stream sees the decision. A turn that already ended refuses the event; that
// is expected and never fails the decision.
func (h *handler) notifyProposal(ctx context.Context, p Proposal) {
	if p.TurnID == "" || h.cfg.Runner == nil {
		return
	}
	_, _ = h.cfg.Runner.appendTurnEvent(context.WithoutCancel(ctx), p.Owner, p.TurnID, EventProposalUpdated, ProposalEventData{Proposal: p})
}

// appendOutcomeNotice writes the transcript notice a decided proposal leaves
// behind. The next turn's model then knows what the user did and what happened,
// without the client resending anything.
func (h *handler) appendOutcomeNotice(ctx context.Context, p Proposal) {
	text, isError, ok := proposalNotice(p)
	if !ok {
		return
	}
	_, _ = h.cfg.Store.AppendMessage(context.WithoutCancel(ctx), p.Owner, Message{
		ConversationID: p.ConversationID,
		Role:           RoleNotice,
		Parts: []Part{{
			Type:       PartProposalOutcome,
			ProposalID: p.ID,
			State:      string(p.State),
			Tool:       p.Tool,
			Text:       text,
			Result:     cloneRaw(p.Result),
			IsError:    isError,
		}},
	})
}

// nonTerminalProposals returns one conversation's undecided proposals, oldest
// first. Overdue pending or approved proposals are left out without a write:
// the adapters' sweep (or the next decision) stores them as expired.
func (h *handler) nonTerminalProposals(ctx context.Context, owner OwnerKey, conversationID string) ([]Proposal, error) {
	now := time.Now().UTC()
	states := []ProposalState{ProposalStatePending, ProposalStateApproved, ProposalStateExecuting}
	out := []Proposal{}
	page := Page{Limit: maxPageLimit}
	for {
		items, next, err := h.cfg.Proposals.ListProposals(ctx, owner, conversationID, states, page)
		if err != nil {
			return nil, err
		}
		for _, p := range items {
			if p.State != ProposalStateExecuting && proposalPastExpiry(p, now) {
				continue
			}
			out = append(out, p)
		}
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
}

// --- editor outcomes ---

// Bounds on one client-reported editor outcome. They keep a hostile or buggy
// host from writing an unbounded report into the proposal and the transcript.
const (
	maxOutcomeEditorRevision = 200
	maxOutcomeHunks          = 500
	maxOutcomeHunkID         = 200
	maxOutcomeMessageRunes   = 500
)

// outcomeHunk is one hunk's reported local result in an outcome request.
type outcomeHunk struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// outcomeRequest is the body of POST {prefix}/proposals/{id}/outcome.
//
// Revision is the approved proposal's revision, so the report addresses exactly
// the approval it belongs to. Status is what the browser did with the patch:
// applied, stale or failed. Hunks and Message are optional.
type outcomeRequest struct {
	Revision       *int          `json:"revision"`
	Status         *string       `json:"status"`
	EditorRevision *string       `json:"editorRevision"`
	Hunks          []outcomeHunk `json:"hunks"`
	Message        *string       `json:"message"`
}

const (
	outcomeApplied = "applied"
	outcomeStale   = "stale"
	outcomeFailed  = "failed"
)

// proposalOutcome records what the browser editor did with an approved editor
// proposal.
//
// The report is an acknowledgement, never proof of a saved PocketBase write:
// the state it records only says the draft patch was applied locally, went
// stale, or failed. Saving still goes through the app's own protected API.
func (h *handler) proposalOutcome(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	var req outcomeRequest
	if !h.decodeBody(w, r, false, &req) {
		return
	}
	status, target, editorRevision, message, ok := h.validateOutcome(w, &req)
	if !ok {
		return
	}

	id := r.PathValue("id")
	stored, ok := h.loadProposal(w, r, caller, id)
	if !ok {
		return
	}
	if stored.Location != ProposalLocationEditor {
		// A server proposal has a server outcome; a client report would be a
		// different, unchecked claim.
		h.writeError(w, http.StatusBadRequest, CodeUnsupported, msgProposalOutcomeUnsupported)
		return
	}
	if stored.State != ProposalStateApproved {
		if stored.State == target {
			// The same report twice is idempotent and returns the stored state,
			// even if a retry carries an older revision.
			h.writeJSON(w, http.StatusOK, decisionView{Proposal: stored, Replayed: true})
			return
		}
		h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalNotApproved, stored)
		return
	}
	if *req.Revision != stored.Revision {
		h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalStale, stored)
		return
	}
	if proposalPastExpiry(stored, time.Now().UTC()) {
		// An approved proposal may expire too: the browser may not apply a
		// patch whose approval the user has since outlived.
		expired, err := h.cfg.Proposals.TransitionProposal(r.Context(), caller.Owner, stored.ID,
			ProposalStateApproved, stored.Revision, ProposalStateExpired, ProposalUpdate{})
		if err != nil {
			if errors.Is(err, ErrConflict) {
				h.resolveOutcomeConflict(w, r, caller, stored.ID, target)
				return
			}
			h.storeError(w, err)
			return
		}
		h.notifyProposal(r.Context(), expired)
		h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalExpired, expired)
		return
	}

	update := ProposalUpdate{EditorRevision: editorRevision}
	if status != outcomeApplied && message != "" {
		update.Failure = message
	}
	if len(req.Hunks) > 0 {
		update.Hunks = make([]HunkOutcome, 0, len(req.Hunks))
		for _, hunk := range req.Hunks {
			update.Hunks = append(update.Hunks, HunkOutcome{ID: hunk.ID, Status: hunk.Status})
		}
	}

	final, err := h.cfg.Proposals.TransitionProposal(r.Context(), caller.Owner, stored.ID,
		ProposalStateApproved, stored.Revision, target, update)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			h.resolveOutcomeConflict(w, r, caller, stored.ID, target)
			return
		}
		h.storeError(w, err)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	h.notifyProposal(ctx, final)
	h.appendOutcomeNotice(ctx, final)
	h.writeJSON(w, http.StatusOK, decisionView{Proposal: final})
}

// validateOutcome checks one outcome body and reduces it to what the store
// needs. Every violation is the same fixed invalid-request answer, so a client
// can only branch on the code.
func (h *handler) validateOutcome(w http.ResponseWriter, req *outcomeRequest) (status string, target ProposalState, editorRevision, message string, ok bool) {
	invalid := func() (string, ProposalState, string, string, bool) {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return "", "", "", "", false
	}
	if req.Revision == nil || *req.Revision < 1 || req.Status == nil {
		return invalid()
	}
	status = strings.ToLower(strings.TrimSpace(*req.Status))
	switch status {
	case outcomeApplied:
		target = ProposalStateApplied
	case outcomeStale:
		target = ProposalStateStale
	case outcomeFailed:
		target = ProposalStateFailed
	default:
		return invalid()
	}
	if req.EditorRevision != nil {
		editorRevision = *req.EditorRevision
		if utf8.RuneCountInString(editorRevision) > maxOutcomeEditorRevision {
			return invalid()
		}
	}
	if status == outcomeApplied && strings.TrimSpace(editorRevision) == "" {
		// An application with no revision cannot be checked or referred to.
		return invalid()
	}
	if len(req.Hunks) > maxOutcomeHunks {
		return invalid()
	}
	seen := make(map[string]struct{}, len(req.Hunks))
	for _, hunk := range req.Hunks {
		if hunk.ID == "" || utf8.RuneCountInString(hunk.ID) > maxOutcomeHunkID {
			return invalid()
		}
		if _, dup := seen[hunk.ID]; dup {
			return invalid()
		}
		seen[hunk.ID] = struct{}{}
		switch hunk.Status {
		case HunkStatusApplied, HunkStatusStale, HunkStatusFailed, HunkStatusSkipped:
		default:
			return invalid()
		}
	}
	if req.Message != nil {
		message = *req.Message
		if utf8.RuneCountInString(message) > maxOutcomeMessageRunes {
			return invalid()
		}
	}
	return status, target, editorRevision, message, true
}

// resolveOutcomeConflict re-reads a proposal after a lost compare-and-swap and
// answers from its stored state: a replay when the same report already landed,
// an expired conflict when the approval ran out, and a conflict otherwise.
func (h *handler) resolveOutcomeConflict(w http.ResponseWriter, r *http.Request, caller Caller, id string, want ProposalState) {
	latest, ok := h.loadProposal(w, r, caller, id)
	if !ok {
		return
	}
	if latest.State == want {
		h.writeJSON(w, http.StatusOK, decisionView{Proposal: latest, Replayed: true})
		return
	}
	if latest.State == ProposalStateExpired {
		h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalExpired, latest)
		return
	}
	h.writeProposalError(w, http.StatusConflict, CodeConflict, msgProposalNotApproved, latest)
}

// --- notices ---

// proposalNotice is the transcript sentence a decided proposal leaves for the
// next turn's model, and whether it reports an error. ok is false for states
// that leave no notice. The summary is server-built (never model text), so it is
// safe to quote. Editor notices say what happened in the browser and that
// nothing is saved yet: an applied draft is never a completed PocketBase write.
func proposalNotice(p Proposal) (text string, isError, ok bool) {
	editor := p.Location == ProposalLocationEditor
	var detail string
	switch p.State {
	case ProposalStateSucceeded:
		detail = "approved and applied."
		if len(p.Result) > 0 {
			detail += " Result: " + compactNoticeResult(p.Result)
		}
	case ProposalStateApproved:
		if !editor {
			return "", false, false
		}
		detail = "approved; waiting for the editor to apply it."
	case ProposalStateApplied:
		revision := p.EditorRevision
		if revision == "" {
			revision = "an unknown revision"
		}
		detail = "applied in the editor at revision " + revision + " (not saved yet)."
		if applied, total := countAppliedHunks(p.Hunks); total > 0 && applied < total {
			detail = fmt.Sprintf("partially applied in the editor (%d of %d parts) at revision %s (not saved yet).", applied, total, revision)
		}
	case ProposalStateStale:
		detail = "could not be applied: the editor content changed. Ask for a new proposal if still needed."
	case ProposalStateRejected:
		detail = "rejected by the user."
	case ProposalStateFailed:
		isError = true
		switch {
		case editor && p.Failure == "":
			detail = "failed to apply in the editor."
		case editor:
			detail = "failed to apply in the editor: " + strconv.Quote(truncateNotice(p.Failure))
		case p.Failure == "":
			detail = "failed: the tool failed"
		default:
			detail = "failed: " + p.Failure
		}
	case ProposalStateUnknown:
		isError = true
		detail = "outcome unknown; it may or may not have applied. Check before retrying."
	default:
		return "", false, false
	}
	summary := p.Summary
	if summary == "" {
		summary = p.Tool
	}
	return fmt.Sprintf("[Proposal %s %q: %s]", p.ID, summary, detail), isError, true
}

// countAppliedHunks reports how many reported hunks applied out of the total
// the client reported. A partial application is never presented as a whole.
func countAppliedHunks(hunks []HunkOutcome) (applied, total int) {
	for _, h := range hunks {
		if h.Status == HunkStatusApplied {
			applied++
		}
	}
	return applied, len(hunks)
}

// compactNoticeResult renders one stored result as compact JSON and bounds it,
// so a notice stays a short, unambiguous sentence.
func compactNoticeResult(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return truncateNotice(buf.String())
	}
	quoted, err := json.Marshal(string(raw))
	if err != nil {
		return "null"
	}
	return truncateNotice(string(quoted))
}

func truncateNotice(s string) string {
	if len(s) <= maxNoticeResultBytes {
		return s
	}
	cut := maxNoticeResultBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// toolFailureText renders an executor failure as user-safe text: a ToolError's
// own message, or fallback for anything else.
func toolFailureText(err error, fallback string) string {
	if msg, ok := toolFailureMessage(err); ok {
		return msg
	}
	return fallback
}
