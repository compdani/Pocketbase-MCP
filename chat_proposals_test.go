package pocketbasemcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
	"github.com/compdani/pocketbase-mcp/protocol"
	"github.com/compdani/pocketbase-mcp/schema"
)

// --- fixture ---

// proposalsFixture is the chat-with-writes setup: the shared chat test host
// plus a memstore-backed runner that can store proposals, and custom write
// tools covering the review, auto-approval and schema cases.
type proposalsFixture struct {
	srv    *Server
	host   *chatTestHost
	audit  *auditRecorder
	store  *chat.MemStore
	runner *chat.Runner
	prov   *scripted.Provider

	titled *toolCounter
	auto   *toolCounter
	strict *toolCounter
}

func chatOwner() chat.OwnerKey {
	return chat.OwnerKey{Namespace: "tests", Mount: "/mcp", AuthCollectionID: "col_users", UserID: "u1"}
}

func newProposalsFixture(t *testing.T, steps ...scripted.Step) *proposalsFixture {
	t.Helper()
	host := newChatTestHost()
	audit := &auditRecorder{}
	srv := newChatTestServer(host, audit, Config{})
	f := &proposalsFixture{host: host, audit: audit, titled: &toolCounter{}, auto: &toolCounter{}, strict: &toolCounter{}}
	srv.cfg.CustomTools = append(srv.cfg.CustomTools,
		ToolSpec{Name: "titled_publish", Title: "Publish article", Description: "publish an article", Channels: "chat", Write: true, InputSchema: map[string]any{"type": "object"}, Handler: f.titled.handler},
		ToolSpec{Name: "auto_publish", Description: "publish without review", Channels: "both", Write: true, Approval: "auto", InputSchema: map[string]any{"type": "object"}, Handler: f.auto.handler},
		ToolSpec{Name: "strict_publish", Description: "publish a named thing", Channels: "chat", Write: true, InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []string{"name"},
		}, Handler: f.strict.handler},
	)
	f.srv = srv
	f.store = chat.NewMemStore(chat.MemStoreOptions{})
	f.prov = scripted.New(steps...)
	runner, err := chat.NewRunner(chat.RunnerConfig{Store: f.store, Provider: f.prov, Model: "test-model", Proposals: f.store})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})
	f.runner = runner
	return f
}

// exec returns the caller's tool executor with chat writes on, as ChatHandler
// would build it from ChatOptions{Proposals: true}.
func (f *proposalsFixture) exec(t *testing.T, autoApprove bool) chat.ToolExecutor {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/chat/v1/conversations", nil)
	return f.srv.chatCallerWithWrites(chatPrincipal(f.host, "good"), req, "tests", "", true, autoApprove).Tools
}

func (f *proposalsFixture) conversation(t *testing.T) chat.Conversation {
	t.Helper()
	conv, err := f.store.CreateConversation(context.Background(), chat.Conversation{Owner: chatOwner(), Title: "proposals"})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return conv
}

// runTurn submits one turn with exec, waits for its terminal state and returns
// the stored events.
func (f *proposalsFixture) runTurn(t *testing.T, conv chat.Conversation, exec chat.ToolExecutor) []chat.Event {
	t.Helper()
	turn, _, err := f.runner.Submit(context.Background(), chatOwner(), conv.ID, "go", "", exec, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, err := f.store.GetTurn(context.Background(), chatOwner(), turn.ID)
		if err == nil && stored.State.Terminal() {
			if stored.State != chat.TurnStateCompleted {
				t.Fatalf("turn ended as %s: %+v", stored.State, stored.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s did not finish", turn.ID)
		}
		time.Sleep(2 * time.Millisecond)
	}
	events, err := f.store.EventsAfter(context.Background(), chatOwner(), turn.ID, 0)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	return events
}

func (f *proposalsFixture) proposals(t *testing.T, conv chat.Conversation) []chat.Proposal {
	t.Helper()
	items, _, err := f.store.ListProposals(context.Background(), chatOwner(), conv.ID, nil, chat.Page{})
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	return items
}

func proposalCallStep(calls ...provider.ToolCall) scripted.Step {
	chunks := make([]provider.Chunk, 0, len(calls)+1)
	for i := range calls {
		chunks = append(chunks, provider.Chunk{Kind: provider.ChunkToolCall, ToolCall: &calls[i]})
	}
	chunks = append(chunks, provider.Chunk{Kind: provider.ChunkDone})
	return scripted.Step{Chunks: chunks}
}

func eventData[T any](t *testing.T, events []chat.Event, typ chat.EventType) []T {
	t.Helper()
	var out []T
	for _, e := range events {
		if e.Type != typ {
			continue
		}
		var v T
		if err := json.Unmarshal(e.Data, &v); err != nil {
			t.Fatalf("decode %s: %v", typ, err)
		}
		out = append(out, v)
	}
	return out
}

func eventIndexes(events []chat.Event) map[chat.EventType]int {
	out := map[chat.EventType]int{}
	for i, e := range events {
		if _, seen := out[e.Type]; !seen {
			out[e.Type] = i
		}
	}
	return out
}

// --- tests ---

func TestChatProposalForGeneratedCreate(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{"name":"Acme"}}`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("It is waiting for your review."))
	conv := f.conversation(t)
	events := f.runTurn(t, conv, f.exec(t, false))

	items := f.proposals(t, conv)
	if len(items) != 1 {
		t.Fatalf("proposals = %+v", items)
	}
	p := items[0]
	if p.State != chat.ProposalStatePending || p.Location != chat.ProposalLocationServer {
		t.Fatalf("proposal = %+v", p)
	}
	if p.Tool != "create_deals" || p.Target != "deals" || p.Summary != "Create a record in deals" {
		t.Fatalf("proposal identity = %+v", p)
	}
	if p.ConversationID != conv.ID || p.TurnID == "" || p.CallID != "call-1" {
		t.Fatalf("proposal binding = %+v", p)
	}
	if p.ManifestRevision != f.srv.manifest().Revision() {
		t.Fatalf("manifest revision = %q, want %q", p.ManifestRevision, f.srv.manifest().Revision())
	}
	wantFP, ok := f.srv.ChatToolFingerprint("create_deals")
	if !ok || p.ToolFingerprint != wantFP {
		t.Fatalf("fingerprint = %q (%v), want %q", p.ToolFingerprint, ok, wantFP)
	}
	if string(p.Arguments) != `{"data":{"name":"Acme"}}` {
		t.Fatalf("arguments = %s", p.Arguments)
	}

	// Nothing ran: the host never saw a write.
	if f.host.writeCount() != 0 {
		t.Fatalf("chat executed %d writes", f.host.writeCount())
	}

	// Event order is tool.started, proposal.created, tool.completed; the result
	// tells the model nothing was applied.
	idx := eventIndexes(events)
	started, okStart := idx[chat.EventToolStarted]
	created, okCreated := idx[chat.EventProposalCreated]
	completed, okCompleted := idx[chat.EventToolCompleted]
	if !okStart || !okCreated || !okCompleted || !(started < created && created < completed) {
		t.Fatalf("event order = %v", idx)
	}
	results := eventData[chat.ToolCompleted](t, events, chat.EventToolCompleted)
	if len(results) != 1 {
		t.Fatalf("tool.completed = %+v", results)
	}
	var result map[string]any
	if err := json.Unmarshal(results[0].Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result["status"] != "pending_review" || result["proposalId"] != p.ID || result["summary"] != p.Summary {
		t.Fatalf("tool result = %v", result)
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "Not applied") {
		t.Fatalf("tool result note = %q", note)
	}

	// An MCP client still calls the same write directly: the chat surface did
	// not change MCP behavior.
	if _, out := rpc(t, f.srv, "good", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_deals","arguments":{"data":{}}}}`); out["result"] == nil || out["result"].(map[string]any)["isError"] == true {
		t.Fatalf("MCP create failed: %v", out)
	}
	if f.host.writeCount() != 1 {
		t.Fatalf("MCP writes = %d, want 1", f.host.writeCount())
	}
}

func TestChatProposalSummariesForGeneratedWrites(t *testing.T) {
	calls := []provider.ToolCall{
		{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{}}`)},
		{ID: "call-2", Name: "update_deals", Arguments: json.RawMessage(`{"id":"r1","data":{}}`)},
		{ID: "call-3", Name: "delete_deals", Arguments: json.RawMessage(`{"id":"r1"}`)},
	}
	f := newProposalsFixture(t, proposalCallStep(calls...), textStep("queued"))
	conv := f.conversation(t)
	f.runTurn(t, conv, f.exec(t, false))

	items := f.proposals(t, conv)
	if len(items) != 3 {
		t.Fatalf("proposals = %+v", items)
	}
	want := []struct{ summary, target string }{
		{"Create a record in deals", "deals"},
		{"Update record r1 in deals", "deals/r1"},
		{"Delete record r1 in deals", "deals/r1"},
	}
	for i, w := range want {
		if items[i].Summary != w.summary {
			t.Errorf("proposal %d summary = %q, want %q", i, items[i].Summary, w.summary)
		}
		if items[i].Target != w.target {
			t.Errorf("proposal %d target = %q, want %q", i, items[i].Target, w.target)
		}
	}
	if f.host.writeCount() != 0 {
		t.Fatalf("chat executed %d writes", f.host.writeCount())
	}
}

func TestChatProposalForCustomWriteTool(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "titled_publish", Arguments: json.RawMessage(`{"title":"Hello"}`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("queued"))
	conv := f.conversation(t)
	f.runTurn(t, conv, f.exec(t, false))

	items := f.proposals(t, conv)
	if len(items) != 1 {
		t.Fatalf("proposals = %+v", items)
	}
	if items[0].Summary != "Publish article (changes data)" {
		t.Fatalf("summary = %q", items[0].Summary)
	}
	if items[0].Target != "" {
		t.Fatalf("custom tool target = %q, want empty", items[0].Target)
	}
	if f.titled.calls() != 0 {
		t.Fatalf("custom write handler ran %d times", f.titled.calls())
	}
}

func TestChatProposalInvalidArgumentsFailTheCall(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "strict_publish", Arguments: json.RawMessage(`{}`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("I need the name."))
	conv := f.conversation(t)
	events := f.runTurn(t, conv, f.exec(t, false))

	if items := f.proposals(t, conv); len(items) != 0 {
		t.Fatalf("invalid arguments created a proposal: %+v", items)
	}
	if f.strict.calls() != 0 {
		t.Fatalf("handler ran for invalid arguments")
	}
	failed := eventData[chat.ToolFailed](t, events, chat.EventToolFailed)
	if len(failed) != 1 {
		t.Fatalf("tool.failed = %+v", failed)
	}
	// The message is exactly what the same -32602 error would give a direct
	// chat call, so a proposal refusal and a call refusal cannot drift.
	strict := f.srv.cfg.CustomTools[len(f.srv.cfg.CustomTools)-1]
	sch, err := schema.Compile(strict.InputSchema)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := sch.Validate(map[string]any{}); err == nil {
		t.Fatal("fixture schema accepted empty arguments")
	} else {
		want := chatRPCErrorMessage(&protocol.Error{Code: protocol.CodeInvalidParams, Message: "Invalid params", Data: err.Error()})
		if failed[0].Message != want {
			t.Fatalf("failure = %q, want %q", failed[0].Message, want)
		}
	}
}

func TestChatProposalsOffKeepsReadOnlySurface(t *testing.T) {
	f := newProposalsFixture(t)
	p := chatPrincipal(f.host, "good")

	readOnly := providerToolNames(t, mustChatDefs(t, chatExecution(t, f.srv, p)))
	for _, absent := range []string{"create_deals", "update_deals", "delete_deals", "both_writer", "titled_publish", "auto_publish"} {
		if readOnly[absent] {
			t.Errorf("read-only chat surface offers %q", absent)
		}
	}

	// The public read-only listing and the internal variant with writes off are
	// the same surface.
	plain := normalizedToolJSON(t, f.srv, p, "chat")
	off := f.srv.listChatTools(p, false)
	if plain != normalizedToolList(t, off) {
		t.Fatal("listChatTools(p, false) differs from listTools(p, \"chat\")")
	}

	// A write call through the ordinary (read-only) caller is refused, and the
	// internal writes flag cannot widen the MCP surface either.
	exec := chatExecution(t, f.srv, p)
	if _, err := exec.Call(context.Background(), "create_deals", json.RawMessage(`{"data":{}}`)); err == nil {
		t.Fatal("read-only chat executor allowed a write")
	} else {
		var te chat.ToolError
		if !errors.As(err, &te) || te.Message != "Tool is unavailable." {
			t.Fatalf("refusal = %v", err)
		}
	}
	mcpPlain := normalizedToolJSON(t, f.srv, p, "mcp")
	if mcpPlain != normalizedToolList(t, f.srv.listToolsWithWrites(p, "mcp", true)) {
		t.Fatal("the chat writes flag changed the MCP tools list")
	}
	if f.host.writeCount() != 0 {
		t.Fatalf("chat executed %d writes with proposals off", f.host.writeCount())
	}
}

func mustChatDefs(t *testing.T, exec chat.ToolExecutor) []provider.ToolDef {
	t.Helper()
	defs, err := exec.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	return defs
}

func normalizedToolList(t *testing.T, defs []protocol.ToolDefinition) string {
	t.Helper()
	b, err := json.Marshal(defs)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("normalize tools: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("remarshal tools: %v", err)
	}
	return string(out)
}

func TestChatProposalsHandlerValidation(t *testing.T) {
	f := newProposalsFixture(t)
	base := ChatOptions{Namespace: "tests", Runner: f.runner, Store: f.store, Proposals: true}
	noStoreRunner, err := chat.NewRunner(chat.RunnerConfig{Store: f.store, Provider: f.prov, Model: "m"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = noStoreRunner.Shutdown(ctx)
	})

	cases := []struct {
		name string
		srv  *Server
		opts ChatOptions
		want string
	}{
		{
			"writes disabled",
			newChatTestServer(newChatTestHost(), nil, Config{}), // newChatTestServer forces AllowWrites on; see below
			base,
			"AllowWrites",
		},
		{"runner without proposal store", f.srv, func() ChatOptions {
			o := base
			o.Runner = noStoreRunner
			return o
		}(), "proposal store"},
		{"auto-approve without proposals", f.srv, ChatOptions{Namespace: "tests", Runner: f.runner, Store: f.store, AutoApprove: true}, "auto-approve"},
	}
	// The first case needs writes off, which the shared chat fixture enables by
	// default, so build it explicitly.
	writesOff := newChatTestServer(newChatTestHost(), nil, Config{})
	writesOff.cfg.AllowWrites = false
	cases[0].srv = writesOff

	for _, c := range cases {
		if _, err := c.srv.ChatHandler(c.opts); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", c.name, err, c.want)
		}
	}

	if _, err := f.srv.ChatHandler(base); err != nil {
		t.Fatalf("valid proposals options rejected: %v", err)
	}
	auto := base
	auto.AutoApprove = true
	if _, err := f.srv.ChatHandler(auto); err != nil {
		t.Fatalf("valid auto-approve options rejected: %v", err)
	}
}

func TestChatProposalAutoApprove(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "auto_publish", Arguments: json.RawMessage(`{"title":"Hello"}`)}

	t.Run("auto-approved runs now", func(t *testing.T) {
		f := newProposalsFixture(t, proposalCallStep(call), textStep("published"))
		conv := f.conversation(t)
		events := f.runTurn(t, conv, f.exec(t, true))

		if items := f.proposals(t, conv); len(items) != 0 {
			t.Fatalf("auto-approved call created a proposal: %+v", items)
		}
		if f.auto.calls() != 1 {
			t.Fatalf("handler ran %d times, want 1", f.auto.calls())
		}
		if eventIndexes(events)[chat.EventProposalCreated] != 0 || eventIndexes(events)[chat.EventToolFailed] != 0 {
			t.Fatalf("events = %v", eventIndexes(events))
		}
	})

	t.Run("without the host flag it is a proposal", func(t *testing.T) {
		f := newProposalsFixture(t, proposalCallStep(call), textStep("queued"))
		conv := f.conversation(t)
		f.runTurn(t, conv, f.exec(t, false))

		if items := f.proposals(t, conv); len(items) != 1 {
			t.Fatalf("proposals = %+v", items)
		}
		if f.auto.calls() != 0 {
			t.Fatal("a tool without host auto-approval ran unreviewed")
		}
	})
}

func TestChatToolFingerprintTracksDefinition(t *testing.T) {
	f := newProposalsFixture(t)
	srv := f.srv

	// A read tool is not a write tool.
	if _, ok := srv.ChatToolFingerprint("chat_digest"); ok {
		t.Fatal("a read tool reported a write-tool fingerprint")
	}
	if _, ok := srv.ChatToolFingerprint("no_such_tool"); ok {
		t.Fatal("an unknown tool reported a fingerprint")
	}

	fp, ok := srv.ChatToolFingerprint("create_deals")
	if !ok || !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("fingerprint = %q (%v)", fp, ok)
	}
	if again, _ := srv.ChatToolFingerprint("create_deals"); again != fp {
		t.Fatalf("fingerprint is not stable: %q vs %q", fp, again)
	}
	// The collection schema is part of the generated tool's identity.
	deals := f.host.cols[0]
	deals.FieldMap = map[string]FieldInfo{"name": {Name: "name", Type: "text"}}
	deals.CreateFields = []string{"name"}
	changed, ok := srv.ChatToolFingerprint("create_deals")
	if !ok || changed == fp {
		t.Fatalf("fingerprint did not change with the input schema: %q", changed)
	}

	// A custom tool's declared schema is part of its identity too.
	customFP, ok := srv.ChatToolFingerprint("strict_publish")
	if !ok {
		t.Fatal("strict_publish has no fingerprint")
	}
	for i := range srv.cfg.CustomTools {
		if srv.cfg.CustomTools[i].Name != "strict_publish" {
			continue
		}
		srv.cfg.CustomTools[i].InputSchema = map[string]any{
			"type":       "object",
			"properties": map[string]any{"slug": map[string]any{"type": "string"}},
			"required":   []string{"slug"},
		}
	}
	if next, _ := srv.ChatToolFingerprint("strict_publish"); next == customFP {
		t.Fatal("custom fingerprint did not change with the input schema")
	}
}

func TestChatProposalAuditEvent(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{}}`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("queued"))
	conv := f.conversation(t)
	f.runTurn(t, conv, f.exec(t, false))

	var proposed *AuditEvent
	for _, e := range f.audit.snapshot() {
		if e.Outcome == "proposed" {
			event := e
			proposed = &event
		}
	}
	if proposed == nil {
		t.Fatalf("no proposed audit event in %+v", f.audit.snapshot())
	}
	if proposed.Channel != "chat" || proposed.Tool != "create_deals" || proposed.Effect != "write" || proposed.Collection != "deals" || proposed.Action != "create" {
		t.Fatalf("proposed audit = %+v", proposed)
	}
	if proposed.Payload != nil {
		t.Fatalf("proposed audit carried a payload without AuditIncludePayload: %+v", proposed.Payload)
	}
}

// failingProposalStore refuses every CreateProposal.
type failingProposalStore struct{ *chat.MemStore }

func (failingProposalStore) CreateProposal(context.Context, chat.OwnerKey, chat.Proposal) (chat.Proposal, error) {
	return chat.Proposal{}, errors.New("store down")
}

// Review fix: the "proposed" audit line is written only after the proposal is
// stored, and carries its id.
func TestChatProposalAuditOnlyAfterStore(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{}}`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("queued"))
	conv := f.conversation(t)
	runner, err := chat.NewRunner(chat.RunnerConfig{Store: f.store, Provider: f.prov, Model: "test-model", Proposals: failingProposalStore{f.store}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Shutdown(context.Background()) })
	turn, _, err := runner.Submit(context.Background(), chatOwner(), conv.ID, "go", "", f.exec(t, false), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, err := f.store.GetTurn(context.Background(), chatOwner(), turn.ID)
		if err == nil && stored.State.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, e := range f.audit.snapshot() {
		if e.Outcome == "proposed" {
			t.Fatalf("a proposal that was never stored was audited: %+v", e)
		}
	}

	// With a working store the line exists and names the stored proposal.
	ok := newProposalsFixture(t, proposalCallStep(call), textStep("queued"))
	okConv := ok.conversation(t)
	ok.runTurn(t, okConv, ok.exec(t, false))
	items := ok.proposals(t, okConv)
	if len(items) != 1 {
		t.Fatalf("proposals = %+v", items)
	}
	found := false
	for _, e := range ok.audit.snapshot() {
		if e.Outcome == "proposed" && e.RequestID == "proposal-"+items[0].ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no proposed audit naming %s in %+v", items[0].ID, ok.audit.snapshot())
	}
}

func TestChatProposalPlanRules(t *testing.T) {
	f := newProposalsFixture(t)
	readExec := chatExecution(t, f.srv, chatPrincipal(f.host, "good"))
	writeExec := f.exec(t, true)

	// Reads and unknown names are never planned: Call decides them.
	for _, name := range []string{"search_deals", "describe_access", "no_such_tool"} {
		draft, err := writeExec.(chat.ProposalPlanner).Plan(context.Background(), name, json.RawMessage(`{}`))
		if draft != nil || err != nil {
			t.Fatalf("Plan(%q) = %v, %v", name, draft, err)
		}
	}

	// A write tool outside the caller's surface is refused with the same text a
	// hidden tool gets.
	draft, err := writeExec.(chat.ProposalPlanner).Plan(context.Background(), "create_secrets", json.RawMessage(`{"data":{}}`))
	if draft != nil {
		t.Fatalf("Plan planned a policy-denied write: %+v", draft)
	}
	var te chat.ToolError
	if !errors.As(err, &te) || te.Message != "Tool is unavailable." {
		t.Fatalf("Plan(create_secrets) = %v", err)
	}

	// Calling a write directly, bypassing the planner, is refused.
	_, err = writeExec.Call(context.Background(), "create_deals", json.RawMessage(`{"data":{}}`))
	if !errors.As(err, &te) || te.Message != "This change needs review." {
		t.Fatalf("direct write Call = %v", err)
	}

	// The read-only executor does not plan writes at all; the channel gate
	// refuses them as unavailable, as v1.1 did.
	if d, err := readExec.(chat.ProposalPlanner).Plan(context.Background(), "create_deals", json.RawMessage(`{"data":{}}`)); d != nil {
		t.Fatalf("read-only Plan returned a draft: %+v", d)
	} else if !errors.As(err, &te) || te.Message != "Tool is unavailable." {
		t.Fatalf("read-only Plan = %v", err)
	}
	if f.host.writeCount() != 0 {
		t.Fatalf("planning executed %d writes", f.host.writeCount())
	}
}

func TestValidateCustomToolsApproval(t *testing.T) {
	handler := okHandler
	cases := []struct {
		name string
		tool ToolSpec
		want string
	}{
		{"default review", ToolSpec{Name: "t", Channels: "chat", Write: true, Handler: handler}, ""},
		{"auto on write", ToolSpec{Name: "t", Channels: "chat", Write: true, Approval: "auto", Handler: handler}, ""},
		{"auto on read", ToolSpec{Name: "t", Channels: "chat", Approval: "auto", Handler: handler}, "approval"},
		{"unknown approval", ToolSpec{Name: "t", Channels: "chat", Write: true, Approval: "sometimes", Handler: handler}, "approval"},
	}
	for _, c := range cases {
		err := Config{CustomTools: []ToolSpec{c.tool}}.ValidateCustomTools()
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: rejected: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", c.name, err, c.want)
		}
	}
}

// newGenericProposalsServer builds a generic-tool-mode server over the shared
// chat test host, so the generic write branch (create/update/delete with a
// collection argument) can be exercised.
func newGenericProposalsServer(h Host, audit *auditRecorder) *Server {
	m := &Manifest{SchemaVersion: "1", ToolMode: "generic", Collections: []ManifestCollection{
		{Collection: "deals", Operations: []string{"describe", "search", "fetch", "create", "update", "delete"}, Policy: "authenticated"},
		{Collection: "secrets", Operations: []string{"describe", "search", "fetch"}, Policy: "field:role:admin"},
	}}
	cfg := Config{Manifest: m, AllowWrites: true, Auth: AuthConfig{Collections: []string{"users"}}}
	if audit != nil {
		cfg.AuditFunc = audit.add
	}
	return New(cfg, h)
}

func TestChatProposalsGenericMode(t *testing.T) {
	host := newChatTestHost()
	audit := &auditRecorder{}
	srv := newGenericProposalsServer(host, audit)
	store := chat.NewMemStore(chat.MemStoreOptions{})
	call := provider.ToolCall{ID: "call-1", Name: "create", Arguments: json.RawMessage(`{"collection":"deals","data":{"name":"Acme"}}`)}
	prov := scripted.New(proposalCallStep(call), textStep("queued"))
	runner, err := chat.NewRunner(chat.RunnerConfig{Store: store, Provider: prov, Model: "m", Proposals: store})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})
	p := chatPrincipal(host, "good")
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/chat/v1/conversations", nil)
	exec := srv.chatCallerWithWrites(p, req, "tests", "", true, false).Tools

	// Writes appear only on the writes variant, and per-collection gates still
	// apply: the policy-denied collection is absent from the enum.
	withWrites := listNames(srv.listChatTools(p, true))
	for _, want := range []string{"create", "update", "delete", "search", "fetch"} {
		if !withWrites[want] {
			t.Errorf("chat writes surface missing %q", want)
		}
	}
	readOnly := listNames(srv.listChatTools(p, false))
	for _, absent := range []string{"create", "update", "delete"} {
		if readOnly[absent] {
			t.Errorf("read-only chat surface offers %q", absent)
		}
	}
	for _, def := range srv.listChatTools(p, true) {
		if def.Name != "create" {
			continue
		}
		props, _ := def.InputSchema["properties"].(map[string]any)
		collection, _ := props["collection"].(map[string]any)
		enum, _ := collection["enum"].([]string)
		if len(enum) != 1 || enum[0] != "deals" {
			t.Errorf("create collection enum = %v", enum)
		}
	}

	planner := exec.(chat.ProposalPlanner)
	draft, err := planner.Plan(context.Background(), "create", json.RawMessage(`{"collection":"deals","data":{}}`))
	if err != nil || draft == nil {
		t.Fatalf("Plan(create) = %v, %v", draft, err)
	}
	if draft.Summary != "Create a record in deals" || draft.Target != "deals" || draft.Location != chat.ProposalLocationServer {
		t.Fatalf("draft = %+v", draft)
	}
	if fp, ok := srv.ChatToolFingerprint("create"); !ok || draft.ToolFingerprint != fp {
		t.Fatalf("fingerprint = %q (%v)", draft.ToolFingerprint, ok)
	}

	for _, c := range []struct {
		name, args, summary string
	}{
		{"update", `{"collection":"deals","id":"r1","data":{}}`, "Update record r1 in deals"},
		{"delete", `{"collection":"deals","id":"r1"}`, "Delete record r1 in deals"},
	} {
		d, err := planner.Plan(context.Background(), c.name, json.RawMessage(c.args))
		if err != nil || d == nil {
			t.Fatalf("Plan(%s) = %v, %v", c.name, d, err)
		}
		if d.Summary != c.summary || d.Target != "deals/r1" {
			t.Fatalf("Plan(%s) = %+v", c.name, d)
		}
	}

	// The same two checks callTool applies: the collection must exist, and the
	// caller must be allowed the operation on it.
	var te chat.ToolError
	if _, err := planner.Plan(context.Background(), "create", json.RawMessage(`{"collection":"nope","data":{}}`)); !errors.As(err, &te) || te.Message != "Unknown or unavailable collection. Call describe_access to list collections." {
		t.Fatalf("unknown collection = %v", err)
	}
	if _, err := planner.Plan(context.Background(), "create", json.RawMessage(`{"collection":"secrets","data":{}}`)); !errors.As(err, &te) || !strings.Contains(te.Message, "This operation is unavailable") {
		t.Fatalf("unwritable collection = %v", err)
	}

	// A full turn stores the proposal and never touches the host.
	conv, err := store.CreateConversation(context.Background(), chat.Conversation{Owner: chatOwner(), Title: "generic"})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	turn, _, err := runner.Submit(context.Background(), chatOwner(), conv.ID, "go", "", exec, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, err := store.GetTurn(context.Background(), chatOwner(), turn.ID)
		if err == nil && stored.State.Terminal() {
			if stored.State != chat.TurnStateCompleted {
				t.Fatalf("turn ended as %s: %+v", stored.State, stored.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}
	items, _, err := store.ListProposals(context.Background(), chatOwner(), conv.ID, nil, chat.Page{})
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(items) != 1 || items[0].Summary != "Create a record in deals" || items[0].Tool != "create" {
		t.Fatalf("proposals = %+v", items)
	}
	if host.writeCount() != 0 {
		t.Fatalf("generic chat write executed %d times", host.writeCount())
	}
}

func TestChatHandlerOffersWritesAsProposals(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{"name":"Acme"}}`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("It is waiting for your review."))
	h, err := f.srv.ChatHandler(ChatOptions{Namespace: "tests", Runner: f.runner, Store: f.store, Proposals: true})
	if err != nil {
		t.Fatalf("ChatHandler: %v", err)
	}

	code, body := chatHTTP(t, h, http.MethodPost, DefaultChatPrefix+"/conversations", "good", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create conversation: %d %s", code, body)
	}
	var conv chat.Conversation
	if err := json.Unmarshal(body, &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	code, body = chatHTTP(t, h, http.MethodPost, DefaultChatPrefix+"/conversations/"+conv.ID+"/turns", "good", `{"text":"create a deal"}`)
	if code != http.StatusAccepted {
		t.Fatalf("submit turn: %d %s", code, body)
	}
	var submitted struct {
		Turn chat.Turn `json:"turn"`
	}
	if err := json.Unmarshal(body, &submitted); err != nil {
		t.Fatalf("decode turn: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		turn, err := f.store.GetTurn(context.Background(), chatOwner(), submitted.Turn.ID)
		if err == nil && turn.State.Terminal() {
			if turn.State != chat.TurnStateCompleted {
				t.Fatalf("turn ended as %s: %+v", turn.State, turn.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}

	items, _, err := f.store.ListProposals(context.Background(), chatOwner(), conv.ID, nil, chat.Page{})
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(items) != 1 || items[0].Tool != "create_deals" || items[0].State != chat.ProposalStatePending {
		t.Fatalf("proposals = %+v", items)
	}
	if f.host.writeCount() != 0 {
		t.Fatalf("the chat turn executed %d writes", f.host.writeCount())
	}

	// The model was offered the write tool on this handler, which proves the
	// option reached the executor through the authenticator.
	reqs := f.prov.Requests()
	if len(reqs) == 0 {
		t.Fatal("provider never called")
	}
	offered := map[string]bool{}
	for _, d := range reqs[0].Tools {
		offered[d.Name] = true
	}
	if !offered["create_deals"] || !offered["titled_publish"] {
		t.Errorf("model was not offered the write tools: %v", offered)
	}
	if offered["mcp_digest"] {
		t.Errorf("model was offered an mcp-only tool: %v", offered)
	}
}

// --- decisions over HTTP ---

// proposalsHandler builds the chat handler with proposals on, as a host that
// wants review would.
func (f *proposalsFixture) proposalsHandler(t *testing.T, mutate func(*ChatOptions)) http.Handler {
	t.Helper()
	opts := ChatOptions{Namespace: "tests", Runner: f.runner, Store: f.store, Proposals: true}
	if mutate != nil {
		mutate(&opts)
	}
	h, err := f.srv.ChatHandler(opts)
	if err != nil {
		t.Fatalf("ChatHandler: %v", err)
	}
	return h
}

// decision is the decoded body of a decision request.
type decision struct {
	Proposal chat.Proposal `json:"proposal"`
	Replayed bool          `json:"replayed"`
	Error    struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// decide approves or rejects one proposal over the chat HTTP surface.
func (f *proposalsFixture) decide(t *testing.T, h http.Handler, id, choice string, revision int, args string) (int, decision) {
	t.Helper()
	body := `{"decision":"` + choice + `","revision":` + strconv.Itoa(revision) + args + `}`
	code, raw := chatHTTP(t, h, http.MethodPost, DefaultChatPrefix+"/proposals/"+id+"/decision", "good", body)
	var out decision
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode decision: %v (body %s)", err, raw)
		}
	}
	return code, out
}

// messages returns one conversation's transcript oldest first.
func (f *proposalsFixture) messages(t *testing.T, convID string) []chat.Message {
	t.Helper()
	var all []chat.Message
	page := chat.Page{Limit: 200}
	for {
		items, next, err := f.store.ListMessages(context.Background(), chatOwner(), convID, page)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		all = append(all, items...)
		if next == "" {
			return all
		}
		page.Cursor = next
	}
}

// proposeTurn runs one turn that plans a single write and returns the stored
// conversation and proposal.
func (f *proposalsFixture) proposeTurn(t *testing.T, call provider.ToolCall) (chat.Conversation, chat.Proposal) {
	t.Helper()
	conv := f.conversation(t)
	f.runTurn(t, conv, f.exec(t, false))
	items := f.proposals(t, conv)
	if len(items) != 1 {
		t.Fatalf("proposals = %+v", items)
	}
	if items[0].State != chat.ProposalStatePending {
		t.Fatalf("proposal state = %s", items[0].State)
	}
	return conv, items[0]
}

func createDealCall(args string) provider.ToolCall {
	return provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(args)}
}

func TestChatProposalApproveExecutesOnce(t *testing.T) {
	f := newProposalsFixture(t, proposalCallStep(createDealCall(`{"data":{"name":"Acme"}}`)), textStep("waiting for review"))
	conv, p := f.proposeTurn(t, createDealCall(`{"data":{"name":"Acme"}}`))
	if f.host.writeCount() != 0 {
		t.Fatalf("the turn executed %d writes", f.host.writeCount())
	}
	h := f.proposalsHandler(t, nil)

	// GET returns the stored proposal.
	code, body := chatHTTP(t, h, http.MethodGet, DefaultChatPrefix+"/proposals/"+p.ID, "good", "")
	if code != http.StatusOK {
		t.Fatalf("GET proposal: %d %s", code, body)
	}
	var single struct {
		Proposal chat.Proposal `json:"proposal"`
	}
	if err := json.Unmarshal(body, &single); err != nil {
		t.Fatalf("decode proposal: %v", err)
	}
	if single.Proposal.ID != p.ID || single.Proposal.State != chat.ProposalStatePending {
		t.Fatalf("proposal = %+v", single.Proposal)
	}

	code, out := f.decide(t, h, p.ID, "approve", p.Revision, "")
	if code != http.StatusOK || out.Replayed {
		t.Fatalf("approve: %d %+v", code, out)
	}
	if out.Proposal.State != chat.ProposalStateSucceeded {
		t.Fatalf("state = %s (%q)", out.Proposal.State, out.Proposal.Failure)
	}
	if f.host.writeCount() != 1 || f.host.writeOps[0] != "create" {
		t.Fatalf("writes = %d %v, want one create", f.host.writeCount(), f.host.writeOps)
	}
	if out.Proposal.Result == nil || !strings.Contains(string(out.Proposal.Result), `"collection":"deals"`) {
		t.Fatalf("result = %s", out.Proposal.Result)
	}

	// Approving again runs nothing and returns the stored outcome.
	code, replay := f.decide(t, h, p.ID, "approve", p.Revision, "")
	if code != http.StatusOK || !replay.Replayed || replay.Proposal.State != chat.ProposalStateSucceeded {
		t.Fatalf("replay: %d %+v", code, replay)
	}
	if f.host.writeCount() != 1 {
		t.Fatalf("a replay executed another write: %d", f.host.writeCount())
	}

	// The transcript carries one notice the next turn's model can read.
	msgs := f.messages(t, conv.ID)
	last := msgs[len(msgs)-1]
	if last.Role != chat.RoleNotice || len(last.Parts) != 1 {
		t.Fatalf("last message = %+v", last)
	}
	part := last.Parts[0]
	if part.Type != chat.PartProposalOutcome || part.ProposalID != p.ID || part.State != string(chat.ProposalStateSucceeded) {
		t.Fatalf("notice part = %+v", part)
	}
	if !strings.Contains(part.Text, "approved and applied") || !strings.Contains(part.Text, "deals") {
		t.Fatalf("notice text = %q", part.Text)
	}

	// A second turn sends that notice to the model as a user-role message.
	f2 := newProposalsFixture(t,
		proposalCallStep(createDealCall(`{"data":{}}`)), textStep("queued"),
		textStep("all set"))
	conv2 := f2.conversation(t)
	f2.runTurn(t, conv2, f2.exec(t, false))
	items := f2.proposals(t, conv2)
	h2 := f2.proposalsHandler(t, nil)
	if code, out := f2.decide(t, h2, items[0].ID, "approve", items[0].Revision, ""); code != http.StatusOK {
		t.Fatalf("approve: %d %+v", code, out)
	}
	turn, _, err := f2.runner.Submit(context.Background(), chatOwner(), conv2.ID, "next", "", f2.exec(t, false), time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, err := f2.store.GetTurn(context.Background(), chatOwner(), turn.ID)
		if err == nil && stored.State.Terminal() {
			if stored.State != chat.TurnStateCompleted {
				t.Fatalf("second turn ended as %s", stored.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second turn did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}
	reqs := f2.prov.Requests()
	lastReq := reqs[len(reqs)-1]
	found := false
	for _, msg := range lastReq.Messages {
		if msg.Role != provider.RoleUser {
			continue
		}
		if strings.HasPrefix(msg.Text, "[Proposal "+items[0].ID) && strings.Contains(msg.Text, "approved and applied") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the next turn's history carries no outcome notice: %+v", lastReq.Messages)
	}
}

func TestChatProposalDecisionReject(t *testing.T) {
	f := newProposalsFixture(t, proposalCallStep(createDealCall(`{"data":{}}`)), textStep("waiting"))
	conv, p := f.proposeTurn(t, createDealCall(`{"data":{}}`))
	h := f.proposalsHandler(t, nil)

	code, out := f.decide(t, h, p.ID, "reject", p.Revision, "")
	if code != http.StatusOK || out.Proposal.State != chat.ProposalStateRejected {
		t.Fatalf("reject: %d %+v", code, out)
	}
	if f.host.writeCount() != 0 {
		t.Fatalf("a rejected proposal executed %d writes", f.host.writeCount())
	}
	msgs := f.messages(t, conv.ID)
	part := msgs[len(msgs)-1].Parts[0]
	if part.State != string(chat.ProposalStateRejected) || !strings.Contains(part.Text, "rejected by the user") {
		t.Fatalf("reject notice = %+v", part)
	}

	// Approving a rejected proposal is a conflict and writes nothing.
	code, conflict := f.decide(t, h, p.ID, "approve", p.Revision, "")
	if code != http.StatusConflict || conflict.Error.Code != chat.CodeConflict || conflict.Proposal.State != chat.ProposalStateRejected {
		t.Fatalf("late approve: %d %+v", code, conflict)
	}
	if f.host.writeCount() != 0 {
		t.Fatalf("a conflicted decision wrote %d records", f.host.writeCount())
	}
}

func TestChatProposalDecisionStale(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *proposalsFixture)
		status int
		code   string
	}{
		// Credentials revoked at the host mean the approving request cannot
		// even authenticate, so nothing reaches the proposal.
		{"credentials revoked", func(f *proposalsFixture) { f.host.setRevoked("good") },
			http.StatusUnauthorized, chat.CodeUnauthorized},
		// Permission withdrawn while the proposal waited is stale, and the
		// proposal stays pending so the user can reject it.
		{"collection hidden", func(f *proposalsFixture) { f.host.hide("deals") },
			http.StatusConflict, chat.CodeStale},
		{"manifest changed", func(f *proposalsFixture) {
			f.srv.cfg.Manifest.Collections = append(f.srv.cfg.Manifest.Collections, ManifestCollection{
				Collection: "other", Operations: []string{"fetch"}, Policy: "authenticated",
			})
		}, http.StatusConflict, chat.CodeStale},
		{"tool schema changed", func(f *proposalsFixture) {
			deals := f.host.cols[0]
			deals.CreateFields = []string{"name"}
			deals.FieldMap = map[string]FieldInfo{"name": {Name: "name", Type: "text"}}
		}, http.StatusConflict, chat.CodeStale},
		{"writes revoked", func(f *proposalsFixture) { f.srv.cfg.AllowWrites = false },
			http.StatusConflict, chat.CodeStale},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			call := createDealCall(`{"data":{}}`)
			f := newProposalsFixture(t, proposalCallStep(call), textStep("waiting"))
			_, p := f.proposeTurn(t, call)
			h := f.proposalsHandler(t, nil)

			c.mutate(f)
			code, out := f.decide(t, h, p.ID, "approve", p.Revision, "")
			if code != c.status {
				t.Fatalf("approve: %d %+v, want %d", code, out, c.status)
			}
			if out.Error.Code != c.code {
				t.Fatalf("code = %q, want %q", out.Error.Code, c.code)
			}
			if f.host.writeCount() != 0 {
				t.Fatalf("a stale approval wrote %d records", f.host.writeCount())
			}
			if c.status != http.StatusConflict {
				return
			}
			if out.Proposal.State != chat.ProposalStatePending {
				t.Fatalf("state = %s, want it left pending", out.Proposal.State)
			}
			// The user can still reject what can no longer run.
			if code, out := f.decide(t, h, p.ID, "reject", p.Revision, ""); code != http.StatusOK || out.Proposal.State != chat.ProposalStateRejected {
				t.Fatalf("reject after stale: %d %+v", code, out)
			}
		})
	}
}

func TestChatProposalArgumentEdits(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "update_deals", Arguments: json.RawMessage(`{"id":"r1","data":{"name":"Old"}}`)}

	t.Run("disabled", func(t *testing.T) {
		f := newProposalsFixture(t, proposalCallStep(call), textStep("waiting"))
		_, p := f.proposeTurn(t, call)
		h := f.proposalsHandler(t, nil)

		code, _ := f.decide(t, h, p.ID, "approve", p.Revision, `,"arguments":{"id":"r1","data":{"name":"New"}}`)
		if code != http.StatusBadRequest {
			t.Fatalf("edit with edits disabled: %d", code)
		}
		if f.host.writeCount() != 0 {
			t.Fatalf("a refused edit wrote %d records", f.host.writeCount())
		}
	})

	t.Run("enabled", func(t *testing.T) {
		f := newProposalsFixture(t, proposalCallStep(call), textStep("waiting"))
		_, p := f.proposeTurn(t, call)
		h := f.proposalsHandler(t, func(o *ChatOptions) { o.AllowArgumentEdits = true })
		if f.host.writeCount() != 0 {
			t.Fatalf("the turn wrote %d records", f.host.writeCount())
		}

		code, out := f.decide(t, h, p.ID, "approve", p.Revision, `,"arguments":{"id":"r1","data":{"name":"New"}}`)
		if code != http.StatusOK || out.Proposal.State != chat.ProposalStateSucceeded {
			t.Fatalf("approve: %d %+v", code, out)
		}
		if out.Proposal.Revision != p.Revision+3 {
			t.Fatalf("revision = %d, want %d", out.Proposal.Revision, p.Revision+3)
		}
		if !strings.Contains(string(out.Proposal.Arguments), `"New"`) {
			t.Fatalf("stored arguments = %s", out.Proposal.Arguments)
		}
		if f.host.writeCount() != 1 {
			t.Fatalf("writes = %d, want 1", f.host.writeCount())
		}
	})

	t.Run("retargeting is refused", func(t *testing.T) {
		f := newProposalsFixture(t, proposalCallStep(call), textStep("waiting"))
		_, p := f.proposeTurn(t, call)
		h := f.proposalsHandler(t, func(o *ChatOptions) { o.AllowArgumentEdits = true })

		code, _ := f.decide(t, h, p.ID, "approve", p.Revision, `,"arguments":{"id":"r2","data":{"name":"New"}}`)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("retargeted edit: %d", code)
		}
		if f.host.writeCount() != 0 {
			t.Fatalf("a refused edit wrote %d records", f.host.writeCount())
		}
	})
}

func TestChatProposalCustomWriteApprove(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "strict_publish", Arguments: json.RawMessage(`{"name":"Hello"}`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("waiting"))
	_, p := f.proposeTurn(t, call)
	h := f.proposalsHandler(t, nil)

	code, out := f.decide(t, h, p.ID, "approve", p.Revision, "")
	if code != http.StatusOK || out.Proposal.State != chat.ProposalStateSucceeded {
		t.Fatalf("approve: %d %+v", code, out)
	}
	if f.strict.calls() != 1 {
		t.Fatalf("custom handler ran %d times, want 1", f.strict.calls())
	}
	if string(out.Proposal.Result) != `{"ok":true}` {
		t.Fatalf("result = %s", out.Proposal.Result)
	}

	// The audit stream records the approved call as a chat call labelled with
	// the proposal it applied.
	var sawApproved bool
	for _, e := range f.audit.snapshot() {
		if e.Outcome == "ok" && e.Channel == "chat" && e.Tool == "strict_publish" && strings.HasPrefix(e.RequestID, "proposal-") {
			sawApproved = true
		}
	}
	if !sawApproved {
		t.Fatalf("no approved-call audit event in %+v", f.audit.snapshot())
	}
}

func TestChatProposalCustomFailureOutcomes(t *testing.T) {
	setHandler := func(t *testing.T, f *proposalsFixture, timeout time.Duration, handler func(context.Context, CallContext, map[string]any) (any, error)) {
		t.Helper()
		for i := range f.srv.cfg.CustomTools {
			if f.srv.cfg.CustomTools[i].Name != "strict_publish" {
				continue
			}
			f.srv.cfg.CustomTools[i].Handler = handler
			if timeout > 0 {
				f.srv.cfg.CustomTools[i].Timeout = timeout
			}
		}
	}

	t.Run("handler error is a definite failure", func(t *testing.T) {
		call := provider.ToolCall{ID: "call-1", Name: "strict_publish", Arguments: json.RawMessage(`{"name":"Hello"}`)}
		f := newProposalsFixture(t, proposalCallStep(call), textStep("waiting"))
		_, p := f.proposeTurn(t, call)
		setHandler(t, f, 0, func(context.Context, CallContext, map[string]any) (any, error) {
			return nil, errors.New("handler detail that must not leak")
		})
		h := f.proposalsHandler(t, nil)

		code, out := f.decide(t, h, p.ID, "approve", p.Revision, "")
		if code != http.StatusOK || out.Proposal.State != chat.ProposalStateFailed {
			t.Fatalf("approve: %d %+v", code, out)
		}
		if strings.Contains(out.Proposal.Failure, "handler detail") {
			t.Fatalf("failure leaked handler text: %q", out.Proposal.Failure)
		}
		if out.Proposal.Failure != "Tool execution failed." {
			t.Fatalf("failure = %q", out.Proposal.Failure)
		}

		// A definite failure is never rerun.
		code, replay := f.decide(t, h, p.ID, "approve", p.Revision, "")
		if code != http.StatusOK || !replay.Replayed || replay.Proposal.State != chat.ProposalStateFailed {
			t.Fatalf("replay: %d %+v", code, replay)
		}
	})

	t.Run("handler timeout is an unknown outcome", func(t *testing.T) {
		call := provider.ToolCall{ID: "call-1", Name: "strict_publish", Arguments: json.RawMessage(`{"name":"Hello"}`)}
		f := newProposalsFixture(t, proposalCallStep(call), textStep("waiting"))
		_, p := f.proposeTurn(t, call)
		setHandler(t, f, 20*time.Millisecond, func(ctx context.Context, _ CallContext, _ map[string]any) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		h := f.proposalsHandler(t, nil)

		code, out := f.decide(t, h, p.ID, "approve", p.Revision, "")
		if code != http.StatusOK || out.Proposal.State != chat.ProposalStateUnknown {
			t.Fatalf("approve: %d %+v", code, out)
		}
		if out.Proposal.Failure != chat.UnknownOutcomeMessage {
			t.Fatalf("failure = %q, want the unknown-outcome text", out.Proposal.Failure)
		}
	})
}

func TestChatProposalHandlerStoreMustImplementProposals(t *testing.T) {
	f := newProposalsFixture(t)
	// A store that only implements chat.Store cannot answer the decision
	// endpoints, so the handler refuses it at startup.
	readOnly := struct{ chat.Store }{Store: f.store}
	_, err := f.srv.ChatHandler(ChatOptions{Namespace: "tests", Runner: f.runner, Store: readOnly, Proposals: true})
	if err == nil || !strings.Contains(err.Error(), "ProposalStore") {
		t.Fatalf("ChatHandler accepted a read-only store: %v", err)
	}
}

func TestChatProposalRoutesAbsentWithoutProposals(t *testing.T) {
	f := newProposalsFixture(t)
	h, err := f.srv.ChatHandler(ChatOptions{Namespace: "tests", Runner: f.runner, Store: f.store})
	if err != nil {
		t.Fatalf("ChatHandler: %v", err)
	}
	code, body := chatHTTP(t, h, http.MethodGet, DefaultChatPrefix+"/status", "good", "")
	if code != http.StatusOK || !strings.Contains(string(body), `"proposals":false`) {
		t.Fatalf("status: %d %s", code, body)
	}
	for _, target := range []string{
		DefaultChatPrefix + "/proposals/000000000000000",
		DefaultChatPrefix + "/proposals/000000000000000/decision",
	} {
		method := http.MethodGet
		if strings.HasSuffix(target, "/decision") {
			method = http.MethodPost
		}
		if code, body := chatHTTP(t, h, method, target, "good", `{"decision":"approve","revision":1}`); code != http.StatusNotFound {
			t.Fatalf("%s: %d %s", target, code, body)
		}
	}
	// The MCP surface is unchanged by the chat proposal code.
	p := chatPrincipal(f.host, "good")
	if normalizedToolJSON(t, f.srv, p, "mcp") != normalizedToolList(t, f.srv.listTools(p, "mcp")) {
		t.Fatal("the MCP tools list changed")
	}
}

// --- editor tools ---

// editorToolSpec is a valid editor ToolSpec: chat-only, handler-free, requiring
// the document id and the editor revision the patch was computed against.
func editorToolSpec() ToolSpec {
	return ToolSpec{
		Name:              "edit_section",
		Title:             "Edit section",
		Description:       "Draft an edit in the user's open editor.",
		Channels:          "chat",
		ExecutionLocation: "editor",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"editorId":     map[string]any{"type": "string", "description": "Editor/document to patch."},
				"baseRevision": map[string]any{"type": "string", "description": "Editor revision the patch was computed against."},
				"text":         map[string]any{"type": "string"},
			},
			"required":             []string{"editorId", "baseRevision", "text"},
			"additionalProperties": false,
		},
	}
}

func TestValidateCustomToolsEditor(t *testing.T) {
	base := func(tool ToolSpec) error {
		return Config{CustomTools: []ToolSpec{tool}}.ValidateCustomTools()
	}
	valid := editorToolSpec()
	if err := base(valid); err != nil {
		t.Fatalf("valid editor tool rejected: %v", err)
	}
	withWrite := editorToolSpec()
	withWrite.Effects = Effects{Kind: "write"}
	if err := base(withWrite); err != nil {
		t.Fatalf("editor tool with an explicit write effect rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ToolSpec)
		want   string
	}{
		{"mcp channel", func(t *ToolSpec) { t.Channels = "mcp" }, "editor"},
		{"both channels", func(t *ToolSpec) { t.Channels = "both" }, "editor"},
		{"default channel", func(t *ToolSpec) { t.Channels = "" }, "editor"},
		{"handler", func(t *ToolSpec) { t.Handler = okHandler }, "editor"},
		{"auto approval", func(t *ToolSpec) { t.Approval = "auto" }, "editor"},
		{"read effect", func(t *ToolSpec) { t.Effects = Effects{Kind: "read"} }, "editor"},
		{"no schema", func(t *ToolSpec) { t.InputSchema = nil }, "editor"},
		{"not an object", func(t *ToolSpec) { t.InputSchema["type"] = "array" }, "editor"},
		{"missing editorId", func(t *ToolSpec) {
			delete(t.InputSchema["properties"].(map[string]any), "editorId")
		}, "editorId"},
		{"missing baseRevision", func(t *ToolSpec) {
			delete(t.InputSchema["properties"].(map[string]any), "baseRevision")
		}, "baseRevision"},
		{"editorId not required", func(t *ToolSpec) {
			t.InputSchema["required"] = []string{"baseRevision", "text"}
		}, "editorId"},
		{"baseRevision not required", func(t *ToolSpec) {
			t.InputSchema["required"] = []string{"editorId", "text"}
		}, "baseRevision"},
		{"editorId not a string", func(t *ToolSpec) {
			t.InputSchema["properties"].(map[string]any)["editorId"] = map[string]any{"type": "integer"}
		}, "editorId"},
		{"unknown location", func(t *ToolSpec) { t.ExecutionLocation = "browser" }, "invalid execution location"},
	}
	for _, c := range cases {
		tool := editorToolSpec()
		c.mutate(&tool)
		err := base(tool)
		if c.want == "editorId" && err != nil && strings.Contains(err.Error(), "editorId") {
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", c.name, err, c.want)
		}
	}
}

// chatWritesExecution builds the caller's tool executor with chat writes on, as
// ChatHandler does for ChatOptions{Proposals: true}.
func chatWritesExecution(t *testing.T, srv *Server, p Principal) chat.ToolExecutor {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/chat/v1/conversations", nil)
	return srv.chatCallerWithWrites(p, req, "tests", "", true, false).Tools
}

func TestChatEditorToolSurface(t *testing.T) {
	f := newProposalsFixture(t)
	f.srv.cfg.CustomTools = append(f.srv.cfg.CustomTools, editorToolSpec())
	// An editor tool writes nothing on the server, so AllowWrites does not gate
	// it; proposals still do.
	f.srv.cfg.AllowWrites = false
	p := chatPrincipal(f.host, "good")

	readOnly := listNames(f.srv.listChatTools(p, false))
	if readOnly["edit_section"] {
		t.Fatal("the read-only chat surface offers an editor tool")
	}
	withWrites := listNames(f.srv.listChatTools(p, true))
	if !withWrites["edit_section"] {
		t.Fatalf("the chat-with-proposals surface is missing the editor tool: %v", withWrites)
	}

	// The read-only executor (proposals off) cannot see or call it; the writes
	// executor offers it to the model.
	if names := providerToolNames(t, mustChatDefs(t, chatExecution(t, f.srv, p))); names["edit_section"] {
		t.Fatal("a proposals-off chat executor offers an editor tool")
	}
	if names := providerToolNames(t, mustChatDefs(t, chatWritesExecution(t, f.srv, p))); !names["edit_section"] {
		t.Fatalf("a proposals-on chat executor does not offer the editor tool: %v", names)
	}

	// MCP never lists or runs it, even with the internal writes variant.
	if strings.Contains(normalizedToolJSON(t, f.srv, p, "mcp"), "edit_section") {
		t.Fatal("MCP tools/list offers an editor tool")
	}
	if normalizedToolJSON(t, f.srv, p, "mcp") != normalizedToolList(t, f.srv.listToolsWithWrites(p, "mcp", true)) {
		t.Fatal("the chat writes flag changed the MCP tools list")
	}
	_, out := rpc(t, f.srv, "good", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edit_section","arguments":{"editorId":"draft-1","baseRevision":"r7","text":"hi"}}}`)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("MCP ran an editor tool: %v", out)
	}

	// The planner drafts an editor proposal and a direct call always refuses.
	planner := chatWritesExecution(t, f.srv, p).(chat.ProposalPlanner)
	draft, err := planner.Plan(context.Background(), "edit_section", json.RawMessage(`{"editorId":"draft-1","baseRevision":"r7","text":"hi"}`))
	if err != nil || draft == nil {
		t.Fatalf("Plan(edit_section) = %v, %v", draft, err)
	}
	if draft.Location != chat.ProposalLocationEditor || draft.Target != "draft-1" {
		t.Fatalf("draft = %+v", draft)
	}
	wantSummary := "Edit section: edit draft-1 (applies in the editor after approval)"
	if draft.Summary != wantSummary {
		t.Fatalf("summary = %q, want %q", draft.Summary, wantSummary)
	}
	if fp, ok := f.srv.ChatToolFingerprint("edit_section"); !ok || draft.ToolFingerprint != fp {
		t.Fatalf("fingerprint = %q (%v), want %q", draft.ToolFingerprint, ok, fp)
	}

	var te chat.ToolError
	_, err = chatWritesExecution(t, f.srv, p).Call(context.Background(), "edit_section", json.RawMessage(`{"editorId":"draft-1","baseRevision":"r7","text":"hi"}`))
	if !errors.As(err, &te) || te.Message != "This change needs review." {
		t.Fatalf("a direct editor call = %v", err)
	}
	// Without proposals the tool is not on the caller's surface at all, so it
	// is refused exactly like a hidden tool.
	_, err = chatExecution(t, f.srv, p).Call(context.Background(), "edit_section", json.RawMessage(`{"editorId":"draft-1","baseRevision":"r7","text":"hi"}`))
	if !errors.As(err, &te) || te.Message != "Tool is unavailable." {
		t.Fatalf("a read-only editor call = %v", err)
	}

	// A missing editorId is refused before a proposal could bind it.
	if _, err := planner.Plan(context.Background(), "edit_section", json.RawMessage(`{"editorId":" ","baseRevision":"r7","text":"hi"}`)); err == nil {
		t.Fatal("Plan accepted an empty editorId")
	}

	if f.host.writeCount() != 0 {
		t.Fatalf("planning an editor change wrote %d records", f.host.writeCount())
	}
}

// addEditorToneProperty changes the editor tool's input schema so its
// fingerprint moves, without making the stored arguments invalid.
func addEditorToneProperty(t *testing.T, srv *Server) {
	t.Helper()
	for i := range srv.cfg.CustomTools {
		if srv.cfg.CustomTools[i].Name != "edit_section" {
			continue
		}
		props, _ := srv.cfg.CustomTools[i].InputSchema["properties"].(map[string]any)
		props["tone"] = map[string]any{"type": "string"}
		return
	}
	t.Fatal("editor tool not registered")
}

func editorCall(args string) provider.ToolCall {
	return provider.ToolCall{ID: "call-1", Name: "edit_section", Arguments: json.RawMessage(args)}
}

func TestChatEditorProposalTurnAndOutcome(t *testing.T) {
	call := editorCall(`{"editorId":"draft-1","baseRevision":"r7","text":"Hello"}`)
	f := newProposalsFixture(t, proposalCallStep(call), textStep("I drafted the change."))
	f.srv.cfg.CustomTools = append(f.srv.cfg.CustomTools, editorToolSpec())
	conv := f.conversation(t)
	f.runTurn(t, conv, f.exec(t, false))

	items := f.proposals(t, conv)
	if len(items) != 1 {
		t.Fatalf("proposals = %+v", items)
	}
	p := items[0]
	if p.Location != chat.ProposalLocationEditor || p.State != chat.ProposalStatePending {
		t.Fatalf("proposal = %+v", p)
	}
	if p.Target != "draft-1" {
		t.Fatalf("target = %q, want the editor id", p.Target)
	}
	if p.Summary != "Edit section: edit draft-1 (applies in the editor after approval)" {
		t.Fatalf("summary = %q", p.Summary)
	}
	if p.ToolFingerprint == "" || p.ManifestRevision != f.srv.manifest().Revision() {
		t.Fatalf("proposal identity = %+v", p)
	}
	if f.host.writeCount() != 0 {
		t.Fatalf("the editor turn executed %d writes", f.host.writeCount())
	}

	h := f.proposalsHandler(t, nil)
	code, out := f.decide(t, h, p.ID, "approve", p.Revision, "")
	if code != http.StatusOK || out.Replayed {
		t.Fatalf("approve: %d %+v", code, out)
	}
	if out.Proposal.State != chat.ProposalStateApproved {
		t.Fatalf("state = %s (%q), want approved", out.Proposal.State, out.Proposal.Failure)
	}
	// Approval authorizes the browser only: no server handler and no record
	// write exists, so neither can have run.
	if f.host.writeCount() != 0 {
		t.Fatalf("an approved editor proposal wrote %d records", f.host.writeCount())
	}

	body := `{"revision":` + strconv.Itoa(out.Proposal.Revision) + `,"status":"applied","editorRevision":"r8",` +
		`"hunks":[{"id":"h1","status":"applied"},{"id":"h2","status":"stale"}]}`
	code, raw := chatHTTP(t, h, http.MethodPost, DefaultChatPrefix+"/proposals/"+p.ID+"/outcome", "good", body)
	if code != http.StatusOK {
		t.Fatalf("outcome: %d %s", code, raw)
	}
	var outcome decision
	if err := json.Unmarshal(raw, &outcome); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if outcome.Proposal.State != chat.ProposalStateApplied || outcome.Proposal.EditorRevision != "r8" || len(outcome.Proposal.Hunks) != 2 {
		t.Fatalf("outcome proposal = %+v", outcome.Proposal)
	}

	msgs := f.messages(t, conv.ID)
	last := msgs[len(msgs)-1]
	part := last.Parts[0]
	if last.Role != chat.RoleNotice || part.Type != chat.PartProposalOutcome || part.ProposalID != p.ID || part.State != string(chat.ProposalStateApplied) {
		t.Fatalf("outcome notice = %+v", last)
	}
	if !strings.Contains(part.Text, "partially applied in the editor (1 of 2 parts) at revision r8 (not saved yet)") {
		t.Fatalf("notice text = %q", part.Text)
	}
	if strings.Contains(part.Text, "published") || strings.Contains(part.Text, "sent") {
		t.Fatalf("the notice claimed a completed write: %q", part.Text)
	}
}

func TestChatEditorProposalStalesWhenDefinitionChanges(t *testing.T) {
	call := editorCall(`{"editorId":"draft-1","baseRevision":"r7","text":"Hello"}`)
	f := newProposalsFixture(t, proposalCallStep(call), textStep("drafted"))
	f.srv.cfg.CustomTools = append(f.srv.cfg.CustomTools, editorToolSpec())
	conv := f.conversation(t)
	f.runTurn(t, conv, f.exec(t, false))
	items := f.proposals(t, conv)
	if len(items) != 1 {
		t.Fatalf("proposals = %+v", items)
	}
	p := items[0]

	addEditorToneProperty(t, f.srv)
	h := f.proposalsHandler(t, nil)
	code, out := f.decide(t, h, p.ID, "approve", p.Revision, "")
	if code != http.StatusConflict || out.Error.Code != chat.CodeStale {
		t.Fatalf("approve after a tool change: %d %+v", code, out)
	}
	if out.Proposal.State != chat.ProposalStatePending {
		t.Fatalf("state = %s, want pending", out.Proposal.State)
	}
	if f.host.writeCount() != 0 {
		t.Fatalf("a stale editor approval wrote %d records", f.host.writeCount())
	}
}

func TestChatEditorProposalsWithoutAllowWrites(t *testing.T) {
	f := newProposalsFixture(t)
	// A read-only manifest plus an editor tool: the host can offer no server
	// write, so proposals (review of an editor patch) do not need AllowWrites.
	m := &Manifest{SchemaVersion: "1", ToolMode: "per_collection", Collections: []ManifestCollection{
		{Collection: "deals", Operations: []string{"describe", "search", "fetch"}, Policy: "authenticated"},
	}}
	srv := New(Config{Manifest: m, AllowWrites: false, Auth: AuthConfig{Collections: []string{"users"}}, CustomTools: []ToolSpec{editorToolSpec()}}, f.host)

	h, err := srv.ChatHandler(ChatOptions{Namespace: "tests", Runner: f.runner, Store: f.store, Proposals: true})
	if err != nil {
		t.Fatalf("ChatHandler rejected an editor-only proposal host: %v", err)
	}
	p := chatPrincipal(f.host, "good")
	if names := listNames(srv.listChatTools(p, true)); !names["edit_section"] {
		t.Fatalf("editor-only host does not list its editor tool: %v", names)
	}
	if names := listNames(srv.listChatTools(p, true)); names["create_deals"] {
		t.Fatalf("an editor-only host offered a server write: %v", names)
	}

	code, body := chatHTTP(t, h, http.MethodGet, DefaultChatPrefix+"/status", "good", "")
	if code != http.StatusOK || !strings.Contains(string(body), `"proposals":true`) {
		t.Fatalf("status: %d %s", code, body)
	}

	// A host that could offer a server write still has to enable writes.
	withWrites := New(Config{Manifest: m, AllowWrites: false, Auth: AuthConfig{Collections: []string{"users"}}, CustomTools: []ToolSpec{
		editorToolSpec(),
		{Name: "publish", Channels: "chat", Write: true, InputSchema: map[string]any{"type": "object"}, Handler: okHandler},
	}}, f.host)
	if _, err := withWrites.ChatHandler(ChatOptions{Namespace: "tests", Runner: f.runner, Store: f.store, Proposals: true}); err == nil || !strings.Contains(err.Error(), "AllowWrites") {
		t.Fatalf("a server write tool without AllowWrites was accepted: %v", err)
	}
}

// Review fixes: generated write calls are shape-checked before they become a
// proposal, and the stored arguments are always the canonical object.
func TestChatProposalGeneratedArgumentShape(t *testing.T) {
	cases := []struct {
		name, tool, args, want string
	}{
		{"update without id", "update_deals", `{"data":{"name":"x"}}`, "Invalid params: /id must be a non-empty string"},
		{"delete with blank id", "delete_deals", `{"id":"  "}`, "Invalid params: /id must be a non-empty string"},
		{"create without data", "create_deals", `{}`, "Invalid params: /data must be an object"},
		{"create with unknown key", "create_deals", `{"data":{},"owner":"x"}`, "Invalid params: /owner is not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := provider.ToolCall{ID: "call-1", Name: tc.tool, Arguments: json.RawMessage(tc.args)}
			f := newProposalsFixture(t, proposalCallStep(call), textStep("ok"))
			conv := f.conversation(t)
			events := f.runTurn(t, conv, f.exec(t, false))
			if items := f.proposals(t, conv); len(items) != 0 {
				t.Fatalf("malformed call created a proposal: %+v", items)
			}
			failed := eventData[chat.ToolFailed](t, events, chat.EventToolFailed)
			if len(failed) != 1 || failed[0].Message != tc.want {
				t.Fatalf("tool.failed = %+v, want %q", failed, tc.want)
			}
		})
	}
}

func TestChatProposalNullArgumentsAreCanonical(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "titled_publish", Arguments: json.RawMessage(`null`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("ok"))
	conv := f.conversation(t)
	f.runTurn(t, conv, f.exec(t, false))
	items := f.proposals(t, conv)
	if len(items) != 1 || string(items[0].Arguments) != `{}` {
		t.Fatalf("proposals = %+v, want one with {} arguments", items)
	}
}

// Review fix: the chat access policy is re-evaluated before every tool, not
// only when the HTTP request arrives.
func TestChatExecutorRechecksChatPolicy(t *testing.T) {
	f := newProposalsFixture(t)
	f.host.mu.Lock()
	p := f.host.principals["good"]
	p.RecordValues = map[string][]string{"role": {"editor"}}
	f.host.principals["good"] = p
	f.host.mu.Unlock()

	exec := f.exec(t, false).(*chatToolExecutor)
	exec.policy = "field:role:editor"
	if _, err := exec.Tools(context.Background()); err != nil {
		t.Fatalf("Tools with the role: %v", err)
	}

	f.host.mu.Lock()
	p.RecordValues = map[string][]string{"role": {"viewer"}}
	f.host.principals["good"] = p
	f.host.mu.Unlock()
	if _, err := exec.Tools(context.Background()); err == nil {
		t.Fatal("Tools still listed after the role was removed")
	}
}

// Review fix: the handler and the runner must share one store.
func TestChatHandlerRefusesRunnerOnAnotherStore(t *testing.T) {
	f := newProposalsFixture(t)
	other := chat.NewMemStore(chat.MemStoreOptions{})
	_, err := f.srv.ChatHandler(ChatOptions{Namespace: "tests", Runner: f.runner, Store: other})
	if err == nil || !strings.Contains(err.Error(), "store the runner uses") {
		t.Fatalf("ChatHandler accepted a different store: %v", err)
	}
}

// Review fix: one chat entry point builds the tool listing once, however many
// gates consult it. Counted through the host's VisibleCollections, which every
// listing calls. Executing an approved proposal consults the listing twice (the
// proposal recheck and the call gate).
func TestChatExecuteBuildsListingOnce(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "create_deals", Arguments: json.RawMessage(`{"data":{"name":"Acme"}}`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("queued"))
	conv := f.conversation(t)
	exec := f.exec(t, false)
	f.runTurn(t, conv, exec)
	items := f.proposals(t, conv)
	if len(items) != 1 {
		t.Fatalf("proposals = %+v", items)
	}
	f.host.mu.Lock()
	f.host.visible = 0
	f.host.mu.Unlock()
	if _, err := exec.(chat.ProposalExecutor).Execute(context.Background(), items[0]); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	f.host.mu.Lock()
	n := f.host.visible
	f.host.mu.Unlock()
	// One listing plus the write's own collection lookup. Without the cache the
	// second gate rebuilds the listing and this is 3.
	if n > 2 {
		t.Fatalf("one Execute read the visible collections %d times, want at most 2", n)
	}
}
