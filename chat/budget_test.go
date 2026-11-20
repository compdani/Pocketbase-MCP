package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

func userMsg(id, text string) Message {
	return Message{ID: id, Role: RoleUser, Parts: []Part{{Type: PartText, Text: text}}}
}

func assistantText(id, text string) Message {
	return Message{ID: id, Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: text}}}
}

func assistantCall(id, callID, tool string) Message {
	return Message{ID: id, Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, CallID: callID, Tool: tool, Arguments: json.RawMessage(`{}`)}}}
}

func toolResult(id, callID, tool string, result string) Message {
	raw, _ := json.Marshal(result)
	return Message{ID: id, Role: RoleTool, Parts: []Part{{Type: PartToolResult, CallID: callID, Tool: tool, Result: raw}}}
}

// A transcript of three turns: a small chat, a turn with a big tool result,
// and the current turn.
func budgetTranscript() []Message {
	return []Message{
		userMsg("u1", "hello"),
		assistantText("a1", "hi, how can I help?"),
		userMsg("u2", "search contracts"),
		assistantCall("a2", "c1", "search_contracts"),
		toolResult("t1", "c1", "search_contracts", strings.Repeat("contract data ", 4000)),
		assistantText("a3", "I found many contracts."),
		userMsg("u3", "summarize the first one"),
	}
}

func TestBuildHistoryUnknownWindowIsUnchanged(t *testing.T) {
	msgs := budgetTranscript()
	got := buildHistory(historyPlan{msgs: msgs})
	want := normalizeHistory(providerMessagesWith(msgs, nil))
	if !reflect.DeepEqual(got.messages, want) {
		t.Fatalf("an unknown window changed the history:\n got %+v\nwant %+v", got.messages, want)
	}
	if got.stubbed+got.truncated+got.dropped != 0 {
		t.Fatalf("an unknown window shrank the history: %+v", got)
	}
}

func TestBuildHistoryFitsWithoutChange(t *testing.T) {
	msgs := budgetTranscript()
	got := buildHistory(historyPlan{msgs: msgs, budget: historyBudget{tokens: 1_000_000}})
	if got.stubbed+got.dropped != 0 || len(got.messages) != len(msgs) {
		t.Fatalf("history that fits was shrunk: %+v", got)
	}
}

func TestBuildHistoryTruncatesOneHugeResult(t *testing.T) {
	msgs := budgetTranscript()
	got := buildHistory(historyPlan{msgs: msgs, budget: historyBudget{tokens: 1_000_000, maxToolResultTokens: 100}})
	if got.truncated != 1 {
		t.Fatalf("truncated = %d, want 1", got.truncated)
	}
	var result string
	for _, m := range got.messages {
		if m.Role == provider.RoleTool {
			_ = json.Unmarshal(m.ToolResult, &result)
		}
	}
	if !strings.Contains(result, "[truncated: the full result was") || estimateText(result) > 150 {
		t.Fatalf("result was not cut to its bound: %d tokens, %q…", estimateText(result), clipRunes(result, 80))
	}
	// The stored message keeps the full result.
	if len(msgs[4].Parts[0].Result) < 40000 {
		t.Fatal("buildHistory edited the stored message")
	}
}

func TestBuildHistoryStubsOldToolResultsFirst(t *testing.T) {
	msgs := budgetTranscript()
	full := buildHistory(historyPlan{msgs: msgs, budget: historyBudget{tokens: 1_000_000}})
	// Room for everything except the big result (already cut to its 8000
	// token bound).
	budget := full.tokens - 4000
	got := buildHistory(historyPlan{msgs: msgs, budget: historyBudget{tokens: budget, keepFullToolTurns: 1}})
	if got.stubbed != 1 || got.dropped != 0 {
		t.Fatalf("stubbed=%d dropped=%d, want the old result stubbed and nothing dropped", got.stubbed, got.dropped)
	}
	if got.tokens > budget {
		t.Fatalf("estimate %d is over the budget %d", got.tokens, budget)
	}
	found := false
	for _, m := range got.messages {
		if m.Role == provider.RoleTool && strings.Contains(string(m.ToolResult), "[result of search_contracts:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no stub naming the tool: %+v", got.messages)
	}
}

func TestBuildHistoryDropsOldestUnitsButKeepsTheCurrentTurn(t *testing.T) {
	msgs := budgetTranscript()
	got := buildHistory(historyPlan{msgs: msgs, budget: historyBudget{tokens: 20}})
	last := got.messages[len(got.messages)-1]
	if last.Role != provider.RoleUser || last.Text != "summarize the first one" {
		t.Fatalf("the current message was lost: %+v", got.messages)
	}
	if got.dropped == 0 {
		t.Fatalf("nothing was dropped from an over-full history: %+v", got)
	}
	// Never a tool result without its call.
	assertPairs(t, got.messages)
}

func TestBuildHistoryKeepsSummaryFirst(t *testing.T) {
	msgs := budgetTranscript()[2:]
	got := buildHistory(historyPlan{msgs: msgs, summary: "The user said hello.", budget: historyBudget{tokens: 1_000_000}})
	if first := got.messages[0]; first.Role != provider.RoleUser || !strings.HasPrefix(first.Text, summaryNoticePrefix) ||
		!strings.HasSuffix(first.Text, "The user said hello.") {
		t.Fatalf("first message = %+v, want the labelled summary", first)
	}
	unknown := buildHistory(historyPlan{msgs: msgs, summary: "The user said hello."})
	if !strings.HasPrefix(unknown.messages[0].Text, summaryNoticePrefix) {
		t.Fatal("an unknown window dropped the summary")
	}
}

func TestBuildHistoryPinsPendingProposals(t *testing.T) {
	pending, _ := json.Marshal(map[string]string{"status": "pending_review", "proposalId": "p1", "summary": "Create item"})
	msgs := []Message{
		userMsg("u1", "create an item"),
		assistantCall("a1", "c1", "create_items"),
		{ID: "t1", Role: RoleTool, Parts: []Part{{Type: PartToolResult, CallID: "c1", Tool: "create_items", Result: pending}}},
		assistantText("a2", "Waiting for your approval."),
		userMsg("u2", strings.Repeat("more talk ", 300)),
		assistantText("a3", strings.Repeat("an answer ", 300)),
		userMsg("u3", "and now?"),
	}
	got := buildHistory(historyPlan{msgs: msgs, budget: historyBudget{tokens: 30}})
	foundCall := false
	for _, m := range got.messages {
		for _, c := range m.ToolCalls {
			if c.Name == "create_items" {
				foundCall = true
			}
		}
	}
	if !foundCall {
		t.Fatalf("the pending proposal's call was dropped: %+v", got.messages)
	}

	// Once a notice records the outcome, the pin is gone.
	decided := append(append([]Message{}, msgs[:4]...),
		Message{ID: "n1", Role: RoleNotice, Parts: []Part{{Type: PartProposalOutcome, ProposalID: "p1", Text: "[Proposal p1 approved]"}}})
	decided = append(decided, msgs[4:]...)
	units := groupUnits(decided)
	for _, u := range units {
		if u.pinned {
			t.Fatalf("a decided proposal stayed pinned: %+v", u)
		}
	}
}

// assertPairs checks the provider rule the history must never break: every
// tool result answers a call that comes before it.
func assertPairs(t *testing.T, msgs []provider.Message) {
	t.Helper()
	declared := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			declared[c.ID] = true
		}
		if m.Role == provider.RoleTool && !declared[m.ToolCallID] {
			t.Fatalf("tool result %s has no call before it: %+v", m.ToolCallID, msgs)
		}
	}
}

func TestBuildHistoryRandomTranscriptsKeepPairs(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 200; round++ {
		var msgs []Message
		n := 0
		id := func() string { n++; return fmt.Sprintf("m%d", n) }
		for turn := 0; turn < 1+rng.Intn(6); turn++ {
			msgs = append(msgs, userMsg(id(), strings.Repeat("q", rng.Intn(400))))
			for step := 0; step < rng.Intn(3); step++ {
				call := id()
				msgs = append(msgs, assistantCall(id(), call, "tool"))
				msgs = append(msgs, toolResult(id(), call, "tool", strings.Repeat("r", rng.Intn(5000))))
			}
			msgs = append(msgs, assistantText(id(), strings.Repeat("a", rng.Intn(400))))
		}
		got := buildHistory(historyPlan{msgs: msgs, budget: historyBudget{tokens: 1 + rng.Intn(3000)}})
		assertPairs(t, got.messages)
		if len(got.messages) == 0 {
			t.Fatalf("round %d: empty history", round)
		}
	}
}

func TestCompactionBoundary(t *testing.T) {
	msgs := budgetTranscript()
	// Keep the last turn only: everything before u3 is summarized.
	if cut := compactionBoundary(msgs, -1, 1); cut != 6 {
		t.Fatalf("boundary keeping one turn = %d, want 6", cut)
	}
	// Keeping more turns than there are leaves nothing.
	if cut := compactionBoundary(msgs, -1, 5); cut != 0 {
		t.Fatalf("boundary keeping five turns = %d, want 0", cut)
	}
	// A large keep leaves everything in full.
	if cut := compactionBoundary(msgs, 1_000_000, 1); cut != 0 {
		t.Fatalf("boundary with room for everything = %d, want 0", cut)
	}
	// A small keep summarizes up to the last turn but never ends on a lone
	// user message.
	if cut := compactionBoundary(msgs, 10, 0); cut != 6 || msgs[cut-1].Role == RoleUser {
		t.Fatalf("boundary with a small keep = %d", cut)
	}
}

func TestDefaultContextWindow(t *testing.T) {
	cases := map[string]int{
		"claude-opus-5-5":             1_000_000,
		"anthropic/claude-sonnet-4-6": 1_000_000,
		"claude-haiku-4-5":            200_000,
		"gpt-4o-mini":                 128_000,
		"gpt-5":                       272_000,
		"demo-1":                      0,
		"":                            0,
	}
	for model, want := range cases {
		if got := DefaultContextWindow(model); got != want {
			t.Errorf("DefaultContextWindow(%q) = %d, want %d", model, got, want)
		}
	}
}

// --- runner integration ---

func TestRunnerRetriesAContextOverflowOnce(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "long")
	for i := 0; i < 6; i++ {
		m := userMsg("", strings.Repeat("old words ", 200))
		m.ConversationID = conv.ID
		if _, err := store.AppendMessage(context.Background(), owner, m); err != nil {
			t.Fatal(err)
		}
		a := assistantText("", "ok")
		a.ConversationID = conv.ID
		if _, err := store.AppendMessage(context.Background(), owner, a); err != nil {
			t.Fatal(err)
		}
	}
	prov := scripted.New(
		scripted.Step{Chunks: []provider.Chunk{{Kind: provider.ChunkError, Err: provider.ErrContextOverflow}}},
		textStep("fine"),
	)
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "unknown-model"})
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "new question", "k", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if final := awaitTerminal(t, store, owner, turn.ID); final.State != TurnStateCompleted {
		t.Fatalf("state = %s (%+v), want completed after one retry", final.State, final.Error)
	}
	reqs := prov.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if len(reqs[1].Messages) >= len(reqs[0].Messages) {
		t.Fatalf("the retry sent %d messages, the failed request %d: want fewer", len(reqs[1].Messages), len(reqs[0].Messages))
	}
	if last := reqs[1].Messages[len(reqs[1].Messages)-1]; last.Text != "new question" {
		t.Fatalf("the retry lost the current message: %+v", last)
	}
}

func TestRunnerFailsWithContextTooLongAfterTheRetry(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "too long")
	overflow := scripted.Step{Chunks: []provider.Chunk{{Kind: provider.ChunkError, Err: provider.ErrContextOverflow}}}
	prov := scripted.New(overflow, overflow)
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	final := awaitTerminal(t, store, owner, turn.ID)
	if final.State != TurnStateFailed || final.Error == nil || final.Error.Code != FailureContextTooLong {
		t.Fatalf("turn = %s %+v, want failed with %s", final.State, final.Error, FailureContextTooLong)
	}
	if n := len(prov.Requests()); n != 2 {
		t.Fatalf("requests = %d, want exactly one retry", n)
	}
}

func TestRunnerBudgetsHistoryForAKnownWindow(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "budget")
	for i := 0; i < 20; i++ {
		m := userMsg("", fmt.Sprintf("message %d %s", i, strings.Repeat("filler ", 300)))
		m.ConversationID = conv.ID
		if _, err := store.AppendMessage(context.Background(), owner, m); err != nil {
			t.Fatal(err)
		}
		a := assistantText("", "noted")
		a.ConversationID = conv.ID
		if _, err := store.AppendMessage(context.Background(), owner, a); err != nil {
			t.Fatal(err)
		}
	}
	prov := scripted.New(textStep("ok"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", ContextWindow: 12_000, MaxOutputTokens: 1000})
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "latest", "k", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, store, owner, turn.ID)
	req := prov.Requests()[0]
	if got := estimateMessages(req.Messages, defaultImageTokenEstimate); got > 12_000-1000-1024 {
		t.Fatalf("history estimate %d does not fit the window", got)
	}
	if len(req.Messages) >= 41 {
		t.Fatalf("all %d messages were sent to a small window", len(req.Messages))
	}
	if last := req.Messages[len(req.Messages)-1]; last.Text != "latest" {
		t.Fatalf("last message = %+v", last)
	}

	status, err := runner.ContextStatus(context.Background(), owner, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Window != 12_000 || status.Tokens <= 0 || !status.Estimated {
		t.Fatalf("status = %+v, want an estimate against the 12k window", status)
	}
}

// summaryProvider answers summary requests with a fixed text and everything
// else with "ok". It records what it was asked to summarize.
type summaryProvider struct {
	scripted.Provider
	asked chan string
}

func (p *summaryProvider) Name() string { return "summary" }

func (p *summaryProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	ch := make(chan provider.Chunk, 4)
	if req.System == CompactionSystemPrompt {
		select {
		case p.asked <- req.Messages[0].Text:
		default:
		}
		ch <- provider.Chunk{Kind: provider.ChunkText, Text: "SUMMARY: the user talked about filler."}
	} else {
		ch <- provider.Chunk{Kind: provider.ChunkUsage, Usage: &provider.Usage{InputTokens: int64(estimateMessages(req.Messages, 0) + 50)}}
		ch <- provider.Chunk{Kind: provider.ChunkText, Text: "ok"}
	}
	ch <- provider.Chunk{Kind: provider.ChunkDone}
	close(ch)
	return ch, nil
}

func TestRunnerSummarizesInTheBackgroundAndUsesTheSummary(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "compact")
	for i := 0; i < 15; i++ {
		m := userMsg("", fmt.Sprintf("message %d %s", i, strings.Repeat("filler ", 200)))
		m.ConversationID = conv.ID
		if _, err := store.AppendMessage(context.Background(), owner, m); err != nil {
			t.Fatal(err)
		}
		a := assistantText("", "noted")
		a.ConversationID = conv.ID
		if _, err := store.AppendMessage(context.Background(), owner, a); err != nil {
			t.Fatal(err)
		}
	}
	prov := &summaryProvider{asked: make(chan string, 4)}
	runner := mustRunner(t, RunnerConfig{
		Store: store, Provider: prov, Model: "m",
		ContextWindow: 8_000, MaxOutputTokens: 500,
		Compaction: &CompactionConfig{},
	})
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "first", "k1", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, store, owner, turn.ID)
	var input string
	select {
	case input = <-prov.asked:
	case <-time.After(testWait):
		t.Fatal("no summary was requested for a nearly full conversation")
	}
	if !strings.Contains(input, "User: message 0") {
		t.Fatalf("summary input does not start at the oldest message: %q", clipRunes(input, 120))
	}
	var summary Summary
	deadline := time.Now().Add(testWait)
	for {
		s, found, err := store.LatestSummary(context.Background(), owner, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			summary = s
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the summary was never stored")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if summary.Text != "SUMMARY: the user talked about filler." {
		t.Fatalf("summary = %+v", summary)
	}

	status, err := runner.ContextStatus(context.Background(), owner, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.SummarizedThrough != summary.ThroughMessageID || !status.CanCompact {
		t.Fatalf("status = %+v, want the summary boundary", status)
	}
}

func TestRunnerCompactNow(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "manual")
	prov := &summaryProvider{asked: make(chan string, 4)}
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", ContextWindow: 100_000, Compaction: &CompactionConfig{}})

	if _, err := runner.Compact(context.Background(), owner, conv.ID); err != ErrNothingToCompact {
		t.Fatalf("empty conversation: err=%v, want ErrNothingToCompact", err)
	}
	// The failed attempt counted for the rate limit: wait it out by faking
	// the clock.
	now := time.Now()
	runner.now = func() time.Time { return now.Add(time.Minute) }
	for i := 0; i < 4; i++ {
		turn, _, err := runner.Submit(context.Background(), owner, conv.ID, fmt.Sprintf("question %d", i), fmt.Sprintf("k%d", i), nil, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		awaitTerminal(t, store, owner, turn.ID)
	}
	res, err := runner.Compact(context.Background(), owner, conv.ID)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	msgs := drainMessages(t, store, owner, conv.ID)
	// Two turns stay in full: the summary ends at the reply to question 1.
	if res.Summary.ThroughMessageID != msgs[3].ID {
		t.Fatalf("summary through %s, want %s (the reply to question 1)", res.Summary.ThroughMessageID, msgs[3].ID)
	}
	if res.Context.SummarizedThrough != res.Summary.ThroughMessageID {
		t.Fatalf("context = %+v", res.Context)
	}
	if _, err := runner.Compact(context.Background(), owner, conv.ID); err != ErrCompactionTooSoon {
		t.Fatalf("second compaction at once: err=%v, want ErrCompactionTooSoon", err)
	}

	off := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	if _, err := off.Compact(context.Background(), owner, conv.ID); err != ErrCompactionUnavailable {
		t.Fatalf("summaries off: err=%v, want ErrCompactionUnavailable", err)
	}
}

// A small window keeps room for history, and when history has to be cut, the
// context status reports what the conversation holds, not the trimmed request.
func TestRunnerSmallWindowReportsHeldTokens(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "small")
	for i := 0; i < 6; i++ {
		a := assistantText("", strings.Repeat("a long earlier answer ", 120))
		u := userMsg("", fmt.Sprintf("question %d", i))
		for _, m := range []Message{u, a} {
			m.ConversationID = conv.ID
			if _, err := store.AppendMessage(context.Background(), owner, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	prov := scripted.New(textStep("ok"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "demo-1", ContextWindow: 4000})
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "latest", "k", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, store, owner, turn.ID)
	req := prov.Requests()[0]
	if len(req.Messages) < 3 {
		t.Fatalf("a 4000-token window sent only %d messages: the reply reserve left no room", len(req.Messages))
	}
	status, err := runner.ContextStatus(context.Background(), owner, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Tokens <= status.Window {
		t.Fatalf("status = %+v: a conversation that no longer fits must report more than the window", status)
	}
}

// A result the model asked for in this turn reaches it whole when it fits,
// even when it is over MaxToolResultTokens; only an over-full history cuts it.
func TestBuildHistoryKeepsTheCurrentTurnsResultWhole(t *testing.T) {
	schema := strings.Repeat("workflow schema field ", 3000) // ~19k tokens
	msgs := []Message{
		userMsg("u1", "build a workflow"),
		assistantCall("a1", "c1", "workflow_schema"),
		toolResult("t1", "c1", "workflow_schema", schema),
	}
	whole := buildHistory(historyPlan{msgs: msgs, budget: historyBudget{tokens: 100_000}})
	if whole.truncated != 0 || !strings.Contains(string(whole.messages[len(whole.messages)-1].ToolResult), schema[:2000]) {
		t.Fatalf("the current turn's result was cut although it fits: truncated=%d", whole.truncated)
	}
	tight := buildHistory(historyPlan{msgs: msgs, budget: historyBudget{tokens: 9_000}})
	if tight.truncated != 1 {
		t.Fatalf("an over-full history kept the huge result whole: truncated=%d", tight.truncated)
	}
}
