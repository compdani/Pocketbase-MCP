package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// fakeProposalTools is a ToolExecutor that also plans, checks, revises and
// executes proposals. Every entry point is counted, so a test can prove a
// decision ran nothing or ran exactly once.
type fakeProposalTools struct {
	fakeTools

	plan    *ProposalDraft
	planErr error
	checkFn func(ctx context.Context, p Proposal) error
	execFn  func(ctx context.Context, p Proposal) (json.RawMessage, error)
	revise  func(ctx context.Context, p Proposal, args json.RawMessage) (json.RawMessage, string, error)

	mu        sync.Mutex
	planned   int
	checks    int
	execs     int
	revisions int
	executed  []Proposal
	revised   []json.RawMessage
	started   chan struct{}
	startOnce sync.Once
}

var _ ToolExecutor = (*fakeProposalTools)(nil)
var _ ProposalPlanner = (*fakeProposalTools)(nil)
var _ ProposalExecutor = (*fakeProposalTools)(nil)

func (f *fakeProposalTools) Plan(_ context.Context, name string, args json.RawMessage) (*ProposalDraft, error) {
	f.mu.Lock()
	f.planned++
	plan, planErr := f.plan, f.planErr
	f.mu.Unlock()
	if planErr != nil {
		return nil, planErr
	}
	if plan == nil {
		return nil, nil
	}
	draft := *plan
	draft.Tool = name
	draft.Arguments = cloneRaw(args)
	return &draft, nil
}

func (f *fakeProposalTools) Check(ctx context.Context, p Proposal) error {
	f.mu.Lock()
	f.checks++
	fn := f.checkFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, p)
	}
	return nil
}

func (f *fakeProposalTools) Execute(ctx context.Context, p Proposal) (json.RawMessage, error) {
	f.mu.Lock()
	f.execs++
	f.executed = append(f.executed, p)
	fn := f.execFn
	started := f.started
	f.mu.Unlock()
	if started != nil {
		f.startOnce.Do(func() { close(started) })
	}
	if fn != nil {
		return fn(ctx, p)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (f *fakeProposalTools) Revise(ctx context.Context, p Proposal, args json.RawMessage) (json.RawMessage, string, error) {
	f.mu.Lock()
	f.revisions++
	f.revised = append(f.revised, cloneRaw(args))
	fn := f.revise
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, p, args)
	}
	canonical, hash, err := HashArguments(args)
	if err != nil {
		return nil, "", ToolError{Message: "Invalid tool arguments."}
	}
	return canonical, hash, nil
}

func (f *fakeProposalTools) counts() (checks, executions, revisions int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, f.execs, f.revisions
}

func (f *fakeProposalTools) executedProposals() []Proposal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Proposal(nil), f.executed...)
}

// serverProposal is a pending server proposal of the shape a runner would store
// for a generated create.
func serverProposal(conversationID string) Proposal {
	return Proposal{
		ConversationID:   conversationID,
		Location:         ProposalLocationServer,
		Tool:             "create_deals",
		ToolFingerprint:  "sha256:tool",
		ManifestRevision: "sha256:manifest",
		Target:           "deals",
		Arguments:        json.RawMessage(`{"data":{"name":"Acme"}}`),
		Summary:          "Create a record in deals",
		ExpiresAt:        time.Now().Add(time.Hour),
	}
}

func mustProposal(t *testing.T, s ProposalStore, owner OwnerKey, p Proposal) Proposal {
	t.Helper()
	stored, err := s.CreateProposal(context.Background(), owner, p)
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	return stored
}

// newProposalFixture returns a fixture with proposals on and a fake proposal
// executor bound to user-a.
func newProposalFixture(t *testing.T, opts fixtureOptions) (*apiFixture, *fakeProposalTools) {
	t.Helper()
	opts.proposals = true
	f := newFixture(t, opts)
	exec := &fakeProposalTools{started: make(chan struct{})}
	f.auth.setExecutor("user-a", exec)
	return f, exec
}

func (f *apiFixture) decide(id, decision string, revision int, extra map[string]any) apiResponse {
	f.t.Helper()
	body := map[string]any{"decision": decision, "revision": revision}
	for k, v := range extra {
		body[k] = v
	}
	return f.call(http.MethodPost, testPrefix+"/proposals/"+id+"/decision", body, nil)
}

func (f *apiFixture) proposal(id string) Proposal {
	f.t.Helper()
	res := f.call(http.MethodGet, testPrefix+"/proposals/"+id, nil, nil).expect(http.StatusOK)
	var view proposalView
	res.decode(&view)
	return view.Proposal
}

// proposalDetail is the conversation detail as the proposal-enabled handler
// writes it.
type proposalDetail struct {
	Conversation Conversation `json:"conversation"`
	Messages     []Message    `json:"messages"`
	NextCursor   string       `json:"nextCursor"`
	ActiveTurn   *Turn        `json:"activeTurn"`
	Proposals    []Proposal   `json:"proposals"`
}

func (f *apiFixture) conversationProposals(id string) []Proposal {
	f.t.Helper()
	res := f.call(http.MethodGet, testPrefix+"/conversations/"+id, nil, nil).expect(http.StatusOK)
	var out proposalDetail
	res.decode(&out)
	return out.Proposals
}

func (f *apiFixture) messages(owner OwnerKey, conversationID string) []Message {
	f.t.Helper()
	var all []Message
	page := Page{Limit: maxPageLimit}
	for {
		items, next, err := f.store.ListMessages(context.Background(), owner, conversationID, page)
		if err != nil {
			f.t.Fatalf("ListMessages: %v", err)
		}
		all = append(all, items...)
		if next == "" {
			return all
		}
		page.Cursor = next
	}
}

func TestProposalDecisionApproveExecutesOnce(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

	if got := f.proposal(p.ID); got.ID != p.ID || got.State != ProposalStatePending {
		t.Fatalf("GET proposal = %+v", got)
	}

	res := f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusOK)
	var out decisionView
	res.decode(&out)
	if out.Replayed {
		t.Fatal("the first decision reported a replay")
	}
	if out.Proposal.State != ProposalStateSucceeded {
		t.Fatalf("state = %s, want succeeded (%s)", out.Proposal.State, out.Proposal.Failure)
	}
	if string(out.Proposal.Result) != `{"ok":true}` {
		t.Fatalf("result = %s", out.Proposal.Result)
	}
	if out.Proposal.DecidedAt == nil || out.Proposal.DecidedAt.IsZero() {
		t.Fatal("a decided proposal carries no decision time")
	}
	if out.Proposal.Revision <= p.Revision {
		t.Fatalf("revision = %d, want more than %d", out.Proposal.Revision, p.Revision)
	}

	checks, executions, revisions := exec.counts()
	if checks != 1 || executions != 1 || revisions != 0 {
		t.Fatalf("checks = %d, executions = %d, revisions = %d, want 1/1/0", checks, executions, revisions)
	}
	if stored := f.proposal(p.ID); stored.State != ProposalStateSucceeded {
		t.Fatalf("stored state = %s", stored.State)
	}

	// The decision leaves one framework note behind, so the next turn's model
	// knows what happened.
	msgs := f.messages(owner, conv.ID)
	if len(msgs) != 1 {
		t.Fatalf("transcript has %d messages, want the one notice", len(msgs))
	}
	notice := msgs[0]
	if notice.Role != RoleNotice || notice.TurnID != "" {
		t.Fatalf("notice message = %+v", notice)
	}
	part := notice.Parts[0]
	if part.Type != PartProposalOutcome || part.ProposalID != p.ID || part.State != string(ProposalStateSucceeded) || part.IsError {
		t.Fatalf("notice part = %+v", part)
	}
	if !strings.Contains(part.Text, "approved and applied. Result: "+`{"ok":true}`) {
		t.Fatalf("notice text = %q", part.Text)
	}
	if string(part.Result) != `{"ok":true}` {
		t.Fatalf("notice result = %s", part.Result)
	}

	// Approving again is a replay: nothing runs and the stored state returns.
	res = f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusOK)
	var replay decisionView
	res.decode(&replay)
	if !replay.Replayed || replay.Proposal.State != ProposalStateSucceeded || replay.Proposal.Revision != out.Proposal.Revision {
		t.Fatalf("replay = %+v", replay)
	}
	if _, executions, _ := exec.counts(); executions != 1 {
		t.Fatalf("executions = %d after replay, want 1", executions)
	}
}

func TestProposalDecisionConcurrentApproveRunsOnce(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	release := make(chan struct{})
	exec.execFn = func(ctx context.Context, _ Proposal) (json.RawMessage, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

	const decisions = 20
	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make([]result, decisions)
	var wg sync.WaitGroup
	wg.Add(decisions)
	for i := 0; i < decisions; i++ {
		go func(i int) {
			defer wg.Done()
			status, body, err := f.decideAsync(p.ID, p.Revision)
			results[i] = result{status: status, body: body, err: err}
		}(i)
	}

	// Wait until one decision reached the executor, then let it finish.
	select {
	case <-exec.started:
	case <-time.After(testWait):
		t.Fatal("no decision reached the executor")
	}
	close(release)
	wg.Wait()

	fresh, replays := 0, 0
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("decision %d: %v", i, res.err)
		}
		if res.status != http.StatusOK {
			t.Fatalf("decision %d: status %d, body %s", i, res.status, res.body)
		}
		var view decisionView
		if err := json.Unmarshal(res.body, &view); err != nil {
			t.Fatalf("decision %d: decode: %v", i, err)
		}
		if view.Proposal.ID != p.ID {
			t.Fatalf("decision %d: proposal %q", i, view.Proposal.ID)
		}
		switch view.Proposal.State {
		case ProposalStateExecuting, ProposalStateSucceeded:
		default:
			t.Fatalf("decision %d: state %s", i, view.Proposal.State)
		}
		if view.Replayed {
			replays++
		} else {
			fresh++
		}
	}
	if _, executions, _ := exec.counts(); executions != 1 {
		t.Fatalf("Execute ran %d times, want exactly 1", executions)
	}
	if fresh != 1 || replays != decisions-1 {
		t.Fatalf("fresh decisions = %d, replays = %d", fresh, replays)
	}

	stored := f.proposal(p.ID)
	if stored.State != ProposalStateSucceeded {
		t.Fatalf("stored state = %s", stored.State)
	}
	if stored.Revision != p.Revision+2 {
		t.Fatalf("revision = %d, want %d (one transition for each stored change)", stored.Revision, p.Revision+2)
	}
	msgs := f.messages(owner, conv.ID)
	notices := 0
	for _, m := range msgs {
		if m.Role == RoleNotice {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("transcript holds %d notices, want exactly 1", notices)
	}
}

// decideAsync issues one approve decision without touching testing.T, so it is
// safe from several goroutines at once.
func (f *apiFixture) decideAsync(id string, revision int) (int, []byte, error) {
	body := fmt.Sprintf(`{"decision":"approve","revision":%d}`, revision)
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+testPrefix+"/proposals/"+id+"/decision", strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", tokenA)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.srv.Client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	return res.StatusCode, raw, err
}

func TestProposalDecisionReject(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

	res := f.decide(p.ID, "reject", p.Revision, nil).expect(http.StatusOK)
	var out decisionView
	res.decode(&out)
	if out.Proposal.State != ProposalStateRejected || out.Replayed {
		t.Fatalf("reject = %+v", out)
	}
	if _, executions, _ := exec.counts(); executions != 0 {
		t.Fatalf("a rejected proposal executed %d times", executions)
	}

	msgs := f.messages(owner, conv.ID)
	part := msgs[len(msgs)-1].Parts[0]
	if part.Type != PartProposalOutcome || part.State != string(ProposalStateRejected) || !strings.Contains(part.Text, "rejected by the user") {
		t.Fatalf("reject notice = %+v", part)
	}

	// The same decision twice is idempotent.
	res = f.decide(p.ID, "reject", p.Revision, nil).expect(http.StatusOK)
	var replay decisionView
	res.decode(&replay)
	if !replay.Replayed || replay.Proposal.State != ProposalStateRejected {
		t.Fatalf("reject replay = %+v", replay)
	}

	// Approving a rejected proposal is a conflict, and still runs nothing.
	conflict := f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusConflict)
	var errBody proposalErrorView
	conflict.decode(&errBody)
	if errBody.Error.Code != CodeConflict {
		t.Fatalf("code = %q, want %q", errBody.Error.Code, CodeConflict)
	}
	if errBody.Proposal == nil || errBody.Proposal.State != ProposalStateRejected {
		t.Fatalf("conflict body = %+v", errBody)
	}
	if _, executions, _ := exec.counts(); executions != 0 {
		t.Fatalf("executions = %d after a conflict", executions)
	}

	// Rejecting an approved proposal is a conflict too.
	other := mustProposal(t, f.store, owner, serverProposal(conv.ID))
	f.decide(other.ID, "approve", other.Revision, nil).expect(http.StatusOK)
	late := f.decide(other.ID, "reject", other.Revision, nil).expect(http.StatusConflict)
	var lateBody proposalErrorView
	late.decode(&lateBody)
	if lateBody.Error.Code != CodeConflict || lateBody.Proposal == nil || lateBody.Proposal.State != ProposalStateSucceeded {
		t.Fatalf("late reject = %+v", lateBody)
	}
}

func TestProposalDecisionValidation(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, serverProposal(conv.ID))
	path := testPrefix + "/proposals/" + p.ID + "/decision"

	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"missing decision", map[string]any{"revision": 1}, http.StatusBadRequest, CodeInvalidRequest},
		{"unknown decision", map[string]any{"decision": "maybe", "revision": 1}, http.StatusBadRequest, CodeInvalidRequest},
		{"missing revision", map[string]any{"decision": "approve"}, http.StatusBadRequest, CodeInvalidRequest},
		{"zero revision", map[string]any{"decision": "approve", "revision": 0}, http.StatusBadRequest, CodeInvalidRequest},
		{"unknown field", map[string]any{"decision": "approve", "revision": 1, "extra": true}, http.StatusBadRequest, CodeInvalidRequest},
		{"no body", "", http.StatusBadRequest, CodeInvalidRequest},
		{"oversized", `{"decision":"approve","revision":1,"arguments":"` + strings.Repeat("a", 70<<10) + `"}`, http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := f.with(t).call(http.MethodPost, path, tc.body, nil).expect(tc.status)
			if got := res.errorCode(); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}

	// Stale revision is a conflict that returns the stored proposal.
	res := f.decide(p.ID, "approve", p.Revision+5, nil).expect(http.StatusConflict)
	var stale proposalErrorView
	res.decode(&stale)
	if stale.Error.Code != CodeConflict || stale.Proposal == nil || stale.Proposal.Revision != p.Revision || stale.Proposal.State != ProposalStatePending {
		t.Fatalf("stale revision = %+v", stale)
	}

	// A missing proposal is 404, and nothing ran.
	f.call(http.MethodGet, testPrefix+"/proposals/000000000000000", nil, nil).expect(http.StatusNotFound)
	f.decide("000000000000000", "approve", 1, nil).expect(http.StatusNotFound)
	if _, executions, _ := exec.counts(); executions != 0 {
		t.Fatalf("executions = %d", executions)
	}
}

func TestProposalDecisionExpired(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := serverProposal(conv.ID)
	p.ExpiresAt = time.Now().Add(-time.Minute)
	stored := mustProposal(t, f.store, owner, p)

	res := f.decide(stored.ID, "approve", stored.Revision, nil).expect(http.StatusConflict)
	var body proposalErrorView
	res.decode(&body)
	if body.Error.Code != CodeConflict {
		t.Fatalf("code = %q, want %q", body.Error.Code, CodeConflict)
	}
	if body.Proposal == nil || body.Proposal.State != ProposalStateExpired {
		t.Fatalf("body = %+v", body)
	}
	if got := f.proposal(stored.ID); got.State != ProposalStateExpired {
		t.Fatalf("stored state = %s, want expired", got.State)
	}
	if _, executions, _ := exec.counts(); executions != 0 {
		t.Fatalf("an expired proposal executed %d times", executions)
	}
	if _, err := f.store.GetProposal(context.Background(), owner, stored.ID); err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
}

func TestProposalDecisionArgumentEdits(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		f, exec := newProposalFixture(t, fixtureOptions{allowEdits: false})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

		res := f.decide(p.ID, "approve", p.Revision, map[string]any{"arguments": map[string]any{"data": map[string]any{"name": "Edited"}}}).
			expect(http.StatusBadRequest)
		if got := res.errorCode(); got != CodeInvalidRequest {
			t.Fatalf("code = %q", got)
		}
		if checks, executions, _ := exec.counts(); checks != 0 || executions != 0 {
			t.Fatalf("a refused edit ran the executor: %d checks, %d executions", checks, executions)
		}
		if got := f.proposal(p.ID); got.State != ProposalStatePending {
			t.Fatalf("state = %s, want pending", got.State)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		f, exec := newProposalFixture(t, fixtureOptions{allowEdits: true})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

		edited := map[string]any{"arguments": map[string]any{"data": map[string]any{"name": "Edited"}}}
		res := f.decide(p.ID, "approve", p.Revision, edited).expect(http.StatusOK)
		var out decisionView
		res.decode(&out)
		if out.Proposal.State != ProposalStateSucceeded {
			t.Fatalf("state = %s (%s)", out.Proposal.State, out.Proposal.Failure)
		}
		// The edit is one stored revision, the decision another two.
		if out.Proposal.Revision != p.Revision+3 {
			t.Fatalf("revision = %d, want %d", out.Proposal.Revision, p.Revision+3)
		}
		if string(out.Proposal.Arguments) != `{"data":{"name":"Edited"}}` {
			t.Fatalf("stored arguments = %s", out.Proposal.Arguments)
		}
		if _, _, revisions := exec.counts(); revisions != 1 {
			t.Fatalf("Revise calls = %d", revisions)
		}
		executed := exec.executedProposals()
		if len(executed) != 1 || string(executed[0].Arguments) != `{"data":{"name":"Edited"}}` {
			t.Fatalf("executor saw %+v", executed)
		}
	})

	t.Run("revise refusal is unprocessable", func(t *testing.T) {
		f, exec := newProposalFixture(t, fixtureOptions{allowEdits: true})
		exec.revise = func(context.Context, Proposal, json.RawMessage) (json.RawMessage, string, error) {
			return nil, "", ToolError{Message: "Name is required."}
		}
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

		res := f.decide(p.ID, "approve", p.Revision, map[string]any{"arguments": map[string]any{"data": map[string]any{}}}).
			expect(http.StatusUnprocessableEntity)
		var body errorBody
		res.decode(&body)
		if body.Error.Code != CodeInvalidArguments || body.Error.Message != "Name is required." {
			t.Fatalf("body = %+v", body)
		}
		if _, executions, _ := exec.counts(); executions != 0 {
			t.Fatalf("executions = %d", executions)
		}
	})

	t.Run("revised revision must match", func(t *testing.T) {
		f, _ := newProposalFixture(t, fixtureOptions{allowEdits: true})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

		f.decide(p.ID, "approve", p.Revision+1, map[string]any{"arguments": map[string]any{"data": map[string]any{"name": "Edited"}}}).
			expect(http.StatusConflict)
		if got := f.proposal(p.ID); got.State != ProposalStatePending || string(got.Arguments) != string(p.Arguments) {
			t.Fatalf("proposal changed on a stale revision: %+v", got)
		}
	})
}

func TestProposalDecisionCheckStaleKeepsPending(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	exec.checkFn = func(context.Context, Proposal) error {
		return ToolError{Message: "This change is out of date. Ask for a new proposal."}
	}
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

	res := f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusConflict)
	var body proposalErrorView
	res.decode(&body)
	if body.Error.Code != CodeStale {
		t.Fatalf("code = %q, want %q", body.Error.Code, CodeStale)
	}
	if body.Error.Message != "This change is out of date. Ask for a new proposal." {
		t.Fatalf("message = %q", body.Error.Message)
	}
	if body.Proposal == nil || body.Proposal.State != ProposalStatePending || body.Proposal.Revision != p.Revision {
		t.Fatalf("body = %+v", body)
	}
	if got := f.proposal(p.ID); got.State != ProposalStatePending {
		t.Fatalf("stored state = %s, want pending", got.State)
	}
	if _, executions, _ := exec.counts(); executions != 0 {
		t.Fatalf("a stale proposal executed %d times", executions)
	}

	// The user can still reject it.
	f.decide(p.ID, "reject", p.Revision, nil).expect(http.StatusOK)
}

func TestProposalDecisionExecuteOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     ProposalState
		failure  string
		notice   string
		isNotice bool
	}{
		{
			name:    "tool error is a definite failure",
			err:     ToolError{Message: "The record is locked."},
			want:    ProposalStateFailed,
			failure: "The record is locked.",
			notice:  "failed: The record is locked.",
		},
		{
			name:    "unknown outcome",
			err:     fmt.Errorf("dispatch lost: %w", ErrOutcomeUnknown),
			want:    ProposalStateUnknown,
			failure: UnknownOutcomeMessage,
			notice:  "outcome unknown",
		},
		{
			name:    "generic error is unknown too",
			err:     errors.New("driver detail that must not leak"),
			want:    ProposalStateUnknown,
			failure: UnknownOutcomeMessage,
			notice:  "outcome unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, exec := newProposalFixture(t, fixtureOptions{})
			exec.execFn = func(context.Context, Proposal) (json.RawMessage, error) {
				return nil, tc.err
			}
			owner := ownerFor("user-a")
			conv := f.createConversation(nil)
			p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

			res := f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusOK)
			var out decisionView
			res.decode(&out)
			if out.Proposal.State != tc.want || out.Proposal.Failure != tc.failure {
				t.Fatalf("proposal = %s %q, want %s %q", out.Proposal.State, out.Proposal.Failure, tc.want, tc.failure)
			}
			if bytes.Contains([]byte(out.Proposal.Failure), []byte("driver detail")) {
				t.Fatal("an internal error text reached the proposal")
			}

			msgs := f.messages(owner, conv.ID)
			notice := msgs[len(msgs)-1]
			if notice.Role != RoleNotice || !notice.Parts[0].IsError {
				t.Fatalf("notice = %+v", notice)
			}
			if !strings.Contains(notice.Parts[0].Text, tc.notice) {
				t.Fatalf("notice text = %q, want it to contain %q", notice.Parts[0].Text, tc.notice)
			}

			// A failed or unknown proposal is never rerun by a replay.
			f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusOK)
			if _, executions, _ := exec.counts(); executions != 1 {
				t.Fatalf("executions = %d, want 1", executions)
			}
		})
	}
}

func TestProposalDecisionDetachedFromClientDisconnect(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	release := make(chan struct{})
	exec.execFn = func(ctx context.Context, _ Proposal) (json.RawMessage, error) {
		<-release
		if ctx.Err() != nil {
			// The client is gone; the execution context must still be alive.
			return nil, fmt.Errorf("execution was cancelled: %w", ErrOutcomeUnknown)
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

	reqCtx, cancel := context.WithCancel(context.Background())
	body := fmt.Sprintf(`{"decision":"approve","revision":%d}`, p.Revision)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, f.srv.URL+testPrefix+"/proposals/"+p.ID+"/decision", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", tokenA)

	done := make(chan error, 1)
	go func() {
		res, err := f.srv.Client().Do(req)
		if err == nil {
			res.Body.Close()
		}
		done <- err
	}()

	select {
	case <-exec.started:
	case <-time.After(testWait):
		t.Fatal("the decision never reached the executor")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatal("the disconnected request did not return")
	}

	// The detached execution finishes and records its outcome even though the
	// client is gone.
	close(release)
	deadline := time.Now().Add(testWait)
	for {
		stored, err := f.store.GetProposal(context.Background(), owner, p.ID)
		if err != nil {
			t.Fatalf("GetProposal: %v", err)
		}
		if stored.State == ProposalStateSucceeded {
			if string(stored.Result) != `{"ok":true}` {
				t.Fatalf("result = %s", stored.Result)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("proposal stayed %s after the client disconnected", stored.State)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProposalDecisionOwnership(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, serverProposal(conv.ID))
	missing := "000000000000000"

	otherGet := f.callAs(tokenB, http.MethodGet, testPrefix+"/proposals/"+p.ID, nil, nil).expect(http.StatusNotFound)
	absentGet := f.call(http.MethodGet, testPrefix+"/proposals/"+missing, nil, nil).expect(http.StatusNotFound)
	if !bytes.Equal(otherGet.body, absentGet.body) {
		t.Fatalf("other-owner body %s differs from missing-record body %s", otherGet.body, absentGet.body)
	}

	body := map[string]any{"decision": "approve", "revision": p.Revision}
	other := f.callAs(tokenB, http.MethodPost, testPrefix+"/proposals/"+p.ID+"/decision", body, nil).expect(http.StatusNotFound)
	absent := f.call(http.MethodPost, testPrefix+"/proposals/"+missing+"/decision", body, nil).expect(http.StatusNotFound)
	if !bytes.Equal(other.body, absent.body) {
		t.Fatalf("other-owner decision body %s differs from %s", other.body, absent.body)
	}
	if checks, executions, _ := exec.counts(); checks != 0 || executions != 0 {
		t.Fatalf("another owner reached the executor: %d checks, %d executions", checks, executions)
	}
	if got := f.proposal(p.ID); got.State != ProposalStatePending {
		t.Fatalf("state = %s", got.State)
	}
}

func TestProposalDecisionUnsupported(t *testing.T) {
	t.Run("executor without proposal support", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{proposals: true})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		stored := mustProposal(t, f.store, owner, serverProposal(conv.ID))

		res := f.decide(stored.ID, "approve", stored.Revision, nil).expect(http.StatusBadRequest)
		if got := res.errorCode(); got != CodeUnsupported {
			t.Fatalf("code = %q, want %q", got, CodeUnsupported)
		}
		if got := f.proposal(stored.ID); got.State != ProposalStatePending {
			t.Fatalf("state = %s", got.State)
		}
	})
}

func TestProposalUpdatedWhileTurnRuns(t *testing.T) {
	f, _ := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	turn, _, err := f.store.CreateTurn(context.Background(), owner, Turn{ConversationID: conv.ID})
	if err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}
	if _, err := f.store.TransitionTurn(context.Background(), owner, turn.ID, TurnStateQueued, TurnStateRunning, nil); err != nil {
		t.Fatalf("TransitionTurn: %v", err)
	}

	p := serverProposal(conv.ID)
	p.TurnID = turn.ID
	stored := mustProposal(t, f.store, owner, p)
	f.decide(stored.ID, "approve", stored.Revision, nil).expect(http.StatusOK)

	var states []ProposalState
	for _, ev := range mustEvents(t, f.store, owner, turn.ID) {
		if ev.Type != EventProposalUpdated {
			continue
		}
		states = append(states, decodeEvent[ProposalEventData](t, ev).Proposal.State)
	}
	want := []ProposalState{ProposalStateExecuting, ProposalStateSucceeded}
	if len(states) != len(want) || states[0] != want[0] || states[1] != want[1] {
		t.Fatalf("proposal.updated states = %v, want %v", states, want)
	}

	// Once the turn is terminal the event is skipped: the transcript notice is
	// the durable record instead.
	ev, err := NewEvent(conv.ID, turn.ID, EventTurnCompleted, nil)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if _, _, err := f.store.FinishTurn(context.Background(), owner, turn.ID, TurnStateRunning, TurnStateCompleted, ev); err != nil {
		t.Fatalf("FinishTurn: %v", err)
	}
	before := len(mustEvents(t, f.store, owner, turn.ID))

	second := serverProposal(conv.ID)
	second.TurnID = turn.ID
	late := mustProposal(t, f.store, owner, second)
	f.decide(late.ID, "approve", late.Revision, nil).expect(http.StatusOK)
	if after := len(mustEvents(t, f.store, owner, turn.ID)); after != before {
		t.Fatalf("a terminal turn gained %d events", after-before)
	}
}

func TestProposalReplyReachesNextTurnHistory(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{"name":"Acme"}}`)}
	f := newFixture(t, fixtureOptions{
		proposals: true,
		steps:     []scripted.Step{toolCallStep(call), textStep("It is waiting for review."), textStep("Done.")},
	})
	exec := &fakeProposalTools{plan: &ProposalDraft{
		ToolFingerprint:  "sha256:tool",
		ManifestRevision: "sha256:manifest",
		Location:         ProposalLocationServer,
		Target:           "deals",
		Summary:          "Create a record in deals",
	}}
	f.auth.setExecutor("user-a", exec)
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)

	first := f.submitTurn(conv.ID, "create a deal", "", nil)
	awaitTerminal(t, f.store, owner, first.Turn.ID)

	items, _, err := f.store.ListProposals(context.Background(), owner, conv.ID, nil, Page{})
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(items) != 1 || items[0].State != ProposalStatePending {
		t.Fatalf("proposals = %+v", items)
	}
	f.decide(items[0].ID, "approve", items[0].Revision, nil).expect(http.StatusOK)

	second := f.submitTurn(conv.ID, "and now?", "", nil)
	awaitTerminal(t, f.store, owner, second.Turn.ID)

	reqs := f.prov.Requests()
	if len(reqs) != 3 {
		t.Fatalf("provider served %d requests, want 3", len(reqs))
	}
	var notices []string
	for _, msg := range reqs[2].Messages {
		if msg.Role == provider.RoleUser && strings.HasPrefix(msg.Text, "[Proposal "+items[0].ID) {
			notices = append(notices, msg.Text)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("the next turn's history carried %d notices: %+v", len(notices), reqs[2].Messages)
	}
	if !strings.Contains(notices[0], "approved and applied") || !strings.Contains(notices[0], `"ok":true`) {
		t.Fatalf("notice text = %q", notices[0])
	}
}

func TestProposalConversationListExpiresAndFilters(t *testing.T) {
	f, _ := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)

	pending := mustProposal(t, f.store, owner, serverProposal(conv.ID))
	overdue := serverProposal(conv.ID)
	overdue.ExpiresAt = time.Now().Add(-time.Minute)
	expired := mustProposal(t, f.store, owner, overdue)
	rejected := mustProposal(t, f.store, owner, serverProposal(conv.ID))
	f.decide(rejected.ID, "reject", rejected.Revision, nil).expect(http.StatusOK)

	items := f.conversationProposals(conv.ID)
	if len(items) != 1 || items[0].ID != pending.ID {
		t.Fatalf("listed proposals = %+v, want just the pending one", items)
	}
	// Listing is read-only: the overdue proposal is hidden, and the sweep (or
	// the next decision) is what stores it as expired.
	if got := f.proposal(expired.ID); got.State != ProposalStatePending {
		t.Fatalf("listing wrote overdue proposal state = %s, want it untouched", got.State)
	}
	if _, err := f.store.ExpireProposals(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := f.proposal(expired.ID); got.State != ProposalStateExpired {
		t.Fatalf("overdue proposal state after sweep = %s, want expired", got.State)
	}

	// The field is a stable [] when the handler serves proposals.
	res := f.call(http.MethodGet, testPrefix+"/conversations/"+mustConversationID(t, f, owner), nil, nil).expect(http.StatusOK)
	if !bytes.Contains(res.body, []byte(`"proposals":[]`)) {
		t.Fatalf("empty proposal list serialized as %s", res.body)
	}
}

func mustConversationID(t *testing.T, f *apiFixture, owner OwnerKey) string {
	t.Helper()
	conv, err := f.store.CreateConversation(context.Background(), Conversation{Owner: owner})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return conv.ID
}

func TestProposalFeaturesOff(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	stored := mustProposal(t, f.store, owner, serverProposal(conv.ID))

	for _, target := range []string{
		testPrefix + "/proposals/" + stored.ID,
		testPrefix + "/proposals/" + stored.ID + "/decision",
	} {
		method := http.MethodGet
		if strings.HasSuffix(target, "/decision") {
			method = http.MethodPost
		}
		res := f.call(method, target, map[string]any{"decision": "approve", "revision": 1}, nil).expect(http.StatusNotFound)
		if got := res.errorCode(); got != CodeNotFound {
			t.Fatalf("%s: code = %q", target, got)
		}
	}

	res := f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK)
	var st statusView
	res.decode(&st)
	if st.Features.Proposals {
		t.Fatal("proposals reported enabled without a proposal store")
	}

	// The conversation body is the v1.1 shape, with no proposals field at all.
	detail := f.call(http.MethodGet, testPrefix+"/conversations/"+conv.ID, nil, nil).expect(http.StatusOK)
	if bytes.Contains(detail.body, []byte(`"proposals"`)) {
		t.Fatalf("conversation detail gained a proposals field: %s", detail.body)
	}
}

func TestProposalStatusReportsEnabled(t *testing.T) {
	f, _ := newProposalFixture(t, fixtureOptions{})
	res := f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK)
	var st statusView
	res.decode(&st)
	if !st.Features.Proposals {
		t.Fatal("status did not report proposals")
	}
}

// --- editor proposals ---

// editorProposal is a pending editor proposal of the shape a runner would store
// for a chat-only editor tool: it targets a document id and binds the editor
// revision the patch was computed against.
func editorProposal(conversationID string) Proposal {
	return Proposal{
		ConversationID:   conversationID,
		Location:         ProposalLocationEditor,
		Tool:             "edit_section",
		ToolFingerprint:  "sha256:editor",
		ManifestRevision: "sha256:manifest",
		Target:           "draft-1",
		Arguments:        json.RawMessage(`{"baseRevision":"r7","editorId":"draft-1","text":"hello"}`),
		Summary:          "Edit section: edit draft-1 (applies in the editor after approval)",
		ExpiresAt:        time.Now().Add(time.Hour),
	}
}

// outcome reports what the browser editor did with an approved proposal.
func (f *apiFixture) outcome(id string, body any) apiResponse {
	f.t.Helper()
	return f.call(http.MethodPost, testPrefix+"/proposals/"+id+"/outcome", body, nil)
}

// approveEditor approves one editor proposal and returns the stored approval.
func (f *apiFixture) approveEditor(t *testing.T, id string, revision int) Proposal {
	t.Helper()
	res := f.decide(id, "approve", revision, nil).expect(http.StatusOK)
	var out decisionView
	res.decode(&out)
	if out.Proposal.State != ProposalStateApproved {
		t.Fatalf("state = %s, want approved (%s)", out.Proposal.State, out.Proposal.Failure)
	}
	return out.Proposal
}

// lastNotice returns the newest framework notice of a conversation.
func (f *apiFixture) lastNotice(owner OwnerKey, conversationID string) Part {
	f.t.Helper()
	msgs := f.messages(owner, conversationID)
	if len(msgs) == 0 {
		f.t.Fatal("transcript is empty")
	}
	last := msgs[len(msgs)-1]
	if last.Role != RoleNotice || len(last.Parts) != 1 {
		f.t.Fatalf("last message = %+v", last)
	}
	return last.Parts[0]
}

func TestProposalEditorApprove(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, editorProposal(conv.ID))

	res := f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusOK)
	var out decisionView
	res.decode(&out)
	if out.Replayed {
		t.Fatal("the first approval reported a replay")
	}
	if out.Proposal.State != ProposalStateApproved {
		t.Fatalf("state = %s (%s), want approved", out.Proposal.State, out.Proposal.Failure)
	}
	if out.Proposal.DecidedAt == nil || out.Proposal.DecidedAt.IsZero() {
		t.Fatal("an approved proposal carries no decision time")
	}
	if out.Proposal.Revision != p.Revision+1 {
		t.Fatalf("revision = %d, want %d", out.Proposal.Revision, p.Revision+1)
	}

	// Approval authorizes the browser; the server ran nothing.
	checks, executions, revisions := exec.counts()
	if checks != 1 || executions != 0 || revisions != 0 {
		t.Fatalf("checks = %d, executions = %d, revisions = %d, want 1/0/0", checks, executions, revisions)
	}

	notice := f.lastNotice(owner, conv.ID)
	if notice.Type != PartProposalOutcome || notice.ProposalID != p.ID || notice.State != string(ProposalStateApproved) || notice.IsError {
		t.Fatalf("notice = %+v", notice)
	}
	want := "[Proposal " + p.ID + ` "` + p.Summary + `": approved; waiting for the editor to apply it.]`
	if notice.Text != want {
		t.Fatalf("notice text = %q, want %q", notice.Text, want)
	}

	// The conversation still lists the approved proposal: the client needs it
	// to apply the patch.
	listed := f.conversationProposals(conv.ID)
	if len(listed) != 1 || listed[0].ID != p.ID || listed[0].State != ProposalStateApproved {
		t.Fatalf("listed proposals = %+v", listed)
	}

	// The same approval is a replay and runs nothing.
	res = f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusOK)
	var replay decisionView
	res.decode(&replay)
	if !replay.Replayed || replay.Proposal.State != ProposalStateApproved {
		t.Fatalf("replay = %+v", replay)
	}
	if _, executions, _ := exec.counts(); executions != 0 {
		t.Fatalf("a replay executed %d times", executions)
	}

	// A late rejection conflicts with the stored approval.
	conflict := f.decide(p.ID, "reject", p.Revision, nil).expect(http.StatusConflict)
	var body proposalErrorView
	conflict.decode(&body)
	if body.Error.Code != CodeConflict || body.Proposal == nil || body.Proposal.State != ProposalStateApproved {
		t.Fatalf("late reject = %+v", body)
	}
}

func TestProposalEditorReject(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, editorProposal(conv.ID))

	res := f.decide(p.ID, "reject", p.Revision, nil).expect(http.StatusOK)
	var out decisionView
	res.decode(&out)
	if out.Proposal.State != ProposalStateRejected || out.Replayed {
		t.Fatalf("reject = %+v", out)
	}
	if checks, executions, _ := exec.counts(); checks != 0 || executions != 0 {
		t.Fatalf("a rejected editor proposal reached the executor: %d checks, %d executions", checks, executions)
	}
	part := f.lastNotice(owner, conv.ID)
	if part.State != string(ProposalStateRejected) || !strings.Contains(part.Text, "rejected by the user") {
		t.Fatalf("notice = %+v", part)
	}

	// Idempotent, and an approval afterwards is a conflict.
	res = f.decide(p.ID, "reject", p.Revision, nil).expect(http.StatusOK)
	var replay decisionView
	res.decode(&replay)
	if !replay.Replayed || replay.Proposal.State != ProposalStateRejected {
		t.Fatalf("replay = %+v", replay)
	}
	f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusConflict)
	if _, executions, _ := exec.counts(); executions != 0 {
		t.Fatalf("executions = %d", executions)
	}
}

func TestProposalEditorApproveStaleKeepsPending(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	exec.checkFn = func(context.Context, Proposal) error {
		return ToolError{Message: "This change is out of date. Ask for a new proposal."}
	}
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, editorProposal(conv.ID))

	res := f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusConflict)
	var body proposalErrorView
	res.decode(&body)
	if body.Error.Code != CodeStale || body.Proposal == nil || body.Proposal.State != ProposalStatePending {
		t.Fatalf("stale approve = %+v", body)
	}
	if _, executions, _ := exec.counts(); executions != 0 {
		t.Fatalf("executions = %d", executions)
	}
	// The user can still reject what can no longer be offered.
	f.decide(p.ID, "reject", p.Revision, nil).expect(http.StatusOK)
	if got := f.proposal(p.ID); got.State != ProposalStateRejected {
		t.Fatalf("state = %s", got.State)
	}
}

func TestProposalEditorArgumentEdits(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		f, _ := newProposalFixture(t, fixtureOptions{})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, editorProposal(conv.ID))

		res := f.decide(p.ID, "approve", p.Revision, map[string]any{
			"arguments": map[string]any{"editorId": "draft-1", "baseRevision": "r7", "text": "edited"},
		}).expect(http.StatusBadRequest)
		if got := res.errorCode(); got != CodeInvalidRequest {
			t.Fatalf("code = %q", got)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		f, exec := newProposalFixture(t, fixtureOptions{allowEdits: true})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, editorProposal(conv.ID))

		res := f.decide(p.ID, "approve", p.Revision, map[string]any{
			"arguments": map[string]any{"editorId": "draft-1", "baseRevision": "r7", "text": "edited"},
		}).expect(http.StatusOK)
		var out decisionView
		res.decode(&out)
		if out.Proposal.State != ProposalStateApproved {
			t.Fatalf("state = %s", out.Proposal.State)
		}
		if !strings.Contains(string(out.Proposal.Arguments), `"edited"`) {
			t.Fatalf("arguments = %s", out.Proposal.Arguments)
		}
		if out.Proposal.Revision != p.Revision+2 {
			t.Fatalf("revision = %d, want %d", out.Proposal.Revision, p.Revision+2)
		}
		// Only Revise and the approval ran; nothing executed.
		checks, executions, revisions := exec.counts()
		if checks != 1 || executions != 0 || revisions != 1 {
			t.Fatalf("checks = %d, executions = %d, revisions = %d, want 1/0/1", checks, executions, revisions)
		}
	})

	t.Run("retargeting is refused", func(t *testing.T) {
		f, exec := newProposalFixture(t, fixtureOptions{allowEdits: true})
		exec.revise = func(_ context.Context, p Proposal, args json.RawMessage) (json.RawMessage, string, error) {
			var in map[string]any
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, "", ToolError{Message: "Invalid tool arguments."}
			}
			if in["editorId"] != p.Target {
				return nil, "", ToolError{Message: "The edited arguments cannot change what the change applies to."}
			}
			canonical, hash, err := HashArguments(args)
			return canonical, hash, err
		}
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, editorProposal(conv.ID))

		res := f.decide(p.ID, "approve", p.Revision, map[string]any{
			"arguments": map[string]any{"editorId": "draft-2", "baseRevision": "r7", "text": "edited"},
		}).expect(http.StatusUnprocessableEntity)
		var body errorBody
		res.decode(&body)
		if body.Error.Code != CodeInvalidArguments {
			t.Fatalf("code = %q", body.Error.Code)
		}
		if got := f.proposal(p.ID); got.State != ProposalStatePending || got.Target != "draft-1" {
			t.Fatalf("proposal changed on a refused edit: %+v", got)
		}
	})
}

func TestProposalEditorOutcome(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		editorRev  string
		hunks      []map[string]any
		message    string
		wantState  ProposalState
		wantHunks  int
		wantFail   string
		wantNotice string
		isError    bool
	}{
		{
			name:       "applied",
			status:     outcomeApplied,
			editorRev:  "r8",
			wantState:  ProposalStateApplied,
			wantNotice: `: applied in the editor at revision r8 (not saved yet).]`,
		},
		{
			name:      "partially applied",
			status:    outcomeApplied,
			editorRev: "r8",
			hunks: []map[string]any{
				{"id": "h1", "status": HunkStatusApplied},
				{"id": "h2", "status": HunkStatusStale},
				{"id": "h3", "status": HunkStatusSkipped},
			},
			wantState:  ProposalStateApplied,
			wantHunks:  3,
			wantNotice: `: partially applied in the editor (1 of 3 parts) at revision r8 (not saved yet).]`,
		},
		{
			name:       "all hunks applied is a whole application",
			status:     outcomeApplied,
			editorRev:  "r8",
			hunks:      []map[string]any{{"id": "h1", "status": HunkStatusApplied}},
			wantState:  ProposalStateApplied,
			wantHunks:  1,
			wantNotice: `: applied in the editor at revision r8 (not saved yet).]`,
		},
		{
			name:       "stale",
			status:     outcomeStale,
			editorRev:  "r9",
			wantState:  ProposalStateStale,
			wantNotice: `: could not be applied: the editor content changed. Ask for a new proposal if still needed.]`,
		},
		{
			name:       "failed",
			status:     outcomeFailed,
			message:    "the patch did not match",
			wantState:  ProposalStateFailed,
			wantFail:   "the patch did not match",
			wantNotice: `: failed to apply in the editor: "the patch did not match"]`,
			isError:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, exec := newProposalFixture(t, fixtureOptions{})
			owner := ownerFor("user-a")
			conv := f.createConversation(nil)
			p := mustProposal(t, f.store, owner, editorProposal(conv.ID))
			approved := f.approveEditor(t, p.ID, p.Revision)

			body := map[string]any{"revision": approved.Revision, "status": tc.status}
			if tc.editorRev != "" {
				body["editorRevision"] = tc.editorRev
			}
			if tc.hunks != nil {
				body["hunks"] = tc.hunks
			}
			if tc.message != "" {
				body["message"] = tc.message
			}
			res := f.outcome(p.ID, body).expect(http.StatusOK)
			var out decisionView
			res.decode(&out)
			if out.Replayed {
				t.Fatal("the first outcome reported a replay")
			}
			if out.Proposal.State != tc.wantState {
				t.Fatalf("state = %s, want %s", out.Proposal.State, tc.wantState)
			}
			if tc.editorRev != "" && out.Proposal.EditorRevision != tc.editorRev {
				t.Fatalf("editor revision = %q, want %q", out.Proposal.EditorRevision, tc.editorRev)
			}
			if len(out.Proposal.Hunks) != tc.wantHunks {
				t.Fatalf("hunks = %+v, want %d", out.Proposal.Hunks, tc.wantHunks)
			}
			if out.Proposal.Failure != tc.wantFail {
				t.Fatalf("failure = %q, want %q", out.Proposal.Failure, tc.wantFail)
			}

			notice := f.lastNotice(owner, conv.ID)
			if notice.ProposalID != p.ID || notice.State != string(tc.wantState) || notice.IsError != tc.isError {
				t.Fatalf("notice = %+v", notice)
			}
			if !strings.HasSuffix(notice.Text, tc.wantNotice) {
				t.Fatalf("notice text = %q, want suffix %q", notice.Text, tc.wantNotice)
			}
			if strings.Contains(notice.Text, "published") || strings.Contains(notice.Text, "sent") {
				t.Fatalf("an editor notice claimed a save: %q", notice.Text)
			}

			// The outcome never executes anything on the server.
			if _, executions, _ := exec.counts(); executions != 0 {
				t.Fatalf("an editor outcome executed %d times", executions)
			}
		})
	}
}

func TestProposalEditorOutcomeReplay(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, editorProposal(conv.ID))
	approved := f.approveEditor(t, p.ID, p.Revision)

	body := map[string]any{"revision": approved.Revision, "status": outcomeApplied, "editorRevision": "r8"}
	f.outcome(p.ID, body).expect(http.StatusOK)

	// The same report again is a replay, even after the revision moved on.
	res := f.outcome(p.ID, body).expect(http.StatusOK)
	var replay decisionView
	res.decode(&replay)
	if !replay.Replayed || replay.Proposal.State != ProposalStateApplied {
		t.Fatalf("replay = %+v", replay)
	}
	if replay.Proposal.EditorRevision != "r8" {
		t.Fatalf("editor revision = %q", replay.Proposal.EditorRevision)
	}

	// A different report conflicts and changes nothing.
	conflict := f.outcome(p.ID, map[string]any{
		"revision": replay.Proposal.Revision, "status": outcomeFailed, "message": "late", "editorRevision": "r9",
	}).expect(http.StatusConflict)
	var body2 proposalErrorView
	conflict.decode(&body2)
	if body2.Error.Code != CodeConflict || body2.Proposal == nil || body2.Proposal.State != ProposalStateApplied {
		t.Fatalf("conflict = %+v", body2)
	}
	if _, executions, _ := exec.counts(); executions != 0 {
		t.Fatalf("executions = %d", executions)
	}
}

func TestProposalEditorOutcomeRejectedCases(t *testing.T) {
	t.Run("before approval", func(t *testing.T) {
		f, _ := newProposalFixture(t, fixtureOptions{})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, editorProposal(conv.ID))

		res := f.outcome(p.ID, map[string]any{
			"revision": p.Revision, "status": outcomeApplied, "editorRevision": "r8",
		}).expect(http.StatusConflict)
		var body proposalErrorView
		res.decode(&body)
		if body.Error.Code != CodeConflict || body.Proposal == nil || body.Proposal.State != ProposalStatePending {
			t.Fatalf("outcome before approval = %+v", body)
		}
		if got := f.proposal(p.ID); got.State != ProposalStatePending {
			t.Fatalf("state = %s", got.State)
		}
	})

	t.Run("server proposal", func(t *testing.T) {
		f, _ := newProposalFixture(t, fixtureOptions{})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

		res := f.outcome(p.ID, map[string]any{
			"revision": p.Revision, "status": outcomeApplied, "editorRevision": "r8",
		}).expect(http.StatusBadRequest)
		if got := res.errorCode(); got != CodeUnsupported {
			t.Fatalf("code = %q, want %q", got, CodeUnsupported)
		}
	})

	t.Run("revision mismatch", func(t *testing.T) {
		f, _ := newProposalFixture(t, fixtureOptions{})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, editorProposal(conv.ID))
		approved := f.approveEditor(t, p.ID, p.Revision)

		res := f.outcome(p.ID, map[string]any{
			"revision": approved.Revision - 1, "status": outcomeApplied, "editorRevision": "r8",
		}).expect(http.StatusConflict)
		var body proposalErrorView
		res.decode(&body)
		if body.Error.Code != CodeConflict || body.Proposal == nil || body.Proposal.State != ProposalStateApproved {
			t.Fatalf("revision mismatch = %+v", body)
		}
	})

	t.Run("approved then expired", func(t *testing.T) {
		f, _ := newProposalFixture(t, fixtureOptions{})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := editorProposal(conv.ID)
		p.ExpiresAt = time.Now().Add(250 * time.Millisecond)
		stored := mustProposal(t, f.store, owner, p)
		approved := f.approveEditor(t, stored.ID, stored.Revision)
		time.Sleep(time.Until(stored.ExpiresAt) + 20*time.Millisecond)

		res := f.outcome(stored.ID, map[string]any{
			"revision": approved.Revision, "status": outcomeApplied, "editorRevision": "r8",
		}).expect(http.StatusConflict)
		var body proposalErrorView
		res.decode(&body)
		if body.Error.Code != CodeConflict || body.Proposal == nil || body.Proposal.State != ProposalStateExpired {
			t.Fatalf("expired outcome = %+v", body)
		}
		if got := f.proposal(stored.ID); got.State != ProposalStateExpired {
			t.Fatalf("state = %s", got.State)
		}
	})

	t.Run("other owner", func(t *testing.T) {
		f, _ := newProposalFixture(t, fixtureOptions{})
		owner := ownerFor("user-a")
		conv := f.createConversation(nil)
		p := mustProposal(t, f.store, owner, editorProposal(conv.ID))
		approved := f.approveEditor(t, p.ID, p.Revision)

		body := map[string]any{"revision": approved.Revision, "status": outcomeApplied, "editorRevision": "r8"}
		other := f.callAs(tokenB, http.MethodPost, testPrefix+"/proposals/"+p.ID+"/outcome", body, nil).expect(http.StatusNotFound)
		absent := f.outcome("000000000000000", body).expect(http.StatusNotFound)
		if !bytes.Equal(other.body, absent.body) {
			t.Fatalf("other-owner body %s differs from %s", other.body, absent.body)
		}
		if got := f.proposal(p.ID); got.State != ProposalStateApproved {
			t.Fatalf("state = %s", got.State)
		}
	})
}

func TestProposalEditorOutcomeValidation(t *testing.T) {
	f, _ := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, editorProposal(conv.ID))
	approved := f.approveEditor(t, p.ID, p.Revision)
	rev := approved.Revision

	tooManyHunks := make([]map[string]any, maxOutcomeHunks+1)
	for i := range tooManyHunks {
		tooManyHunks[i] = map[string]any{"id": fmt.Sprintf("h%d", i), "status": HunkStatusApplied}
	}

	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"no body", "", http.StatusBadRequest, CodeInvalidRequest},
		{"missing status", map[string]any{"revision": rev, "editorRevision": "r8"}, http.StatusBadRequest, CodeInvalidRequest},
		{"unknown status", map[string]any{"revision": rev, "status": "saved", "editorRevision": "r8"}, http.StatusBadRequest, CodeInvalidRequest},
		{"missing revision", map[string]any{"status": outcomeApplied, "editorRevision": "r8"}, http.StatusBadRequest, CodeInvalidRequest},
		{"zero revision", map[string]any{"revision": 0, "status": outcomeApplied, "editorRevision": "r8"}, http.StatusBadRequest, CodeInvalidRequest},
		{"applied without editor revision", map[string]any{"revision": rev, "status": outcomeApplied}, http.StatusBadRequest, CodeInvalidRequest},
		{"editor revision too long", map[string]any{"revision": rev, "status": outcomeApplied, "editorRevision": strings.Repeat("r", maxOutcomeEditorRevision+1)}, http.StatusBadRequest, CodeInvalidRequest},
		{"bad hunk status", map[string]any{"revision": rev, "status": outcomeApplied, "editorRevision": "r8", "hunks": []map[string]any{{"id": "h1", "status": "done"}}}, http.StatusBadRequest, CodeInvalidRequest},
		{"duplicate hunk id", map[string]any{"revision": rev, "status": outcomeApplied, "editorRevision": "r8", "hunks": []map[string]any{{"id": "h1", "status": HunkStatusApplied}, {"id": "h1", "status": HunkStatusApplied}}}, http.StatusBadRequest, CodeInvalidRequest},
		{"empty hunk id", map[string]any{"revision": rev, "status": outcomeApplied, "editorRevision": "r8", "hunks": []map[string]any{{"id": "", "status": HunkStatusApplied}}}, http.StatusBadRequest, CodeInvalidRequest},
		{"hunk id too long", map[string]any{"revision": rev, "status": outcomeApplied, "editorRevision": "r8", "hunks": []map[string]any{{"id": strings.Repeat("h", maxOutcomeHunkID+1), "status": HunkStatusApplied}}}, http.StatusBadRequest, CodeInvalidRequest},
		{"too many hunks", map[string]any{"revision": rev, "status": outcomeApplied, "editorRevision": "r8", "hunks": tooManyHunks}, http.StatusBadRequest, CodeInvalidRequest},
		{"message too long", map[string]any{"revision": rev, "status": outcomeFailed, "message": strings.Repeat("m", maxOutcomeMessageRunes+1)}, http.StatusBadRequest, CodeInvalidRequest},
		{"unknown field", map[string]any{"revision": rev, "status": outcomeApplied, "editorRevision": "r8", "saved": true}, http.StatusBadRequest, CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := f.with(t).outcome(p.ID, tc.body).expect(tc.status)
			if got := res.errorCode(); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}

	// Nothing above touched the stored approval.
	if got := f.proposal(p.ID); got.State != ProposalStateApproved || got.Revision != rev {
		t.Fatalf("proposal = %+v", got)
	}
}

func TestProposalEditorOutcomeRouteAbsentWithoutProposals(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	stored := mustProposal(t, f.store, owner, editorProposal(conv.ID))

	res := f.outcome(stored.ID, map[string]any{
		"revision": stored.Revision, "status": outcomeApplied, "editorRevision": "r8",
	}).expect(http.StatusNotFound)
	if got := res.errorCode(); got != CodeNotFound {
		t.Fatalf("code = %q, want %q", got, CodeNotFound)
	}
}

// Review fix: approving an editor proposal restarts its deadline, so the
// browser gets a full TTL to report its outcome instead of what was left of
// the reviewer's window.
func TestProposalEditorApproveRestartsDeadline(t *testing.T) {
	f, _ := newProposalFixture(t, fixtureOptions{})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, editorProposal(conv.ID))
	ttl := p.ExpiresAt.Sub(p.CreatedAt)

	res := f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusOK)
	var out decisionView
	res.decode(&out)
	if out.Proposal.DecidedAt == nil {
		t.Fatal("no decision time")
	}
	want := out.Proposal.DecidedAt.Add(ttl)
	if d := out.Proposal.ExpiresAt.Sub(want); d < -time.Second || d > time.Second {
		t.Fatalf("expiresAt = %s, want about %s (decision + TTL)", out.Proposal.ExpiresAt, want)
	}
}

func TestProposalDecisionRefusedStaysPending(t *testing.T) {
	f, exec := newProposalFixture(t, fixtureOptions{})
	attempts := 0
	exec.execFn = func(context.Context, Proposal) (json.RawMessage, error) {
		attempts++
		if attempts == 1 {
			return nil, RefusedError{Message: "The subject line is required."}
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	p := mustProposal(t, f.store, owner, serverProposal(conv.ID))

	res := f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusUnprocessableEntity)
	var refused proposalErrorView
	res.decode(&refused)
	if refused.Error.Code != CodeRefused || refused.Error.Message != "The subject line is required." {
		t.Fatalf("error = %+v", refused.Error)
	}
	if refused.Proposal == nil || refused.Proposal.State != ProposalStatePending || refused.Proposal.Failure != "The subject line is required." {
		t.Fatalf("proposal = %+v, want pending with the reason", refused.Proposal)
	}
	if refused.Proposal.DecidedAt != nil {
		t.Fatalf("a pending proposal carries decidedAt %v", refused.Proposal.DecidedAt)
	}
	if refused.Proposal.Revision <= p.Revision {
		t.Fatalf("revision %d did not advance from %d", refused.Proposal.Revision, p.Revision)
	}
	// No outcome notice: nothing was decided.
	for _, m := range f.messages(owner, conv.ID) {
		if m.Role == RoleNotice {
			t.Fatalf("a refused approval left a notice: %+v", m)
		}
	}
	// The old revision can't approve it again; the new one can, and the
	// refusal message is gone once it succeeds.
	f.decide(p.ID, "approve", p.Revision, nil).expect(http.StatusConflict)
	var ok decisionView
	f.decide(p.ID, "approve", refused.Proposal.Revision, nil).expect(http.StatusOK).decode(&ok)
	if ok.Proposal.State != ProposalStateSucceeded || ok.Proposal.Failure != "" {
		t.Fatalf("second approval = %s %q", ok.Proposal.State, ok.Proposal.Failure)
	}
	if _, executions, _ := exec.counts(); executions != 2 {
		t.Fatalf("executions = %d, want 2", executions)
	}
}
