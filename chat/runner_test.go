package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// testWait bounds every wait in these tests. It is intentionally generous: the
// tests synchronize on stored state and events, never on fixed sleeps.
const testWait = 10 * time.Second

func mustRunner(t *testing.T, cfg RunnerConfig) *Runner {
	t.Helper()
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), testWait)
		defer cancel()
		_ = r.Shutdown(shutdownCtx)
	})
	return r
}

func mustConversation(t *testing.T, s Store, owner OwnerKey, title string) Conversation {
	t.Helper()
	c, err := s.CreateConversation(context.Background(), Conversation{Owner: owner, Title: title})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return c
}

func awaitTerminal(t *testing.T, s Store, owner OwnerKey, turnID string) Turn {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		turn, err := s.GetTurn(context.Background(), owner, turnID)
		if err != nil {
			t.Fatalf("GetTurn(%s): %v", turnID, err)
		}
		if turn.State.Terminal() {
			return turn
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s did not finish, state %s", turnID, turn.State)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func mustEvents(t *testing.T, s Store, owner OwnerKey, turnID string) []Event {
	t.Helper()
	events, err := s.EventsAfter(context.Background(), owner, turnID, 0)
	if err != nil {
		t.Fatalf("EventsAfter(%s): %v", turnID, err)
	}
	return events
}

func waitForEventCount(t *testing.T, s Store, owner OwnerKey, turnID string, n int) []Event {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		events, err := s.EventsAfter(context.Background(), owner, turnID, 0)
		if err != nil {
			t.Fatalf("EventsAfter(%s): %v", turnID, err)
		}
		if len(events) >= n {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s had %d events, want at least %d", turnID, len(events), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func typesOf(events []Event) []EventType {
	out := make([]EventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func countTerminals(events []Event) int {
	n := 0
	for _, e := range events {
		if e.Type.Terminal() {
			n++
		}
	}
	return n
}

func decodeEvent[T any](t *testing.T, e Event) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(e.Data, &out); err != nil {
		t.Fatalf("decode %s data: %v", e.Type, err)
	}
	return out
}

func terminalEvent(t *testing.T, events []Event) Event {
	t.Helper()
	var found Event
	n := 0
	for _, e := range events {
		if e.Type.Terminal() {
			found = e
			n++
		}
	}
	if n != 1 {
		t.Fatalf("turn stored %d terminal events, want 1: %v", n, typesOf(events))
	}
	return found
}

func drainMessages(t *testing.T, s Store, owner OwnerKey, conversationID string) []Message {
	t.Helper()
	msgs, _, err := s.ListMessages(context.Background(), owner, conversationID, Page{Limit: maxPageLimit})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	return msgs
}

func assertNoSecret(t *testing.T, secret string, events []Event, msgs []Message, reqs []provider.Request) {
	t.Helper()
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("event leaked %q: %s", secret, raw)
		}
	}
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("message leaked %q: %s", secret, raw)
		}
	}
	for _, r := range reqs {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("request leaked %q: %s", secret, raw)
		}
	}
}

type toolCallRecord struct {
	name string
	args json.RawMessage
}

// fakeTools is a ToolExecutor with a fixed result, an optional error, or a
// custom call function. It records every call.
type fakeTools struct {
	defs   []provider.ToolDef
	result json.RawMessage
	err    error
	fn     func(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)

	mu    sync.Mutex
	calls []toolCallRecord
}

func (f *fakeTools) Tools(context.Context) ([]provider.ToolDef, error) { return f.defs, nil }

func (f *fakeTools) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, toolCallRecord{name: name, args: cloneRaw(args)})
	fn, res, err := f.fn, f.result, f.err
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, name, args)
	}
	return res, err
}

func (f *fakeTools) recorded() []toolCallRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]toolCallRecord, len(f.calls))
	copy(out, f.calls)
	return out
}

// watchingProvider records the context of every Stream call so a test can
// prove cancellation reached the provider.
type watchingProvider struct {
	inner provider.Provider

	mu   sync.Mutex
	ctxs []context.Context
}

func (w *watchingProvider) Name() string { return w.inner.Name() }

func (w *watchingProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	w.mu.Lock()
	w.ctxs = append(w.ctxs, ctx)
	w.mu.Unlock()
	return w.inner.Stream(ctx, req)
}

func (w *watchingProvider) lastContext() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.ctxs) == 0 {
		return nil
	}
	return w.ctxs[len(w.ctxs)-1]
}

func textStep(parts ...string) scripted.Step {
	chunks := make([]provider.Chunk, 0, len(parts)+1)
	for _, p := range parts {
		chunks = append(chunks, provider.Chunk{Kind: provider.ChunkText, Text: p})
	}
	chunks = append(chunks, provider.Chunk{Kind: provider.ChunkDone})
	return scripted.Step{Chunks: chunks}
}

func toolCallStep(calls ...provider.ToolCall) scripted.Step {
	chunks := make([]provider.Chunk, 0, len(calls)+1)
	for i := range calls {
		chunks = append(chunks, provider.Chunk{Kind: provider.ChunkToolCall, ToolCall: &calls[i]})
	}
	chunks = append(chunks, provider.Chunk{Kind: provider.ChunkDone})
	return scripted.Step{Chunks: chunks}
}

func TestRunnerPlainTextReply(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(scripted.Step{Chunks: []provider.Chunk{
		{Kind: provider.ChunkText, Text: "Hello "},
		{Kind: provider.ChunkText, Text: "world"},
		{Kind: provider.ChunkUsage, Usage: &provider.Usage{InputTokens: 4, OutputTokens: 2}},
		{Kind: provider.ChunkDone, StopReason: "end_turn"},
	}})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "test-model", System: "be brief"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "plain")

	turn, created, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "key-1", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !created {
		t.Fatal("Submit reported created=false for a new turn")
	}
	final := awaitTerminal(t, store, owner, turn.ID)
	if final.State != TurnStateCompleted {
		t.Fatalf("state = %s, want completed (%+v)", final.State, final.Error)
	}
	if final.Error != nil {
		t.Fatalf("completed turn carries an error: %+v", final.Error)
	}

	events := mustEvents(t, store, owner, turn.ID)
	want := []EventType{
		EventTurnQueued, EventTurnStarted,
		EventTextDelta, EventTextDelta, EventUsageUpdated,
		EventTurnCompleted,
	}
	got := typesOf(events)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s, want %s (all: %v)", i, got[i], want[i], got)
		}
		if events[i].Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, events[i].Seq, i+1)
		}
	}
	usage := decodeEvent[Usage](t, events[4])
	if usage.InputTokens != 4 || usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}

	var view TurnView
	for _, e := range events {
		if err := view.Apply(e); err != nil {
			t.Fatalf("reducer rejected %s: %v", e.Type, err)
		}
	}
	if view.State != TurnStateCompleted {
		t.Fatalf("reduced state = %s", view.State)
	}
	if view.Text != "Hello world" {
		t.Fatalf("reduced text = %q", view.Text)
	}

	msgs := drainMessages(t, store, owner, conv.ID)
	if len(msgs) != 2 {
		t.Fatalf("stored %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleUser || msgs[0].Parts[0].Text != "hi" {
		t.Fatalf("user message = %+v", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || msgs[1].Parts[0].Text != "Hello world" {
		t.Fatalf("assistant message = %+v", msgs[1])
	}

	reqs := prov.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	if reqs[0].Model != "test-model" || reqs[0].System != "be brief" {
		t.Fatalf("request = %+v", reqs[0])
	}
	if len(reqs[0].Messages) != 1 || reqs[0].Messages[0].Role != provider.RoleUser || reqs[0].Messages[0].Text != "hi" {
		t.Fatalf("history = %+v", reqs[0].Messages)
	}
}

func TestRunnerToolRoundTrip(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	call := provider.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"q":"x"}`)}
	prov := scripted.New(
		toolCallStep(call),
		textStep("all done"),
	)
	tools := &fakeTools{
		defs:   []provider.ToolDef{{Name: "echo", Description: "echo it", InputSchema: map[string]any{"type": "object"}}},
		result: json.RawMessage(`{"ok":true}`),
	}
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "tools")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "use the tool", "key", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	final := awaitTerminal(t, store, owner, turn.ID)
	if final.State != TurnStateCompleted {
		t.Fatalf("state = %s (%+v)", final.State, final.Error)
	}

	events := mustEvents(t, store, owner, turn.ID)
	types := typesOf(events)
	for _, want := range []EventType{EventToolStarted, EventToolCompleted} {
		found := false
		for _, typ := range types {
			if typ == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s in %v", want, types)
		}
	}
	started := decodeEvent[ToolStarted](t, events[2])
	if started.CallID != "call-1" || started.Tool != "echo" || string(started.Arguments) != `{"q":"x"}` {
		t.Fatalf("tool.started = %+v", started)
	}

	reqs := prov.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(reqs))
	}
	if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "echo" {
		t.Fatalf("first request tools = %+v", reqs[0].Tools)
	}
	history := reqs[1].Messages
	if len(history) != 3 {
		t.Fatalf("second request history = %+v", history)
	}
	if history[0].Role != provider.RoleUser || history[0].Text != "use the tool" {
		t.Fatalf("history[0] = %+v", history[0])
	}
	if history[1].Role != provider.RoleAssistant || len(history[1].ToolCalls) != 1 {
		t.Fatalf("history[1] = %+v", history[1])
	}
	if history[1].ToolCalls[0].ID != "call-1" || history[1].ToolCalls[0].Name != "echo" {
		t.Fatalf("assistant tool call = %+v", history[1].ToolCalls[0])
	}
	if history[2].Role != provider.RoleTool || history[2].ToolCallID != "call-1" || history[2].ToolName != "echo" {
		t.Fatalf("history[2] = %+v", history[2])
	}
	if string(history[2].ToolResult) != `{"ok":true}` || history[2].ToolIsError {
		t.Fatalf("tool result = %+v", history[2])
	}

	if calls := tools.recorded(); len(calls) != 1 || calls[0].name != "echo" {
		t.Fatalf("executor calls = %+v", calls)
	}
	msgs := drainMessages(t, store, owner, conv.ID)
	if len(msgs) != 4 {
		t.Fatalf("stored messages = %+v", msgs)
	}
	if msgs[2].Role != RoleTool || msgs[2].Parts[0].Type != PartToolResult || msgs[2].Parts[0].IsError {
		t.Fatalf("stored tool message = %+v", msgs[2])
	}
	if msgs[3].Role != RoleAssistant || msgs[3].Parts[0].Text != "all done" {
		t.Fatalf("final assistant message = %+v", msgs[3])
	}
}

func TestRunnerToolFailureIsUserSafe(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "search", Arguments: json.RawMessage(`{}`)}

	t.Run("ToolError message is shown", func(t *testing.T) {
		store := NewMemStore(MemStoreOptions{})
		prov := scripted.New(toolCallStep(call), textStep("recovered"))
		tools := &fakeTools{err: ToolError{Message: "query is invalid"}}
		runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
		owner := ownerFixture()
		conv := mustConversation(t, store, owner, "tool error")

		turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "go", "k", tools, time.Time{})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		final := awaitTerminal(t, store, owner, turn.ID)
		if final.State != TurnStateCompleted {
			t.Fatalf("state = %s (%+v)", final.State, final.Error)
		}

		events := mustEvents(t, store, owner, turn.ID)
		var failed *ToolFailed
		for i := range events {
			if events[i].Type == EventToolFailed {
				d := decodeEvent[ToolFailed](t, events[i])
				failed = &d
			}
		}
		if failed == nil {
			t.Fatalf("no tool.failed event in %v", typesOf(events))
		}
		if failed.Message != "query is invalid" {
			t.Fatalf("tool.failed message = %q", failed.Message)
		}

		reqs := prov.Requests()
		if len(reqs) != 2 {
			t.Fatalf("provider calls = %d", len(reqs))
		}
		var toolMsg *provider.Message
		for i := range reqs[1].Messages {
			if reqs[1].Messages[i].Role == provider.RoleTool {
				toolMsg = &reqs[1].Messages[i]
			}
		}
		if toolMsg == nil || !toolMsg.ToolIsError {
			t.Fatalf("tool result = %+v", toolMsg)
		}
		if !strings.Contains(string(toolMsg.ToolResult), "query is invalid") {
			t.Fatalf("model did not receive the safe message: %s", toolMsg.ToolResult)
		}
	})

	t.Run("unexpected error is generic", func(t *testing.T) {
		const secret = "dial tcp 10.0.0.1: connection refused"
		store := NewMemStore(MemStoreOptions{})
		prov := scripted.New(toolCallStep(call), textStep("recovered"))
		tools := &fakeTools{err: errors.New(secret)}
		runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
		owner := ownerFixture()
		conv := mustConversation(t, store, owner, "tool error generic")

		turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "go", "k", tools, time.Time{})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		awaitTerminal(t, store, owner, turn.ID)

		events := mustEvents(t, store, owner, turn.ID)
		var failed *ToolFailed
		for i := range events {
			if events[i].Type == EventToolFailed {
				d := decodeEvent[ToolFailed](t, events[i])
				failed = &d
			}
		}
		if failed == nil || failed.Message != toolGenericFailure {
			t.Fatalf("tool.failed = %+v", failed)
		}
		assertNoSecret(t, secret, events, drainMessages(t, store, owner, conv.ID), prov.Requests())
	})

	t.Run("panic is reported and the turn continues", func(t *testing.T) {
		store := NewMemStore(MemStoreOptions{})
		prov := scripted.New(toolCallStep(call), textStep("recovered"))
		tools := &fakeTools{fn: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			panic("executor exploded")
		}}
		runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
		owner := ownerFixture()
		conv := mustConversation(t, store, owner, "tool panic")

		turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "go", "k", tools, time.Time{})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		final := awaitTerminal(t, store, owner, turn.ID)
		if final.State != TurnStateCompleted {
			t.Fatalf("state = %s (%+v)", final.State, final.Error)
		}
		events := mustEvents(t, store, owner, turn.ID)
		if countTerminals(events) != 1 {
			t.Fatalf("terminal events = %d", countTerminals(events))
		}
		var failed *ToolFailed
		for i := range events {
			if events[i].Type == EventToolFailed {
				d := decodeEvent[ToolFailed](t, events[i])
				failed = &d
			}
		}
		if failed == nil || failed.Message != toolGenericFailure {
			t.Fatalf("tool.failed = %+v", failed)
		}
		assertNoSecret(t, "executor exploded", events, drainMessages(t, store, owner, conv.ID), prov.Requests())
	})
}

func TestRunnerCancel(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	inner := scripted.New(scripted.Step{
		Chunks:           []provider.Chunk{{Kind: provider.ChunkText, Text: "partial"}},
		BlockUntilCancel: true,
	})
	prov := &watchingProvider{inner: inner}
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "stop")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "long", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForEventCount(t, store, owner, turn.ID, 3) // queued, started, one delta

	cancelled, err := runner.Cancel(context.Background(), owner, turn.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.State != TurnStateCancelled {
		t.Fatalf("state = %s, want cancelled", cancelled.State)
	}
	if cancelled.Error != nil {
		t.Fatalf("cancelled turn carries an error: %+v", cancelled.Error)
	}

	events := mustEvents(t, store, owner, turn.ID)
	if countTerminals(events) != 1 {
		t.Fatalf("terminal events = %d in %v", countTerminals(events), typesOf(events))
	}
	term := terminalEvent(t, events)
	if term.Type != EventTurnCancelled {
		t.Fatalf("terminal event = %s, want turn.cancelled", term.Type)
	}
	if payload := decodeEvent[TurnCancelled](t, term); payload.Reason != "user" {
		t.Fatalf("cancel reason = %q", payload.Reason)
	}

	providerCtx := prov.lastContext()
	if providerCtx == nil {
		t.Fatal("provider was never called")
	}
	select {
	case <-providerCtx.Done():
	case <-time.After(testWait):
		t.Fatal("cancellation did not reach the provider context")
	}

	// Cancelling a terminal turn changes nothing and does not add an event.
	before := mustEvents(t, store, owner, turn.ID)
	again, err := runner.Cancel(context.Background(), owner, turn.ID)
	if err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if again.State != TurnStateCancelled {
		t.Fatalf("second Cancel state = %s", again.State)
	}
	after := mustEvents(t, store, owner, turn.ID)
	if len(after) != len(before) || countTerminals(after) != 1 {
		t.Fatalf("second Cancel added events: %v", typesOf(after))
	}
}

func TestRunnerCancelQueuedTurn(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	release := make(chan struct{})
	tools := &fakeTools{fn: func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		select {
		case <-release:
			return json.RawMessage(`{"ok":true}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	call := provider.ToolCall{ID: "c1", Name: "gate", Arguments: json.RawMessage(`{}`)}
	prov := scripted.New(toolCallStep(call), textStep("done"))
	runner := mustRunner(t, RunnerConfig{
		Store: store, Provider: prov, Model: "m",
		MaxConcurrentPerUser: 1,
	})
	owner := ownerFixture()
	first := mustConversation(t, store, owner, "first")
	second := mustConversation(t, store, owner, "second")

	running, _, err := runner.Submit(context.Background(), owner, first.ID, "block", "k1", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit first: %v", err)
	}
	waitForEventCount(t, store, owner, running.ID, 3) // tool.started

	queued, _, err := runner.Submit(context.Background(), owner, second.ID, "wait", "k2", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit second: %v", err)
	}
	if queued.State != TurnStateQueued {
		t.Fatalf("second turn state = %s, want queued", queued.State)
	}
	got, err := store.GetTurn(context.Background(), owner, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != TurnStateQueued {
		t.Fatalf("second turn state in store = %s, want queued", got.State)
	}

	cancelled, err := runner.Cancel(context.Background(), owner, queued.ID)
	if err != nil {
		t.Fatalf("Cancel queued: %v", err)
	}
	if cancelled.State != TurnStateCancelled {
		t.Fatalf("cancelled queued state = %s", cancelled.State)
	}
	events := mustEvents(t, store, owner, queued.ID)
	if countTerminals(events) != 1 || terminalEvent(t, events).Type != EventTurnCancelled {
		t.Fatalf("queued turn events = %v", typesOf(events))
	}

	close(release)
	if final := awaitTerminal(t, store, owner, running.ID); final.State != TurnStateCompleted {
		t.Fatalf("first turn state = %s (%+v)", final.State, final.Error)
	}
	// The cancelled turn never ran the model.
	if reqs := prov.Requests(); len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool step + final step of the first turn)", len(reqs))
	}
}

func TestRunnerPerUserConcurrency(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	release := make(chan struct{})
	tools := &fakeTools{fn: func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		select {
		case <-release:
			return json.RawMessage(`true`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	call := provider.ToolCall{ID: "c1", Name: "gate", Arguments: json.RawMessage(`{}`)}
	prov := scripted.New(toolCallStep(call), textStep("done"), textStep("second done"))
	runner := mustRunner(t, RunnerConfig{
		Store: store, Provider: prov, Model: "m",
		MaxConcurrentPerUser: 1,
	})
	owner := ownerFixture()
	first := mustConversation(t, store, owner, "first")
	second := mustConversation(t, store, owner, "second")

	running, _, err := runner.Submit(context.Background(), owner, first.ID, "block", "k1", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit first: %v", err)
	}
	waitForEventCount(t, store, owner, running.ID, 3)

	queued, _, err := runner.Submit(context.Background(), owner, second.ID, "wait", "k2", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit second: %v", err)
	}

	// While the first turn runs, the second must stay queued, not fail. The
	// observation window is short; the ordering assertion below is the real
	// proof that it never ran early.
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		got, err := store.GetTurn(context.Background(), owner, queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != TurnStateQueued {
			t.Fatalf("second turn left the queue while the first was running: %s", got.State)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(release)
	if final := awaitTerminal(t, store, owner, running.ID); final.State != TurnStateCompleted {
		t.Fatalf("first state = %s (%+v)", final.State, final.Error)
	}
	if final := awaitTerminal(t, store, owner, queued.ID); final.State != TurnStateCompleted {
		t.Fatalf("second state = %s (%+v)", final.State, final.Error)
	}

	firstEvents := mustEvents(t, store, owner, running.ID)
	secondEvents := mustEvents(t, store, owner, queued.ID)
	firstDone := terminalEvent(t, firstEvents)
	var secondStart Event
	for _, e := range secondEvents {
		if e.Type == EventTurnStarted {
			secondStart = e
		}
	}
	if secondStart.At.IsZero() {
		t.Fatal("second turn never started")
	}
	if secondStart.At.Before(firstDone.At) {
		t.Fatalf("second turn started at %s before the first ended at %s", secondStart.At, firstDone.At)
	}
}

func TestRunnerGlobalConcurrency(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	release := make(chan struct{})
	tools := &fakeTools{fn: func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		select {
		case <-release:
			return json.RawMessage(`true`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	call := provider.ToolCall{ID: "c1", Name: "gate", Arguments: json.RawMessage(`{}`)}
	prov := scripted.New(toolCallStep(call), textStep("done"), textStep("second done"))
	runner := mustRunner(t, RunnerConfig{
		Store: store, Provider: prov, Model: "m",
		MaxConcurrentGlobal:  1,
		MaxConcurrentPerUser: 4,
	})
	owner := ownerFixture()
	first := mustConversation(t, store, owner, "first")
	second := mustConversation(t, store, owner, "second")

	running, _, err := runner.Submit(context.Background(), owner, first.ID, "block", "k1", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit first: %v", err)
	}
	waitForEventCount(t, store, owner, running.ID, 3)

	queued, _, err := runner.Submit(context.Background(), owner, second.ID, "wait", "k2", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit second: %v", err)
	}
	if got, err := store.GetTurn(context.Background(), owner, queued.ID); err != nil || got.State != TurnStateQueued {
		t.Fatalf("second turn = %+v, %v; want queued", got, err)
	}

	close(release)
	if final := awaitTerminal(t, store, owner, running.ID); final.State != TurnStateCompleted {
		t.Fatalf("first state = %s (%+v)", final.State, final.Error)
	}
	if final := awaitTerminal(t, store, owner, queued.ID); final.State != TurnStateCompleted {
		t.Fatalf("second state = %s (%+v)", final.State, final.Error)
	}
}

func TestRunnerIdempotentSubmit(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(textStep("once"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "idempotent")

	first, created, err := runner.Submit(context.Background(), owner, conv.ID, "hello", "same-key", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !created {
		t.Fatal("first Submit created=false")
	}
	second, created, err := runner.Submit(context.Background(), owner, conv.ID, "hello again", "same-key", nil, time.Time{})
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if created {
		t.Fatal("duplicate Submit created a turn")
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate returned turn %q, want %q", second.ID, first.ID)
	}

	awaitTerminal(t, store, owner, first.ID)
	if reqs := prov.Requests(); len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	msgs := drainMessages(t, store, owner, conv.ID)
	users := 0
	for _, m := range msgs {
		if m.Role == RoleUser {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("stored %d user messages, want 1: %+v", users, msgs)
	}
}

func TestRunnerActiveTurnRejected(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(scripted.Step{BlockUntilCancel: true})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "active")

	first, _, err := runner.Submit(context.Background(), owner, conv.ID, "one", "k1", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForEventCount(t, store, owner, first.ID, 2) // queued, started

	if _, _, err := runner.Submit(context.Background(), owner, conv.ID, "two", "k2", nil, time.Time{}); !errors.Is(err, ErrActiveTurn) {
		t.Fatalf("second turn = %v, want ErrActiveTurn", err)
	}
	if _, err := runner.Cancel(context.Background(), owner, first.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestRunnerMaxSteps(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	call := provider.ToolCall{ID: "c1", Name: "loop", Arguments: json.RawMessage(`{}`)}
	prov := scripted.New(toolCallStep(call), textStep("never"))
	tools := &fakeTools{result: json.RawMessage(`true`)}
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", MaxSteps: 1})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "steps")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "loop", "k", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	final := awaitTerminal(t, store, owner, turn.ID)
	if final.State != TurnStateFailed {
		t.Fatalf("state = %s", final.State)
	}
	if final.Error == nil || final.Error.Code != FailureMaxSteps {
		t.Fatalf("error = %+v", final.Error)
	}
	events := mustEvents(t, store, owner, turn.ID)
	if countTerminals(events) != 1 {
		t.Fatalf("terminal events = %d", countTerminals(events))
	}
	if term := terminalEvent(t, events); term.Type != EventTurnFailed {
		t.Fatalf("terminal event = %s", term.Type)
	}
	if reqs := prov.Requests(); len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
}

func TestRunnerProviderFailures(t *testing.T) {
	const secret = "raw provider detail: model overloaded"

	cases := []struct {
		name string
		step scripted.Step
	}{
		{"error chunk", scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: "partial"},
			{Kind: provider.ChunkError, Err: errors.New(secret)},
		}}},
		{"stream error", scripted.Step{StreamErr: errors.New(secret)}},
		{"truncated stream", scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: "partial"},
		}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemStore(MemStoreOptions{})
			prov := scripted.New(tc.step)
			runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
			owner := ownerFixture()
			conv := mustConversation(t, store, owner, tc.name)

			turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			final := awaitTerminal(t, store, owner, turn.ID)
			if final.State != TurnStateFailed {
				t.Fatalf("state = %s", final.State)
			}
			if final.Error == nil || final.Error.Code != FailureProvider {
				t.Fatalf("error = %+v", final.Error)
			}
			if final.Error.Message != failureMessage(FailureProvider) {
				t.Fatalf("message = %q, want the generic text", final.Error.Message)
			}
			events := mustEvents(t, store, owner, turn.ID)
			if countTerminals(events) != 1 {
				t.Fatalf("terminal events = %d", countTerminals(events))
			}
			assertNoSecret(t, secret, events, drainMessages(t, store, owner, conv.ID), prov.Requests())
		})
	}
}

func TestRunnerTimeout(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(scripted.Step{BlockUntilCancel: true})
	runner := mustRunner(t, RunnerConfig{
		Store: store, Provider: prov, Model: "m",
		TurnTimeout: 40 * time.Millisecond,
	})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "timeout")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "slow", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	final := awaitTerminal(t, store, owner, turn.ID)
	if final.State != TurnStateFailed {
		t.Fatalf("state = %s", final.State)
	}
	if final.Error == nil || final.Error.Code != FailureTimeout {
		t.Fatalf("error = %+v", final.Error)
	}
	events := mustEvents(t, store, owner, turn.ID)
	if countTerminals(events) != 1 {
		t.Fatalf("terminal events = %d", countTerminals(events))
	}
}

func TestRunnerAuthDeadline(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(scripted.Step{BlockUntilCancel: true})
	runner := mustRunner(t, RunnerConfig{
		Store: store, Provider: prov, Model: "m",
		TurnTimeout: 5 * time.Second,
	})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "auth")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Now().Add(40*time.Millisecond))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	final := awaitTerminal(t, store, owner, turn.ID)
	if final.State != TurnStateFailed {
		t.Fatalf("state = %s", final.State)
	}
	if final.Error == nil || final.Error.Code != FailureAuthExpired {
		t.Fatalf("error = %+v, want %s", final.Error, FailureAuthExpired)
	}
}

func TestRunnerShutdown(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	inner := scripted.New(scripted.Step{BlockUntilCancel: true})
	prov := &watchingProvider{inner: inner}
	runner, err := NewRunner(RunnerConfig{Store: store, Provider: prov, Model: "m"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "shutdown")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "long", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForEventCount(t, store, owner, turn.ID, 2)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := runner.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	final, err := store.GetTurn(context.Background(), owner, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != TurnStateInterrupted {
		t.Fatalf("state = %s, want interrupted", final.State)
	}
	if final.Error == nil || final.Error.Code != FailureInterrupted {
		t.Fatalf("error = %+v", final.Error)
	}
	events := mustEvents(t, store, owner, turn.ID)
	if countTerminals(events) != 1 {
		t.Fatalf("terminal events = %d in %v", countTerminals(events), typesOf(events))
	}
	if term := terminalEvent(t, events); term.Type != EventTurnFailed {
		t.Fatalf("terminal event = %s", term.Type)
	}

	// A shut-down runner refuses new work explicitly.
	if _, _, err := runner.Submit(context.Background(), owner, conv.ID, "again", "k2", nil, time.Time{}); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Submit after Shutdown = %v, want ErrShutdown", err)
	}
}

func TestRunnerHistoryKeepsToolGroups(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	seed := func(t *testing.T, store Store, owner OwnerKey, conv Conversation) {
		t.Helper()
		msgs := []Message{
			{ConversationID: conv.ID, Role: RoleUser, Parts: []Part{{Type: PartText, Text: "first"}}, CreatedAt: base},
			{ConversationID: conv.ID, Role: RoleAssistant, Parts: []Part{
				{Type: PartToolCall, CallID: "c1", Tool: "one", Arguments: json.RawMessage(`{}`)},
				{Type: PartToolCall, CallID: "c2", Tool: "two", Arguments: json.RawMessage(`{}`)},
			}, CreatedAt: base.Add(time.Second)},
			{ConversationID: conv.ID, Role: RoleTool, Parts: []Part{
				{Type: PartToolResult, CallID: "c1", Tool: "one", Result: json.RawMessage(`1`)},
			}, CreatedAt: base.Add(2 * time.Second)},
			{ConversationID: conv.ID, Role: RoleTool, Parts: []Part{
				{Type: PartToolResult, CallID: "c2", Tool: "two", Result: json.RawMessage(`2`)},
			}, CreatedAt: base.Add(3 * time.Second)},
			{ConversationID: conv.ID, Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "old answer"}}, CreatedAt: base.Add(4 * time.Second)},
		}
		for _, m := range msgs {
			if _, err := store.AppendMessage(context.Background(), owner, m); err != nil {
				t.Fatalf("seed message: %v", err)
			}
		}
	}

	t.Run("cut inside a group keeps the whole group", func(t *testing.T) {
		store := NewMemStore(MemStoreOptions{})
		prov := scripted.New(textStep("new answer"))
		runner := mustRunner(t, RunnerConfig{
			Store: store, Provider: prov, Model: "m",
			MaxHistoryMessages: 5,
		})
		owner := ownerFixture()
		conv := mustConversation(t, store, owner, "history")
		seed(t, store, owner, conv)

		turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "new question", "k", nil, time.Time{})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		awaitTerminal(t, store, owner, turn.ID)

		reqs := prov.Requests()
		if len(reqs) != 1 {
			t.Fatalf("provider calls = %d", len(reqs))
		}
		history := reqs[0].Messages
		if len(history) != 5 {
			t.Fatalf("history length = %d: %+v", len(history), history)
		}
		if history[0].Role != provider.RoleAssistant || len(history[0].ToolCalls) != 2 {
			t.Fatalf("history[0] = %+v", history[0])
		}
		if history[1].Role != provider.RoleTool || history[1].ToolCallID != "c1" {
			t.Fatalf("history[1] = %+v", history[1])
		}
		if history[2].Role != provider.RoleTool || history[2].ToolCallID != "c2" {
			t.Fatalf("history[2] = %+v", history[2])
		}
		if history[3].Role != provider.RoleAssistant || history[3].Text != "old answer" {
			t.Fatalf("history[3] = %+v", history[3])
		}
		if history[4].Role != provider.RoleUser || history[4].Text != "new question" {
			t.Fatalf("history[4] = %+v", history[4])
		}
	})

	t.Run("cut on a tool result never orphans it", func(t *testing.T) {
		store := NewMemStore(MemStoreOptions{})
		prov := scripted.New(textStep("new answer"))
		runner := mustRunner(t, RunnerConfig{
			Store: store, Provider: prov, Model: "m",
			MaxHistoryMessages: 3,
		})
		owner := ownerFixture()
		conv := mustConversation(t, store, owner, "history")
		seed(t, store, owner, conv)

		turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "new question", "k", nil, time.Time{})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		awaitTerminal(t, store, owner, turn.ID)

		history := prov.Requests()[0].Messages
		if len(history) == 0 {
			t.Fatal("empty history")
		}
		if history[0].Role == provider.RoleTool {
			t.Fatalf("history starts with an orphan tool result: %+v", history)
		}
		declared := map[string]bool{}
		for _, m := range history {
			switch m.Role {
			case provider.RoleAssistant:
				for _, c := range m.ToolCalls {
					declared[c.ID] = true
				}
			case provider.RoleTool:
				if !declared[m.ToolCallID] {
					t.Fatalf("tool result %q has no preceding call: %+v", m.ToolCallID, history)
				}
			}
		}
	})
}

func TestRunnerCancelledMidToolKeepsCompletedResults(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	release := make(chan struct{})
	started := make(chan struct{})
	once := sync.Once{}
	tools := &fakeTools{fn: func(ctx context.Context, name string, _ json.RawMessage) (json.RawMessage, error) {
		if name == "first" {
			return json.RawMessage(`{"kept":true}`), nil
		}
		once.Do(func() { close(started) })
		select {
		case <-release:
			return json.RawMessage(`true`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	prov := scripted.New(toolCallStep(
		provider.ToolCall{ID: "c1", Name: "first", Arguments: json.RawMessage(`{}`)},
		provider.ToolCall{ID: "c2", Name: "block", Arguments: json.RawMessage(`{}`)},
	))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "mid tool")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "go", "k", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("second tool never started")
	}

	cancelled, err := runner.Cancel(context.Background(), owner, turn.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.State != TurnStateCancelled {
		t.Fatalf("state = %s", cancelled.State)
	}
	events := mustEvents(t, store, owner, turn.ID)
	if countTerminals(events) != 1 {
		t.Fatalf("terminal events = %d", countTerminals(events))
	}

	msgs := drainMessages(t, store, owner, conv.ID)
	var kept bool
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == PartToolResult && p.CallID == "c1" && string(p.Result) == `{"kept":true}` {
				kept = true
			}
		}
	}
	if !kept {
		t.Fatalf("completed tool result was lost: %+v", msgs)
	}
	close(release)
}

func TestRunnerNilToolsAndEmptyText(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(textStep("ok"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "nil tools")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	final := awaitTerminal(t, store, owner, turn.ID)
	if final.State != TurnStateCompleted {
		t.Fatalf("state = %s (%+v)", final.State, final.Error)
	}
	if reqs := prov.Requests(); len(reqs) != 1 || len(reqs[0].Tools) != 0 {
		t.Fatalf("request = %+v", reqs)
	}
	if msgs := drainMessages(t, store, owner, conv.ID); len(msgs) != 2 {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestNewRunnerRequiresPorts(t *testing.T) {
	if _, err := NewRunner(RunnerConfig{}); err == nil {
		t.Fatal("runner without store and provider accepted")
	}
	store := NewMemStore(MemStoreOptions{})
	if _, err := NewRunner(RunnerConfig{Store: store}); err == nil {
		t.Fatal("runner without provider accepted")
	}
	if _, err := NewRunner(RunnerConfig{Provider: scripted.New()}); err == nil {
		t.Fatal("runner without store accepted")
	}
}

func TestRunnerSubmitRequiresOwner(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: scripted.New()})
	if _, _, err := runner.Submit(context.Background(), OwnerKey{}, "c", "t", "k", nil, time.Time{}); err == nil {
		t.Fatal("Submit without an owner key accepted")
	}
}

func TestTrimHistory(t *testing.T) {
	msgs := func(roles ...string) []Message {
		out := make([]Message, len(roles))
		for i, r := range roles {
			out[i] = Message{Role: r, ID: fmt.Sprintf("m%d", i)}
		}
		return out
	}
	all := msgs(RoleUser, RoleAssistant, RoleTool, RoleTool, RoleAssistant)
	if got := trimHistory(all, 10); len(got) != 5 {
		t.Fatalf("no cut = %d messages", len(got))
	}
	got := trimHistory(all, 4)
	if len(got) != 4 || got[0].Role != RoleAssistant {
		t.Fatalf("cut of 4 = %+v", got)
	}
	got = trimHistory(all, 3)
	if len(got) != 1 || got[0].Role != RoleAssistant || got[0].ID != "m4" {
		t.Fatalf("cut skipped orphan results wrong: %+v", got)
	}
	got = trimHistory(all, 2)
	if len(got) != 1 || got[0].ID != "m4" {
		t.Fatalf("cut of 2 = %+v", got)
	}
	if got := trimHistory(all, 0); len(got) != 5 {
		t.Fatalf("zero max must keep everything: %+v", got)
	}
}

func TestNormalizeHistoryDropsDanglingCalls(t *testing.T) {
	in := []provider.Message{
		{Role: provider.RoleUser, Text: "hi"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "one"}, {ID: "c2", Name: "two"}}},
		{Role: provider.RoleTool, ToolCallID: "c1", ToolResult: json.RawMessage(`1`)},
	}
	out := normalizeHistory(in)
	if len(out) != 3 {
		t.Fatalf("out = %+v", out)
	}
	if out[2].Role != provider.RoleTool || out[2].ToolCallID != "c1" {
		t.Fatalf("resolved result dropped: %+v", out)
	}
	if len(out[1].ToolCalls) != 1 || out[1].ToolCalls[0].ID != "c1" {
		t.Fatalf("dangling call kept: %+v", out[1].ToolCalls)
	}

	// An assistant message left with nothing is dropped entirely.
	in = []provider.Message{
		{Role: provider.RoleUser, Text: "hi"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "one"}}},
	}
	if out := normalizeHistory(in); len(out) != 1 {
		t.Fatalf("empty assistant message kept: %+v", out)
	}

	// A result without its call is dropped too.
	in = []provider.Message{
		{Role: provider.RoleUser, Text: "hi"},
		{Role: provider.RoleTool, ToolCallID: "gone", ToolResult: json.RawMessage(`1`)},
	}
	if out := normalizeHistory(in); len(out) != 1 || out[0].Role != provider.RoleUser {
		t.Fatalf("orphan result kept: %+v", out)
	}
}

func TestProviderMessagesKeepsNoticeOutOfToolPairs(t *testing.T) {
	// A decision can land while the turn is still running, so a stored notice
	// may sit between an assistant message's tool calls and their results. It
	// must be moved after the group: providers reject a tool result that is not
	// immediately preceded by its call's group.
	msgs := []Message{
		{Role: RoleUser, ID: "m1", Parts: []Part{{Type: PartText, Text: "go"}}},
		{Role: RoleAssistant, ID: "m2", Parts: []Part{
			{Type: PartToolCall, CallID: "c1", Tool: "one", Arguments: json.RawMessage(`{}`)},
			{Type: PartToolCall, CallID: "c2", Tool: "two", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: RoleTool, ID: "m3", Parts: []Part{{Type: PartToolResult, CallID: "c1", Tool: "one", Result: json.RawMessage(`1`)}}},
		{Role: RoleNotice, ID: "m4", Parts: []Part{{
			Type: PartProposalOutcome, ProposalID: "p1", State: string(ProposalStateSucceeded),
			Text: "[Proposal p1 \"Create a deal\": approved and applied.]",
		}}},
		{Role: RoleTool, ID: "m5", Parts: []Part{{Type: PartToolResult, CallID: "c2", Tool: "two", Result: json.RawMessage(`2`)}}},
	}

	out := normalizeHistory(providerMessages(msgs))
	roles := make([]provider.Role, len(out))
	for i, m := range out {
		roles[i] = m.Role
	}
	want := []provider.Role{provider.RoleUser, provider.RoleAssistant, provider.RoleTool, provider.RoleTool, provider.RoleUser}
	if len(out) != len(want) {
		t.Fatalf("messages = %v, want %v", roles, want)
	}
	for i := range want {
		if out[i].Role != want[i] {
			t.Fatalf("message %d = %s, want %s (all: %v)", i, out[i].Role, want[i], roles)
		}
	}
	if !strings.HasPrefix(out[4].Text, "[Proposal p1") {
		t.Fatalf("notice text = %q", out[4].Text)
	}
	// The tool-call group stayed intact, so both calls are still declared.
	if len(out[1].ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", out[1].ToolCalls)
	}
}

// failingAppendStore fails AppendMessage once, after CreateTurn has claimed the
// conversation's active slot.
type failingAppendStore struct {
	*MemStore
	fail atomic.Bool
}

func (s *failingAppendStore) AppendMessage(ctx context.Context, owner OwnerKey, m Message) (Message, error) {
	if s.fail.CompareAndSwap(true, false) {
		return Message{}, errors.New("transient store failure")
	}
	return s.MemStore.AppendMessage(ctx, owner, m)
}

// Review fix: a submission that fails after claiming the active slot releases
// it, so the conversation is not blocked until a restart.
func TestRunnerFailedSubmitReleasesActiveTurn(t *testing.T) {
	store := &failingAppendStore{MemStore: NewMemStore(MemStoreOptions{})}
	runner, err := NewRunner(RunnerConfig{Store: store, Provider: scripted.New(scripted.Step{Chunks: []provider.Chunk{{Kind: provider.ChunkText, Text: "hi"}, {Kind: provider.ChunkDone}}}), Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Shutdown(context.Background()) })
	owner := ownerFixture()
	conv, err := store.CreateConversation(context.Background(), Conversation{Owner: owner, Title: "t"})
	if err != nil {
		t.Fatal(err)
	}

	store.fail.Store(true)
	failed, _, err := runner.Submit(context.Background(), owner, conv.ID, "first", "k1", nil, time.Time{})
	if err == nil {
		t.Fatal("submission with a failing store succeeded")
	}
	got, err := store.GetTurn(context.Background(), owner, failed.ID)
	if err != nil || got.State != TurnStateInterrupted {
		t.Fatalf("failed submission left turn %+v (%v), want interrupted", got, err)
	}
	if _, _, err := runner.Submit(context.Background(), owner, conv.ID, "second", "k2", nil, time.Time{}); err != nil {
		t.Fatalf("next submission refused: %v", err)
	}
}
