package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// planningTools is a ToolExecutor that also plans calls. Planning is recorded,
// and Call is recorded by the embedded fakeTools, so a test can prove a planned
// call never ran.
type planningTools struct {
	fakeTools

	mu       sync.Mutex
	planFn   func(ctx context.Context, name string, args json.RawMessage) (*ProposalDraft, error)
	planned  []toolCallRecord
	planCall int
}

func (p *planningTools) Plan(ctx context.Context, name string, args json.RawMessage) (*ProposalDraft, error) {
	p.mu.Lock()
	p.planned = append(p.planned, toolCallRecord{name: name, args: cloneRaw(args)})
	p.planCall++
	fn := p.planFn
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, name, args)
	}
	return nil, nil
}

func (p *planningTools) planCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.planCall
}

func draftFor(name string, args json.RawMessage) *ProposalDraft {
	return &ProposalDraft{
		Tool:             name,
		ToolFingerprint:  "sha256:tool-fingerprint",
		ManifestRevision: "sha256:manifest-revision",
		Location:         ProposalLocationServer,
		Target:           "deals/r1",
		Arguments:        args,
		Summary:          "Create a record in deals",
	}
}

// failingProposalStore is a ProposalStore whose creates always fail, which lets
// a test prove the runner ends such a turn as failed rather than pretending the
// proposal was stored.
type failingProposalStore struct {
	ProposalStore
	err error
}

func (f failingProposalStore) CreateProposal(context.Context, OwnerKey, Proposal) (Proposal, error) {
	return Proposal{}, f.err
}

func TestRunnerProposalInsteadOfCall(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "proposals")
	fixed := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"b":2,"a":1}`)}
	prov := scripted.New(toolCallStep(call), textStep("I queued it for review."))
	runner := mustRunner(t, RunnerConfig{
		Store:       store,
		Provider:    prov,
		Model:       "m",
		Proposals:   store,
		ProposalTTL: time.Hour,
		Now:         func() time.Time { return fixed },
	})
	tools := &planningTools{
		fakeTools: fakeTools{err: errors.New("Call must not run for a planned write")},
		planFn: func(_ context.Context, name string, args json.RawMessage) (*ProposalDraft, error) {
			return draftFor(name, args), nil
		},
	}

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "create a deal", "key", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	final := awaitTerminal(t, store, owner, turn.ID)
	if final.State != TurnStateCompleted {
		t.Fatalf("state = %s, want completed (%+v)", final.State, final.Error)
	}

	events := mustEvents(t, store, owner, turn.ID)
	want := []EventType{
		EventTurnQueued, EventTurnStarted,
		EventToolStarted, EventProposalCreated, EventToolCompleted,
		EventTextDelta, EventTurnCompleted,
	}
	got := typesOf(events)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}

	created := decodeEvent[ProposalEventData](t, events[3])
	p := created.Proposal
	if p.State != ProposalStatePending || p.Revision != 1 {
		t.Fatalf("proposal state = %s rev %d", p.State, p.Revision)
	}
	if p.ConversationID != conv.ID || p.TurnID != turn.ID || p.CallID != "call-1" {
		t.Fatalf("proposal identity = %+v", p)
	}
	if p.Tool != "create_deals" || p.ToolFingerprint != "sha256:tool-fingerprint" || p.ManifestRevision != "sha256:manifest-revision" {
		t.Fatalf("proposal tool = %+v", p)
	}
	if p.Location != ProposalLocationServer || p.Target != "deals/r1" || p.Summary != "Create a record in deals" {
		t.Fatalf("proposal effect = %+v", p)
	}
	if string(p.Arguments) != `{"a":1,"b":2}` {
		t.Fatalf("proposal arguments = %s, want canonical form", p.Arguments)
	}
	if _, hash, err := HashArguments(p.Arguments); err != nil || hash != p.ArgumentsHash {
		t.Fatalf("proposal hash = %q (%v)", p.ArgumentsHash, err)
	}
	if !p.ExpiresAt.Equal(fixed.Add(time.Hour)) {
		t.Fatalf("expiresAt = %v, want %v", p.ExpiresAt, fixed.Add(time.Hour))
	}

	completed := decodeEvent[ToolCompleted](t, events[4])
	var result struct {
		Status     string `json:"status"`
		ProposalID string `json:"proposalId"`
		Summary    string `json:"summary"`
		Note       string `json:"note"`
	}
	if err := json.Unmarshal(completed.Result, &result); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if result.Status != "pending_review" || result.ProposalID != p.ID || result.Summary != p.Summary {
		t.Fatalf("tool result = %+v", result)
	}
	if !strings.Contains(result.Note, "Not applied") || !strings.Contains(result.Note, "approve") {
		t.Fatalf("tool result note = %q", result.Note)
	}

	// The executor's Call never ran, and the model got the pending result as a
	// non-error tool result.
	if calls := tools.recorded(); len(calls) != 0 {
		t.Fatalf("Call ran for a planned write: %+v", calls)
	}
	if tools.planCount() != 1 {
		t.Fatalf("Plan calls = %d, want 1", tools.planCount())
	}
	msgs := drainMessages(t, store, owner, conv.ID)
	toolMsg := msgs[len(msgs)-2]
	if toolMsg.Role != RoleTool || toolMsg.Parts[0].IsError {
		t.Fatalf("tool message = %+v", toolMsg)
	}
	if string(toolMsg.Parts[0].Result) != string(completed.Result) {
		t.Fatalf("stored tool result %s != event result %s", toolMsg.Parts[0].Result, completed.Result)
	}

	// The client reducer reconstructs the proposal from the stream alone.
	var view TurnView
	for _, e := range events {
		if err := view.Apply(e); err != nil {
			t.Fatalf("reducer rejected %s: %v", e.Type, err)
		}
	}
	if view.State != TurnStateCompleted {
		t.Fatalf("reduced state = %s", view.State)
	}
	if len(view.Proposals) != 1 || view.Proposals[0].ID != p.ID || view.Proposals[0].State != ProposalStatePending {
		t.Fatalf("reduced proposals = %+v", view.Proposals)
	}

	stored, _, err := store.ListProposals(context.Background(), owner, conv.ID, []ProposalState{ProposalStatePending}, Page{})
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(stored) != 1 || stored[0].ID != p.ID {
		t.Fatalf("stored proposals = %+v", stored)
	}
}

func TestRunnerProposalPlanErrorsFailTheCall(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{}`)}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"tool error is user-safe", ToolError{Message: "Plan says no."}, "Plan says no."},
		{"internal error is generic", errors.New("driver detail that must not leak"), toolGenericFailure},
		{"pointer tool error", &ToolError{Message: "Pointer plan says no."}, "Pointer plan says no."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := NewMemStore(MemStoreOptions{})
			owner := ownerFixture()
			conv := mustConversation(t, store, owner, "plan error")
			prov := scripted.New(toolCallStep(call), textStep("understood"))
			runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Proposals: store})
			tools := &planningTools{planFn: func(context.Context, string, json.RawMessage) (*ProposalDraft, error) {
				return nil, c.err
			}}

			turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "go", "key", tools, time.Time{})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if final := awaitTerminal(t, store, owner, turn.ID); final.State != TurnStateCompleted {
				t.Fatalf("state = %s (%+v)", final.State, final.Error)
			}

			events := mustEvents(t, store, owner, turn.ID)
			if got := typesOf(events); !containsType(got, EventToolFailed) || containsType(got, EventProposalCreated) {
				t.Fatalf("events = %v", got)
			}
			failed := decodeEvent[ToolFailed](t, eventOfType(t, events, EventToolFailed))
			if failed.Message != c.want {
				t.Fatalf("failure message = %q, want %q", failed.Message, c.want)
			}
			if len(tools.recorded()) != 0 {
				t.Fatalf("Call ran after a plan error: %+v", tools.recorded())
			}
			msgs := drainMessages(t, store, owner, conv.ID)
			toolMsg := msgs[len(msgs)-2]
			if toolMsg.Role != RoleTool || !toolMsg.Parts[0].IsError {
				t.Fatalf("tool message = %+v", toolMsg)
			}
			if _, _, err := store.ListProposals(context.Background(), owner, conv.ID, nil, Page{}); err != nil {
				t.Fatalf("ListProposals: %v", err)
			}
		})
	}
}

func TestRunnerProposalWithoutStoreFailsTheCall(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "no store")
	call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{}}`)}
	prov := scripted.New(toolCallStep(call), textStep("understood"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	tools := &planningTools{planFn: func(_ context.Context, name string, args json.RawMessage) (*ProposalDraft, error) {
		return draftFor(name, args), nil
	}}

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "go", "key", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if final := awaitTerminal(t, store, owner, turn.ID); final.State != TurnStateCompleted {
		t.Fatalf("state = %s (%+v)", final.State, final.Error)
	}
	events := mustEvents(t, store, owner, turn.ID)
	failed := decodeEvent[ToolFailed](t, eventOfType(t, events, EventToolFailed))
	if failed.Message != proposalReviewDisabledMessage {
		t.Fatalf("failure message = %q", failed.Message)
	}
	if len(tools.recorded()) != 0 {
		t.Fatalf("a planned write ran without a proposal store: %+v", tools.recorded())
	}
	if containsType(typesOf(events), EventProposalCreated) {
		t.Fatal("a proposal was reported without a store")
	}
}

func TestRunnerProposalDefaultsAndStoreFailure(t *testing.T) {
	t.Run("default TTL is 24 hours", func(t *testing.T) {
		store := NewMemStore(MemStoreOptions{})
		owner := ownerFixture()
		conv := mustConversation(t, store, owner, "ttl")
		fixed := time.Date(2026, 11, 18, 8, 30, 0, 0, time.UTC)
		call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{}}`)}
		runner := mustRunner(t, RunnerConfig{
			Store:     store,
			Provider:  scripted.New(toolCallStep(call), textStep("ok")),
			Model:     "m",
			Proposals: store,
			Now:       func() time.Time { return fixed },
		})
		tools := &planningTools{planFn: func(_ context.Context, name string, args json.RawMessage) (*ProposalDraft, error) {
			return draftFor(name, args), nil
		}}
		if _, _, err := runner.Submit(context.Background(), owner, conv.ID, "go", "key", tools, time.Time{}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		deadline := time.Now().Add(testWait)
		for {
			items, _, err := store.ListProposals(context.Background(), owner, conv.ID, nil, Page{})
			if err != nil {
				t.Fatalf("ListProposals: %v", err)
			}
			if len(items) == 1 {
				if want := fixed.Add(defaultProposalTTL); !items[0].ExpiresAt.Equal(want) {
					t.Fatalf("expiresAt = %v, want %v", items[0].ExpiresAt, want)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("proposal was not stored")
			}
			time.Sleep(2 * time.Millisecond)
		}
	})

	t.Run("store failure ends the turn as internal", func(t *testing.T) {
		store := NewMemStore(MemStoreOptions{})
		owner := ownerFixture()
		conv := mustConversation(t, store, owner, "store failure")
		call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{}}`)}
		runner := mustRunner(t, RunnerConfig{
			Store:     store,
			Provider:  scripted.New(toolCallStep(call), textStep("ok")),
			Model:     "m",
			Proposals: failingProposalStore{ProposalStore: store, err: errors.New("store down")},
		})
		tools := &planningTools{planFn: func(_ context.Context, name string, args json.RawMessage) (*ProposalDraft, error) {
			return draftFor(name, args), nil
		}}
		turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "go", "key", tools, time.Time{})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		final := awaitTerminal(t, store, owner, turn.ID)
		if final.State != TurnStateFailed || final.Error == nil || final.Error.Code != FailureInternal {
			t.Fatalf("state = %s, error = %+v", final.State, final.Error)
		}
		if len(tools.recorded()) != 0 {
			t.Fatalf("a planned write ran after a store failure: %+v", tools.recorded())
		}
	})
}

func TestRunnerNilPlannerKeepsCallPath(t *testing.T) {
	// A read-only executor has no Plan at all; the existing call path must stay
	// byte-identical. This guards the ProposalPlanner assertion.
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "read")
	call := provider.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}
	prov := scripted.New(toolCallStep(call), textStep("done"))
	tools := &fakeTools{result: json.RawMessage(`{"ok":true}`)}
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Proposals: store})

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "go", "key", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if final := awaitTerminal(t, store, owner, turn.ID); final.State != TurnStateCompleted {
		t.Fatalf("state = %s (%+v)", final.State, final.Error)
	}
	if calls := tools.recorded(); len(calls) != 1 || calls[0].name != "echo" {
		t.Fatalf("executor calls = %+v", calls)
	}
	if got := typesOf(mustEvents(t, store, owner, turn.ID)); containsType(got, EventProposalCreated) {
		t.Fatalf("a read call created a proposal: %v", got)
	}
}

func containsType(types []EventType, want EventType) bool {
	for _, typ := range types {
		if typ == want {
			return true
		}
	}
	return false
}

func eventOfType(t *testing.T, events []Event, want EventType) Event {
	t.Helper()
	for _, e := range events {
		if e.Type == want {
			return e
		}
	}
	t.Fatalf("no %s event in %v", want, typesOf(events))
	return Event{}
}
