package pbpostgress

// Proposals: the chat.ProposalStore half of the fork ChatStore.
//
// A proposal is reviewed before anything runs, so its stored row is the whole
// decision: owner, conversation, tool identity, the exact validated arguments
// and their hash, the effect summary, a monotonic revision and a deadline.
//
// Fork-specific behaviour (see the package comment of chatstore.go):
//
//   - PostgreSQL/MySQL transactions really run concurrently across app
//     instances, so TransitionProposal and ReviseProposal lock the proposal row
//     (SELECT ... FOR UPDATE) and re-read it before comparing state and
//     revision. The loser of a race waits for the winner and then sees its
//     committed state, so exactly one decision is applied.
//   - RecoverExecuting and ExpireProposals are scoped to the store's namespace
//     and mount when it has one, exactly like InterruptRunning: restarting one
//     app must never reconcile another app's proposals on the shared database.
//   - JSON payloads (arguments, result, hunks) are stored as TEXT so a stored
//     argument blob round-trips byte for byte.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/compdani/pocketbase-mcp/chat"
)

func chatProposalFrom(rec *core.Record) chat.Proposal {
	p := chat.Proposal{
		ID:               rec.Id,
		ConversationID:   rec.GetString(chatFieldProposalConversation),
		TurnID:           rec.GetString(chatFieldProposalTurn),
		CallID:           rec.GetString(chatFieldCallID),
		Owner:            chatOwnerFrom(rec),
		Tool:             rec.GetString(chatFieldTool),
		ToolFingerprint:  rec.GetString(chatFieldToolFingerprint),
		ManifestRevision: rec.GetString(chatFieldManifestRevision),
		Location:         chat.ProposalLocation(rec.GetString(chatFieldLocation)),
		Target:           rec.GetString(chatFieldTarget),
		Arguments:        chatRawJSON(rec, chatFieldArguments),
		ArgumentsHash:    rec.GetString(chatFieldArgumentsHash),
		Summary:          rec.GetString(chatFieldSummary),
		State:            chat.ProposalState(rec.GetString(chatFieldState)),
		Revision:         rec.GetInt(chatFieldRevision),
		CreatedAt:        rec.GetDateTime(chatFieldCreatedAt).Time(),
		UpdatedAt:        rec.GetDateTime(chatFieldUpdatedAt).Time(),
		ExpiresAt:        rec.GetDateTime(chatFieldExpiresAt).Time(),
		Result:           chatRawJSON(rec, chatFieldResult),
		Failure:          rec.GetString(chatFieldFailure),
		EditorRevision:   rec.GetString(chatFieldEditorRevision),
	}
	if decided := rec.GetDateTime(chatFieldDecidedAt).Time(); !decided.IsZero() {
		p.DecidedAt = &decided
	}
	if raw := chatRawJSON(rec, chatFieldWarnings); len(raw) > 0 {
		var warnings []string
		if err := json.Unmarshal(raw, &warnings); err == nil && len(warnings) > 0 {
			p.Warnings = warnings
		}
	}
	if raw := chatRawJSON(rec, chatFieldHunks); len(raw) > 0 {
		var hunks []chat.HunkOutcome
		if err := json.Unmarshal(raw, &hunks); err == nil {
			p.Hunks = hunks
		}
	}
	return p
}

// chatProposalExpired reports whether a non-zero expiry is at or before at.
func chatProposalExpired(expires, at time.Time) bool {
	return !expires.IsZero() && !expires.After(at)
}

// chatSetHunks stores the per-hunk editor outcomes, or clears them.
func chatSetHunks(rec *core.Record, hunks []chat.HunkOutcome) error {
	if hunks == nil {
		return chatSetJSON(rec, chatFieldHunks, nil)
	}
	raw, err := json.Marshal(hunks)
	if err != nil {
		return fmt.Errorf("chat store: marshal proposal hunks: %w", err)
	}
	return chatSetJSON(rec, chatFieldHunks, raw)
}

// CreateProposal stores a new pending proposal at revision 1. The owner is the
// caller's, never the record's, and the arguments are canonicalized so the
// stored bytes are exactly what ArgumentsHash covers.
func (s *ChatStore) CreateProposal(ctx context.Context, owner chat.OwnerKey, p chat.Proposal) (chat.Proposal, error) {
	if err := owner.Validate(); err != nil {
		return chat.Proposal{}, err
	}
	canonical, hash, err := chat.HashArguments(p.Arguments)
	if err != nil {
		return chat.Proposal{}, err
	}
	if p.ArgumentsHash != "" && p.ArgumentsHash != hash {
		return chat.Proposal{}, fmt.Errorf("chat: proposal arguments hash does not match: %w", chat.ErrConflict)
	}
	col, err := chatCollection(s.app, s.proposals)
	if err != nil {
		return chat.Proposal{}, err
	}
	var out chat.Proposal
	err = s.chatRunTx(func(tx core.App) error {
		// The conversation lock serializes this create with a delete of the
		// same conversation.
		if err := s.chatLockConversation(tx, owner, p.ConversationID); err != nil {
			return err
		}
		rec := core.NewRecord(col)
		id := p.ID
		if id == "" {
			id = core.GenerateDefaultRandomId()
		}
		rec.Set(core.FieldNameId, id)
		chatSetOwner(rec, owner)
		rec.Set(chatFieldProposalConversation, p.ConversationID)
		rec.Set(chatFieldProposalTurn, p.TurnID)
		rec.Set(chatFieldCallID, p.CallID)
		rec.Set(chatFieldTool, p.Tool)
		rec.Set(chatFieldToolFingerprint, p.ToolFingerprint)
		rec.Set(chatFieldManifestRevision, p.ManifestRevision)
		rec.Set(chatFieldLocation, string(p.Location))
		rec.Set(chatFieldTarget, p.Target)
		if err := chatSetJSON(rec, chatFieldArguments, canonical); err != nil {
			return err
		}
		rec.Set(chatFieldArgumentsHash, hash)
		rec.Set(chatFieldSummary, p.Summary)
		rec.Set(chatFieldState, string(chat.ProposalStatePending))
		rec.Set(chatFieldRevision, 1)
		created := chatTime(p.CreatedAt)
		if created.IsZero() {
			created = chatNow()
		}
		rec.Set(chatFieldCreatedAt, created)
		rec.Set(chatFieldUpdatedAt, created)
		rec.Set(chatFieldExpiresAt, chatTime(p.ExpiresAt))
		rec.Set(chatFieldDecidedAt, time.Time{})
		if err := chatSetJSON(rec, chatFieldResult, nil); err != nil {
			return err
		}
		rec.Set(chatFieldFailure, "")
		rec.Set(chatFieldEditorRevision, "")
		if err := chatSetJSON(rec, chatFieldHunks, nil); err != nil {
			return err
		}
		if len(p.Warnings) > 0 {
			raw, err := json.Marshal(p.Warnings)
			if err != nil {
				return err
			}
			if err := chatSetJSON(rec, chatFieldWarnings, raw); err != nil {
				return err
			}
		}

		// The contract's own validation decides what may be stored, so an
		// invalid proposal never reaches the table.
		if err := chatProposalFrom(rec).Validate(); err != nil {
			return err
		}
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			if chatUniqueViolation(err) {
				return chat.ErrConflict
			}
			return err
		}
		out = chatProposalFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Proposal{}, err
	}
	return out, nil
}

// GetProposal returns one caller-owned proposal.
func (s *ChatStore) GetProposal(_ context.Context, owner chat.OwnerKey, id string) (chat.Proposal, error) {
	rec, err := chatFindOwned(s.app, s.proposals, owner, id)
	if err != nil {
		return chat.Proposal{}, err
	}
	return chatProposalFrom(rec), nil
}

// ListProposals returns one conversation's proposals oldest first, optionally
// filtered to a set of states.
func (s *ChatStore) ListProposals(_ context.Context, owner chat.OwnerKey, conversationID string, states []chat.ProposalState, page chat.Page) ([]chat.Proposal, string, error) {
	if _, err := chatFindOwned(s.app, s.conversations, owner, conversationID); err != nil {
		return nil, "", err
	}
	limit := chatPageLimit(page)
	after, err := chatDecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter := chatFieldOwnerKey + " = {:ownerKey} && " + chatFieldProposalConversation + " = {:conversationId}"
	params := chatOwnerParams(owner)
	params[chatFieldProposalConversation] = conversationID
	if len(states) > 0 {
		parts := make([]string, 0, len(states))
		for i, state := range states {
			key := fmt.Sprintf("state%d", i)
			parts = append(parts, chatFieldState+" = {:"+key+"}")
			params[key] = string(state)
		}
		filter += " && (" + strings.Join(parts, " || ") + ")"
	}
	if after != nil {
		filter += " && (" + chatFieldCreatedAt + " > {:at} || (" + chatFieldCreatedAt + " = {:at} && id > {:id}))"
		params["at"] = chatDateParam(after.at)
		params["id"] = after.id
	}
	recs, err := s.app.FindRecordsByFilter(s.proposals, filter, chatFieldCreatedAt+",id", limit+1, 0, params)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(recs) > limit {
		recs = recs[:limit]
		last := recs[limit-1]
		next = chatEncodeCursor(last.GetDateTime(chatFieldCreatedAt).Time(), last.Id)
	}
	out := make([]chat.Proposal, 0, len(recs))
	for _, rec := range recs {
		out = append(out, chatProposalFrom(rec))
	}
	return out, next, nil
}

// TransitionProposal applies one compare-and-swap state change. The row lock
// plus re-read make (state, revision) authoritative for the whole transaction,
// so of any number of concurrent decisions exactly one wins. The store clock
// decides the expiry rule: a pending proposal past its deadline may only move
// to expired.
func (s *ChatStore) TransitionProposal(ctx context.Context, owner chat.OwnerKey, id string, from chat.ProposalState, revision int, to chat.ProposalState, u chat.ProposalUpdate) (chat.Proposal, error) {
	var out chat.Proposal
	err := s.chatRunTx(func(tx core.App) error {
		rec, err := chatLockAndFind(tx, s.proposals, owner, id)
		if err != nil {
			return err
		}
		state := chat.ProposalState(rec.GetString(chatFieldState))
		if state != from || rec.GetInt(chatFieldRevision) != revision {
			return chat.ErrConflict
		}
		if !chat.CanTransitionProposal(chat.ProposalLocation(rec.GetString(chatFieldLocation)), from, to) {
			return chat.ErrInvalidTransition
		}
		ts := chatNow()
		if state == chat.ProposalStatePending &&
			chatProposalExpired(rec.GetDateTime(chatFieldExpiresAt).Time(), ts) &&
			to != chat.ProposalStateExpired {
			return chat.ErrConflict
		}
		rec.Set(chatFieldState, string(to))
		rec.Set(chatFieldRevision, revision+1)
		rec.Set(chatFieldUpdatedAt, ts)
		if u.Result != nil {
			if err := chatSetJSON(rec, chatFieldResult, u.Result); err != nil {
				return err
			}
		}
		if u.ClearFailure {
			rec.Set(chatFieldFailure, "")
		}
		if u.Failure != "" {
			rec.Set(chatFieldFailure, u.Failure)
		}
		if u.EditorRevision != "" {
			rec.Set(chatFieldEditorRevision, u.EditorRevision)
		}
		if !u.ExpiresAt.IsZero() {
			rec.Set(chatFieldExpiresAt, chatTime(u.ExpiresAt))
		}
		if u.Hunks != nil {
			if err := chatSetHunks(rec, u.Hunks); err != nil {
				return err
			}
		}
		if u.ClearDecided {
			rec.Set(chatFieldDecidedAt, time.Time{})
		}
		if u.Decided {
			rec.Set(chatFieldDecidedAt, ts)
		}
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = chatProposalFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Proposal{}, err
	}
	return out, nil
}

// ReviseProposal replaces a pending proposal's arguments under a revision
// compare-and-swap, for hosts that allow editing before a decision.
func (s *ChatStore) ReviseProposal(ctx context.Context, owner chat.OwnerKey, id string, revision int, args json.RawMessage, argsHash string) (chat.Proposal, error) {
	var out chat.Proposal
	err := s.chatRunTx(func(tx core.App) error {
		rec, err := chatLockAndFind(tx, s.proposals, owner, id)
		if err != nil {
			return err
		}
		if chat.ProposalState(rec.GetString(chatFieldState)) != chat.ProposalStatePending || rec.GetInt(chatFieldRevision) != revision {
			return chat.ErrConflict
		}
		canonical, hash, err := chat.HashArguments(args)
		if err != nil {
			return err
		}
		if argsHash != "" && argsHash != hash {
			return fmt.Errorf("chat: proposal arguments hash does not match: %w", chat.ErrConflict)
		}
		if err := chatSetJSON(rec, chatFieldArguments, canonical); err != nil {
			return err
		}
		rec.Set(chatFieldArgumentsHash, hash)
		// A refusal or warning described the old arguments.
		rec.Set(chatFieldFailure, "")
		if err := chatSetJSON(rec, chatFieldWarnings, nil); err != nil {
			return err
		}
		rec.Set(chatFieldRevision, revision+1)
		rec.Set(chatFieldUpdatedAt, chatNow())
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = chatProposalFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Proposal{}, err
	}
	return out, nil
}

// ExpireProposals ends every undecided proposal whose deadline passed, in this
// store's scope. It is one conditional UPDATE, so a proposal decided
// concurrently is never overwritten: the row only changes while it is still
// pending or approved.
func (s *ChatStore) ExpireProposals(_ context.Context, at time.Time) (int, error) {
	query := "UPDATE {{" + s.proposals + "}} SET " +
		"[[state]] = {:expired}, [[revision]] = [[revision]] + 1, [[updatedAt]] = {:updatedAt}" +
		" WHERE ([[state]] = {:pending} OR [[state]] = {:approved})" +
		" AND [[expiresAt]] != '' AND [[expiresAt]] <= {:now}"
	params := dbx.Params{
		"expired":   string(chat.ProposalStateExpired),
		"pending":   string(chat.ProposalStatePending),
		"approved":  string(chat.ProposalStateApproved),
		"now":       chatDateParam(chatTime(at)),
		"updatedAt": chatDateParam(chatTime(at)),
	}
	if s.namespace != "" {
		query += " AND [[namespace]] = {:namespace} AND [[mount]] = {:mount}"
		params[chatFieldNamespace] = s.namespace
		params[chatFieldMount] = s.mount
	}
	return chatBulkUpdate(s.app, query, params)
}

// RecoverExecuting marks every proposal that was executing at restart as
// unknown, in this store's scope: the effect may have happened, so nothing
// reruns and the user has to reconcile it.
func (s *ChatStore) RecoverExecuting(_ context.Context) (int, error) {
	query := "UPDATE {{" + s.proposals + "}} SET " +
		"[[state]] = {:unknown}, [[revision]] = [[revision]] + 1, [[updatedAt]] = {:updatedAt}, [[failure]] = {:failure}" +
		" WHERE [[state]] = {:executing}"
	params := dbx.Params{
		"unknown":   string(chat.ProposalStateUnknown),
		"executing": string(chat.ProposalStateExecuting),
		"updatedAt": chatDateParam(chatNow()),
		"failure":   chat.UnknownOutcomeMessage,
	}
	if s.namespace != "" {
		query += " AND [[namespace]] = {:namespace} AND [[mount]] = {:mount}"
		params[chatFieldNamespace] = s.namespace
		params[chatFieldMount] = s.mount
	}
	return chatBulkUpdate(s.app, query, params)
}

// chatBulkUpdate runs one conditional update and reports how many rows it
// changed. The table and column names come from the provisioned schema, never
// from user input; every value is bound.
func chatBulkUpdate(app core.App, query string, params dbx.Params) (int, error) {
	db := app.NonconcurrentDB()
	if db == nil {
		// The app was already closed (a sweep racing process termination).
		return 0, errors.New("chat store: database is closed")
	}
	res, err := db.NewQuery(query).Bind(params).Execute()
	if err != nil {
		return 0, fmt.Errorf("chat store: proposal sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("chat store: proposal sweep count: %w", err)
	}
	return int(n), nil
}
