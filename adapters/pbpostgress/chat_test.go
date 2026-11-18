package pbpostgress

// End-to-end chat tests through the real fork router (apis.NewRouter +
// RegisterRoutes + BuildMux). The model is the scripted provider, so nothing
// touches the network. They are the stock adapter's chat HTTP scenarios ported
// to a fork app on PostgreSQL: the only differences are the per-run collection
// name and the ownership partition.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
	"github.com/pocketbase/pocketbase/tools/types"

	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

const chatTestPath = "/api/mcp/chat/v1"

type forkChatEnv struct {
	t   *testing.T
	app core.App
	mux http.Handler
	srv *Server

	provider *scripted.Provider
	store    chat.Store

	prefix    string
	namespace string

	usersCollectionID string
	userID            string
	otherUserID       string
	superToken        string
	userToken         string
	otherToken        string
	closedDealID      string
	draftDealID       string
	dealCollection    string
	noteCollection    string
}

type forkChatEnvOptions struct {
	allowSuperusers bool
	// allowWrites enables server-side writes (and is required for proposals
	// over a manifest that declares a write operation).
	allowWrites bool
	// proposals turns the reviewed-write surface on.
	proposals bool
	// uploads turns the attachment surface on.
	uploads bool
	// manifest overrides the default read-only deals manifest. Nil means a
	// manifest for the fixture's deals collection.
	manifest func(env *forkChatEnv) *pbmcp.Manifest
	// noEnable leaves chat off, to prove the default is "not mounted, nothing
	// provisioned".
	noEnable bool
	// steps builds the scripted provider steps once the fixture ids exist.
	steps func(env *forkChatEnv) []scripted.Step
}

func forkFixedSteps(steps ...scripted.Step) func(*forkChatEnv) []scripted.Step {
	return func(*forkChatEnv) []scripted.Step { return steps }
}

func forkChatManifest(collection string) *pbmcp.Manifest {
	return &pbmcp.Manifest{
		SchemaVersion: "1", ToolMode: "per_collection", MaxGeneratedTools: 40,
		Collections: []pbmcp.ManifestCollection{{
			Collection: collection,
			// A stable alias keeps the generated tool names at
			// search_deals/fetch_deals even though the physical collection
			// name is unique per test run (the database is shared).
			Alias:      "deals",
			Operations: []string{"describe", "search", "fetch"},
			Presentation: pbmcp.Presentation{
				TitleFields:   []string{"name"},
				SummaryFields: []string{"name", "status"},
			},
			Fields: pbmcp.FieldAllowlists{Read: []string{"id", "name", "status", "created", "updated"}},
			Search: pbmcp.SearchSpec{
				TextFields:  []string{"name"},
				Filters:     map[string][]string{"status": {"eq", "in"}},
				SortFields:  []string{"name", "status", "created", "updated"},
				DefaultSort: []string{"-created"},
			},
		}},
	}
}

func newForkChatEnv(t *testing.T, opts forkChatEnvOptions) *forkChatEnv {
	t.Helper()
	app := newChatForkApp(t)
	salt := strings.ToLower(security.RandomString(6))

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}

	dealName := "deals_" + salt
	deals := core.NewBaseCollection(dealName)
	draftRule := `@request.auth.id != "" && status = "closed"`
	deals.ListRule = types.Pointer(draftRule)
	deals.ViewRule = types.Pointer(draftRule)
	deals.Fields.Add(
		&core.TextField{Name: "name"},
		&core.SelectField{Name: "status", Values: []string{"draft", "closed"}, MaxSelect: 1},
	)
	autodates(deals)
	if err := app.Save(deals); err != nil {
		t.Fatal(err)
	}

	// notes is the writable collection the proposal tests propose against. Its
	// name is unique per run because the database is shared.
	noteName := "notes_" + salt
	notes := core.NewBaseCollection(noteName)
	notesRule := `@request.auth.id != ""`
	notes.ListRule, notes.ViewRule = types.Pointer(notesRule), types.Pointer(notesRule)
	notes.CreateRule, notes.UpdateRule, notes.DeleteRule = types.Pointer(notesRule), types.Pointer(notesRule), types.Pointer(notesRule)
	notes.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "body"})
	autodates(notes)
	if err := app.Save(notes); err != nil {
		t.Fatal(err)
	}

	seed := func(name, status string) string {
		rec := core.NewRecord(deals)
		rec.Set("name", name)
		rec.Set("status", status)
		if err := app.Save(rec); err != nil {
			t.Fatal(err)
		}
		return rec.Id
	}
	env := &forkChatEnv{
		t:                 t,
		app:               app,
		usersCollectionID: users.Id,
		closedDealID:      seed("Closed Deal", "closed"),
		draftDealID:       seed("Draft Deal", "draft"),
		dealCollection:    dealName,
		noteCollection:    noteName,
		prefix:            chatTestPrefix(t),
		namespace:         "chat-tests-" + salt,
	}

	newUser := func(email string) (string, string) {
		u := core.NewRecord(users)
		u.SetEmail(email)
		u.SetPassword("password123")
		if err := app.Save(u); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { deleteWithoutHooks(app, u) })
		token, err := u.NewAuthToken()
		if err != nil {
			t.Fatal(err)
		}
		return u.Id, token
	}
	env.userID, env.userToken = newUser("chat-user-" + salt + "@example.com")
	env.otherUserID, env.otherToken = newUser("chat-other-" + salt + "@example.com")

	superusers, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	su := core.NewRecord(superusers)
	su.SetEmail("chat-root-" + salt + "@example.com")
	su.SetPassword("password123")
	if err := app.Save(su); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteWithoutHooks(app, su) })
	if env.superToken, err = su.NewAuthToken(); err != nil {
		t.Fatal(err)
	}

	cfg := pbmcp.Config{
		Path:        "/mcp",
		Auth:        pbmcp.AuthConfig{Collections: []string{"users"}, AllowSuperusers: opts.allowSuperusers},
		Manifest:    forkChatManifest(dealName),
		AllowWrites: opts.allowWrites,
	}
	if opts.manifest != nil {
		cfg.Manifest = opts.manifest(env)
	}
	srv, err := New(app, cfg)
	if err != nil {
		t.Fatal(err)
	}
	env.srv = srv

	if !opts.noEnable {
		var steps []scripted.Step
		if opts.steps != nil {
			steps = opts.steps(env)
		}
		env.provider = scripted.New(steps...)
		if err := srv.EnableChat(ChatConfig{
			Options:          pbmcp.ChatOptions{Namespace: env.namespace, Prefix: chatTestPath, Proposals: opts.proposals, Uploads: opts.uploads},
			Provider:         env.provider,
			Model:            "scripted-model",
			System:           "You are a test model.",
			CollectionPrefix: env.prefix,
		}); err != nil {
			t.Fatalf("EnableChat: %v", err)
		}
		store, ok := srv.ChatStorage()
		if !ok {
			t.Fatal("ChatStorage reports no store after EnableChat")
		}
		env.store = store
		t.Cleanup(func() {
			// Exercise the same OnTerminate hook production uses.
			_ = app.OnTerminate().Trigger(&core.TerminateEvent{App: app})
		})
	}

	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterRoutes(r); err != nil {
		t.Fatal(err)
	}
	if env.mux, err = r.BuildMux(); err != nil {
		t.Fatal(err)
	}
	return env
}

// do performs one request against the fork mux. token is a raw PB auth token.
func (env *forkChatEnv) do(token, method, path, body string) (int, []byte) {
	env.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (env *forkChatEnv) json(token, method, path, body string, out any) int {
	env.t.Helper()
	code, raw := env.do(token, method, path, body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			env.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return code
}

func (env *forkChatEnv) rpc(token, method string, params any) map[string]any {
	env.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	code, raw := env.do(token, http.MethodPost, "/mcp", string(body))
	if code != http.StatusOK {
		env.t.Fatalf("%s: HTTP %d %s", method, code, raw)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		env.t.Fatalf("%s: decode: %v", method, err)
	}
	return out
}

// mcpCall invokes one MCP tool and returns its structured content plus the
// isError flag and the text a client sees.
func (env *forkChatEnv) mcpCall(token, tool string, args map[string]any) (map[string]any, bool, string) {
	env.t.Helper()
	out := env.rpc(token, "tools/call", map[string]any{"name": tool, "arguments": args})
	result, ok := out["result"].(map[string]any)
	if !ok {
		env.t.Fatalf("%s: no result: %v", tool, out)
	}
	isErr, _ := result["isError"].(bool)
	text := ""
	if content, ok := result["content"].([]any); ok && len(content) > 0 {
		if part, ok := content[0].(map[string]any); ok {
			text, _ = part["text"].(string)
		}
	}
	sc, _ := result["structuredContent"].(map[string]any)
	return sc, isErr, text
}

// --- SSE reading ---

// forkReadSSE reads event frames from r until ctx expires. It reports each
// decoded chat.Event on the returned channel; the channel closes at EOF/error.
func forkReadSSE(ctx context.Context, r *bufio.Reader) <-chan chat.Event {
	ch := make(chan chat.Event, 16)
	go func() {
		defer close(ch)
		data := ""
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if data == "" {
					continue
				}
				var ev chat.Event
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					return
				}
				select {
				case ch <- ev:
				case <-ctx.Done():
					return
				}
				data = ""
			}
		}
	}()
	return ch
}

// streamTurn opens the authenticated SSE endpoint for one turn over a real HTTP
// server (so the response is genuinely chunked and flushed, not a recorder).
func (env *forkChatEnv) streamTurn(ctx context.Context, serverURL, token, turnID string) (*http.Response, <-chan chat.Event) {
	env.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+chatTestPath+"/turns/"+turnID+"/events", nil)
	if err != nil {
		env.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		env.t.Fatalf("open event stream: %v", err)
	}
	return resp, forkReadSSE(ctx, bufio.NewReader(resp.Body))
}

func forkWaitEvent(t *testing.T, ch <-chan chat.Event, match func(chat.Event) bool) chat.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("event stream closed before the expected event")
			}
			if match(ev) {
				return ev
			}
			if ev.Type.Terminal() {
				t.Fatalf("saw terminal %s before the expected event", ev.Type)
			}
		case <-deadline:
			t.Fatal("timed out waiting for an SSE event")
		}
	}
}

func forkRawID(id string) json.RawMessage {
	raw, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		panic(err)
	}
	return raw
}

// --- tests ---

func TestChatHTTPEndToEndToolResultMatchesMCP(t *testing.T) {
	args := json.RawMessage(`{"query":"Deal"}`)
	env := newForkChatEnv(t, forkChatEnvOptions{steps: forkFixedSteps(
		scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkToolCall, ToolCall: &provider.ToolCall{ID: "call-1", Name: "search_deals", Arguments: args}},
			{Kind: provider.ChunkDone},
		}},
		scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: "Found the closed deals."},
			{Kind: provider.ChunkDone},
		}},
	)})

	var conv chat.Conversation
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations", `{"title":"deals"}`, &conv); code != http.StatusCreated {
		t.Fatalf("create conversation: HTTP %d", code)
	}
	if conv.ID == "" || conv.Title != "deals" {
		t.Fatalf("conversation = %+v", conv)
	}

	var submitted struct {
		Turn    chat.Turn `json:"turn"`
		Created bool      `json:"created"`
	}
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations/"+conv.ID+"/turns", `{"text":"find deals"}`, &submitted); code != http.StatusAccepted {
		t.Fatalf("submit turn: HTTP %d", code)
	}
	if !submitted.Created || submitted.Turn.ID == "" {
		t.Fatalf("submission = %+v", submitted)
	}

	srv := httptest.NewServer(env.mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, events := env.streamTurn(ctx, srv.URL, env.userToken, submitted.Turn.ID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event stream: HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}

	var (
		toolResult json.RawMessage
		terminal   chat.Event
		order      []chat.EventType
	)
	for terminal.Type == "" {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("stream closed without a terminal event; saw %v", order)
			}
			order = append(order, ev.Type)
			if ev.Type == chat.EventToolCompleted {
				var data chat.ToolCompleted
				if err := json.Unmarshal(ev.Data, &data); err != nil {
					t.Fatalf("decode tool.completed: %v", err)
				}
				if data.Tool != "search_deals" {
					t.Fatalf("tool = %q", data.Tool)
				}
				toolResult = data.Result
			}
			if ev.Type.Terminal() {
				terminal = ev
			}
		case <-ctx.Done():
			t.Fatalf("timed out reading events; saw %v", order)
		}
	}
	if terminal.Type != chat.EventTurnCompleted {
		t.Fatalf("terminal = %+v (events %v)", terminal, order)
	}
	if toolResult == nil {
		t.Fatalf("no tool.completed event: %v", order)
	}

	// The same tool, same user, same arguments through MCP.
	sc, isErr, text := env.mcpCall(env.userToken, "search_deals", map[string]any{"query": "Deal"})
	if isErr {
		t.Fatalf("MCP search_deals failed: %s", text)
	}
	want, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if string(toolResult) != string(want) {
		t.Fatalf("chat tool result\n%s\n!= MCP structured content\n%s", toolResult, want)
	}
	// The PB list rule applied on both surfaces: only the closed deal is
	// visible, never the draft.
	if strings.Contains(string(toolResult), "Draft Deal") {
		t.Fatalf("chat tool result leaked a record denied by the list rule: %s", toolResult)
	}
	if !strings.Contains(string(toolResult), "Closed Deal") {
		t.Fatalf("chat tool result is missing the visible record: %s", toolResult)
	}

	// The persisted transcript matches the stream, including the tool call.
	var detail struct {
		Conversation chat.Conversation `json:"conversation"`
		Messages     []chat.Message    `json:"messages"`
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPath+"/conversations/"+conv.ID, "", &detail); code != http.StatusOK {
		t.Fatalf("get conversation: HTTP %d", code)
	}
	if len(detail.Messages) < 2 {
		t.Fatalf("transcript = %+v", detail.Messages)
	}
	if detail.Messages[0].Role != chat.RoleUser {
		t.Fatalf("first message role = %q", detail.Messages[0].Role)
	}
}

func TestChatHTTPOwnerIsolation(t *testing.T) {
	env := newForkChatEnv(t, forkChatEnvOptions{})
	var conv chat.Conversation
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations", `{"title":"private"}`, &conv); code != http.StatusCreated {
		t.Fatalf("create: HTTP %d", code)
	}

	// Another user cannot read, rename, delete, list or submit to it.
	if code, _ := env.do(env.otherToken, http.MethodGet, chatTestPath+"/conversations/"+conv.ID, ""); code != http.StatusNotFound {
		t.Fatalf("other user get = HTTP %d, want 404", code)
	}
	if code, _ := env.do(env.otherToken, http.MethodPatch, chatTestPath+"/conversations/"+conv.ID, `{"title":"hijack"}`); code != http.StatusNotFound {
		t.Fatalf("other user rename = HTTP %d, want 404", code)
	}
	if code, _ := env.do(env.otherToken, http.MethodPost, chatTestPath+"/conversations/"+conv.ID+"/turns", `{"text":"hi"}`); code != http.StatusNotFound {
		t.Fatalf("other user submit = HTTP %d, want 404", code)
	}
	if code, _ := env.do(env.otherToken, http.MethodDelete, chatTestPath+"/conversations/"+conv.ID, ""); code != http.StatusNotFound {
		t.Fatalf("other user delete = HTTP %d, want 404", code)
	}
	var list struct {
		Items []chat.Conversation `json:"items"`
	}
	if code := env.json(env.otherToken, http.MethodGet, chatTestPath+"/conversations", "", &list); code != http.StatusOK {
		t.Fatalf("other user list = HTTP %d", code)
	}
	if len(list.Items) != 0 {
		t.Fatalf("other user sees %d conversations", len(list.Items))
	}

	// The owner still sees it.
	if code, _ := env.do(env.userToken, http.MethodGet, chatTestPath+"/conversations/"+conv.ID, ""); code != http.StatusOK {
		t.Fatalf("owner get = HTTP %d", code)
	}
}

func TestChatHTTPSuperuserRejectedUnlessAllowed(t *testing.T) {
	closed := newForkChatEnv(t, forkChatEnvOptions{})
	if code, body := closed.do(closed.superToken, http.MethodGet, chatTestPath+"/status", ""); code != http.StatusUnauthorized {
		t.Fatalf("superuser without opt-in = HTTP %d %s", code, body)
	}

	open := newForkChatEnv(t, forkChatEnvOptions{allowSuperusers: true})
	if code, body := open.do(open.superToken, http.MethodGet, chatTestPath+"/status", ""); code != http.StatusOK {
		t.Fatalf("superuser with opt-in = HTTP %d %s", code, body)
	}
}

func TestChatCollectionsAreNotPubliclyReachable(t *testing.T) {
	env := newForkChatEnv(t, forkChatEnvOptions{})
	var conv chat.Conversation
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations", `{"title":"secret"}`, &conv); code != http.StatusCreated {
		t.Fatalf("create: HTTP %d", code)
	}
	store, ok := env.srv.ChatStorage()
	if !ok {
		t.Fatal("no store")
	}
	names := store.(*ChatStore).Collections()

	for _, name := range names {
		for _, path := range []string{
			"/api/collections/" + name + "/records",
			"/api/collections/" + name + "/records/" + conv.ID,
		} {
			code, body := env.do(env.userToken, http.MethodGet, path, "")
			if code != http.StatusNotFound && code != http.StatusForbidden {
				t.Fatalf("%s = HTTP %d %s, want a refusal", path, code, body)
			}
			if strings.Contains(string(body), conv.ID) || strings.Contains(string(body), "secret") {
				t.Fatalf("%s leaked chat data: %s", path, body)
			}
		}
	}

	// A superuser may inspect them through the API (they are ordinary base
	// collections), which proves the refusal above is the nil rule and not an
	// accident of routing.
	convCol := names[0]
	if code, body := env.do(env.superToken, http.MethodGet, "/api/collections/"+convCol+"/records", ""); code != http.StatusOK {
		t.Fatalf("superuser records = HTTP %d %s", code, body)
	} else if !strings.Contains(string(body), conv.ID) {
		t.Fatalf("superuser records did not include the stored conversation: %s", body)
	}

	// And they are absent from the MCP manifest and tool list.
	access := env.rpc(env.userToken, "tools/list", map[string]any{})
	tools, _ := access["result"].(map[string]any)
	list, _ := tools["tools"].([]any)
	for _, raw := range list {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		for _, collection := range names {
			if strings.Contains(strings.ToLower(name), strings.ToLower(strings.TrimPrefix(collection, "_"))) {
				t.Fatalf("chat collection %q leaked into the MCP tool list as %q", collection, name)
			}
		}
	}
}

func TestChatAndMCPAgreeOnRecordRules(t *testing.T) {
	// The model is told to fetch a record the caller's PB ViewRule denies, then
	// (in a second turn) one it allows. Chat and MCP must agree on both.
	env := newForkChatEnv(t, forkChatEnvOptions{steps: func(env *forkChatEnv) []scripted.Step {
		return []scripted.Step{
			{Chunks: []provider.Chunk{
				{Kind: provider.ChunkToolCall, ToolCall: &provider.ToolCall{ID: "call-draft", Name: "fetch_deals", Arguments: forkRawID(env.draftDealID)}},
				{Kind: provider.ChunkDone},
			}},
			{Chunks: []provider.Chunk{{Kind: provider.ChunkText, Text: "denied"}, {Kind: provider.ChunkDone}}},
			{Chunks: []provider.Chunk{
				{Kind: provider.ChunkToolCall, ToolCall: &provider.ToolCall{ID: "call-closed", Name: "fetch_deals", Arguments: forkRawID(env.closedDealID)}},
				{Kind: provider.ChunkDone},
			}},
			{Chunks: []provider.Chunk{{Kind: provider.ChunkText, Text: "ok"}, {Kind: provider.ChunkDone}}},
		}
	}})

	_, draftMCPError, draftMCPText := env.mcpCall(env.userToken, "fetch_deals", map[string]any{"id": env.draftDealID})
	if !draftMCPError {
		t.Fatal("MCP fetch of a ViewRule-denied draft succeeded")
	}
	closedMCP, closedMCPError, closedMCPText := env.mcpCall(env.userToken, "fetch_deals", map[string]any{"id": env.closedDealID})
	if closedMCPError {
		t.Fatalf("MCP fetch of a visible record failed: %s", closedMCPText)
	}

	runTurn := func(text string) (terminal chat.Event, toolEvent chat.Event) {
		var conv chat.Conversation
		if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations", `{}`, &conv); code != http.StatusCreated {
			t.Fatalf("create: HTTP %d", code)
		}
		var submitted struct {
			Turn chat.Turn `json:"turn"`
		}
		if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations/"+conv.ID+"/turns", fmt.Sprintf(`{"text":%q}`, text), &submitted); code != http.StatusAccepted {
			t.Fatalf("submit: HTTP %d", code)
		}
		srv := httptest.NewServer(env.mux)
		defer srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, events := env.streamTurn(ctx, srv.URL, env.userToken, submitted.Turn.ID)
		defer resp.Body.Close()
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatal("stream closed before a terminal event")
				}
				if ev.Type == chat.EventToolCompleted || ev.Type == chat.EventToolFailed {
					toolEvent = ev
				}
				if ev.Type.Terminal() {
					return ev, toolEvent
				}
			case <-ctx.Done():
				t.Fatal("timed out reading events")
			}
		}
	}

	draftTerminal, draftTool := runTurn("fetch the draft")
	if draftTerminal.Type != chat.EventTurnCompleted {
		t.Fatalf("draft turn terminal = %+v", draftTerminal)
	}
	if draftTool.Type != chat.EventToolFailed {
		t.Fatalf("draft fetch event = %+v, want tool.failed", draftTool)
	}
	var draftFailure chat.ToolFailed
	if err := json.Unmarshal(draftTool.Data, &draftFailure); err != nil {
		t.Fatal(err)
	}
	if draftFailure.Message != draftMCPText {
		t.Fatalf("chat denial %q != MCP denial %q", draftFailure.Message, draftMCPText)
	}

	closedTerminal, closedTool := runTurn("fetch the closed one")
	if closedTerminal.Type != chat.EventTurnCompleted {
		t.Fatalf("closed turn terminal = %+v", closedTerminal)
	}
	if closedTool.Type != chat.EventToolCompleted {
		t.Fatalf("closed fetch event = %+v, want tool.completed", closedTool)
	}
	var completed chat.ToolCompleted
	if err := json.Unmarshal(closedTool.Data, &completed); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(closedMCP)
	if err != nil {
		t.Fatal(err)
	}
	if string(completed.Result) != string(want) {
		t.Fatalf("chat fetch result %s != MCP %s", completed.Result, want)
	}
}

func TestChatRestartRecoveryInterruptsRunningTurn(t *testing.T) {
	env := newForkChatEnv(t, forkChatEnvOptions{})
	store := env.store

	owner := chat.OwnerKey{
		Namespace:        env.namespace,
		Mount:            "/mcp",
		AuthCollectionID: env.usersCollectionID,
		UserID:           env.userID,
	}
	conv, err := store.CreateConversation(context.Background(), chat.Conversation{Owner: owner, Title: "doomed"})
	if err != nil {
		t.Fatal(err)
	}
	turn, created, err := store.CreateTurn(context.Background(), owner, chat.Turn{ConversationID: conv.ID, IdempotencyKey: "restart"})
	if err != nil || !created {
		t.Fatalf("CreateTurn = %v, %v", created, err)
	}
	if _, err := store.TransitionTurn(context.Background(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(context.Background(), owner, chat.Event{TurnID: turn.ID, Type: chat.EventTurnStarted}); err != nil {
		t.Fatal(err)
	}

	// A second server on the same app and mount is the next startup: it scopes
	// recovery to the same namespace and mount, so it must find this turn.
	cfg := pbmcp.Config{
		Path:     "/mcp",
		Auth:     pbmcp.AuthConfig{Collections: []string{"users"}},
		Manifest: forkChatManifest(env.dealCollection),
	}
	restarted, err := New(env.app, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.EnableChat(ChatConfig{
		Options:          pbmcp.ChatOptions{Namespace: env.namespace, Prefix: "/api/mcp/chat/v2", Store: store},
		Provider:         scripted.New(),
		Model:            "scripted-model",
		CollectionPrefix: env.prefix,
	}); err != nil {
		t.Fatalf("EnableChat after restart: %v", err)
	}
	// Exercise the same OnTerminate hook production uses, so the restarted
	// server's proposal sweep stops with the app.
	t.Cleanup(func() { _ = env.app.OnTerminate().Trigger(&core.TerminateEvent{App: env.app}) })

	stored, err := store.GetTurn(context.Background(), owner, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != chat.TurnStateInterrupted {
		t.Fatalf("state after restart = %s, want interrupted", stored.State)
	}
	if stored.Error == nil || stored.Error.Code != chat.FailureInterrupted {
		t.Fatalf("failure after restart = %+v", stored.Error)
	}
	events, err := store.EventsAfter(context.Background(), owner, turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, ev := range events {
		if ev.Type != chat.EventTurnFailed {
			continue
		}
		terminals++
		var payload chat.TurnFailed
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("decode turn.failed: %v", err)
		}
		if payload.Code != "interrupted" {
			t.Fatalf("terminal code = %q", payload.Code)
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly 1: %+v", terminals, events)
	}
	// Recovery also freed the conversation.
	if _, _, err := store.CreateTurn(context.Background(), owner, chat.Turn{ConversationID: conv.ID, IdempotencyKey: "after"}); err != nil {
		t.Fatalf("conversation still blocked after recovery: %v", err)
	}
}

func TestChatSSEStreamsBeforeTurnFinishes(t *testing.T) {
	env := newForkChatEnv(t, forkChatEnvOptions{steps: forkFixedSteps(
		scripted.Step{
			Chunks: []provider.Chunk{
				{Kind: provider.ChunkText, Text: "partial "},
				{Kind: provider.ChunkText, Text: "answer"},
			},
			// Keep the model stream open until the turn is cancelled, so the
			// test observes events while the turn is still running.
			BlockUntilCancel: true,
		},
	)})

	var conv chat.Conversation
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations", `{}`, &conv); code != http.StatusCreated {
		t.Fatalf("create: HTTP %d", code)
	}
	var submitted struct {
		Turn chat.Turn `json:"turn"`
	}
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations/"+conv.ID+"/turns", `{"text":"stream please"}`, &submitted); code != http.StatusAccepted {
		t.Fatalf("submit: HTTP %d", code)
	}

	srv := httptest.NewServer(env.mux)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, events := env.streamTurn(ctx, srv.URL, env.userToken, submitted.Turn.ID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: HTTP %d", resp.StatusCode)
	}

	// The first frames must arrive while the turn is still running: the
	// provider is blocked until cancel.
	delta := forkWaitEvent(t, events, func(e chat.Event) bool { return e.Type == chat.EventTextDelta })
	if delta.Seq == 0 {
		t.Fatalf("delta has no sequence: %+v", delta)
	}
	var turn chat.Turn
	if code := env.json(env.userToken, http.MethodGet, chatTestPath+"/turns/"+submitted.Turn.ID, "", &turn); code != http.StatusOK {
		t.Fatalf("get turn: HTTP %d", code)
	}
	if turn.State != chat.TurnStateRunning {
		t.Fatalf("turn state when the first delta arrived = %s, want running", turn.State)
	}

	// Cancelling ends the stream with the one terminal event.
	if code, body := env.do(env.userToken, http.MethodPost, chatTestPath+"/turns/"+submitted.Turn.ID+"/cancel", ""); code != http.StatusOK {
		t.Fatalf("cancel: HTTP %d %s", code, body)
	}
	var terminal chat.Event
	for terminal.Type == "" {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream closed before turn.cancelled")
			}
			if ev.Type.Terminal() {
				terminal = ev
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for turn.cancelled")
		}
	}
	if terminal.Type != chat.EventTurnCancelled {
		t.Fatalf("terminal = %+v", terminal)
	}
	if stored, err := env.store.GetTurn(context.Background(), chat.OwnerKey{
		Namespace: env.namespace, Mount: "/mcp", AuthCollectionID: env.usersCollectionID, UserID: env.userID,
	}, submitted.Turn.ID); err != nil {
		t.Fatal(err)
	} else if stored.State != chat.TurnStateCancelled {
		t.Fatalf("stored state = %s", stored.State)
	}
}

func TestChatDisabledByDefault(t *testing.T) {
	env := newForkChatEnv(t, forkChatEnvOptions{noEnable: true})
	if code, _ := env.do(env.userToken, http.MethodGet, chatTestPath+"/status", ""); code != http.StatusNotFound {
		t.Fatalf("chat status without EnableChat = HTTP %d, want 404", code)
	}
	for _, name := range []string{env.prefix + "Conversations", env.prefix + "Messages", env.prefix + "Turns", env.prefix + "Events"} {
		if _, err := env.app.FindCollectionByNameOrId(name); err == nil {
			t.Fatalf("chat collection %q was provisioned although chat was never enabled", name)
		}
	}
	if _, ok := env.srv.ChatStorage(); ok {
		t.Fatal("ChatStorage reported a store although chat was never enabled")
	}
}

// TestChatEnableTwiceFails keeps the accidental double mount from depending on
// the router's panic behaviour.
func TestChatEnableTwiceFails(t *testing.T) {
	env := newForkChatEnv(t, forkChatEnvOptions{})
	if err := env.srv.EnableChat(ChatConfig{
		Options:  pbmcp.ChatOptions{Namespace: env.namespace},
		Provider: scripted.New(),
	}); err == nil {
		t.Fatal("second EnableChat succeeded")
	}
}

// forkWritableManifest is the fixture manifest plus a writable notes collection
// a chat proposal can create a record in.
func forkWritableManifest(dealCollection, noteCollection string) *pbmcp.Manifest {
	m := forkChatManifest(dealCollection)
	m.Collections = append(m.Collections, pbmcp.ManifestCollection{
		Collection:   noteCollection,
		Alias:        "notes",
		Operations:   []string{"describe", "search", "fetch", "create"},
		Presentation: pbmcp.Presentation{TitleFields: []string{"title"}, SummaryFields: []string{"title"}},
		Fields:       pbmcp.FieldAllowlists{Read: []string{"id", "title", "created", "updated"}, Create: []string{"title"}},
		Search: pbmcp.SearchSpec{
			TextFields:  []string{"title"},
			SortFields:  []string{"title", "created", "updated"},
			DefaultSort: []string{"-created"},
		},
	})
	return m
}

// proposalOwnerFor is the owner key a chat request of this env produces.
func (env *forkChatEnv) proposalOwner() chat.OwnerKey {
	return chat.OwnerKey{
		Namespace:        env.namespace,
		Mount:            "/mcp",
		AuthCollectionID: env.usersCollectionID,
		UserID:           env.userID,
	}
}

// runTurn submits one message and waits for its terminal event.
func (env *forkChatEnv) runTurn(text string) (chat.Conversation, chat.Turn) {
	env.t.Helper()
	var conv chat.Conversation
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations", `{}`, &conv); code != http.StatusCreated {
		env.t.Fatalf("create conversation: HTTP %d", code)
	}
	var submitted struct {
		Turn chat.Turn `json:"turn"`
	}
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/conversations/"+conv.ID+"/turns", fmt.Sprintf(`{"text":%q}`, text), &submitted); code != http.StatusAccepted {
		env.t.Fatalf("submit turn: HTTP %d", code)
	}
	srv := httptest.NewServer(env.mux)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, events := env.streamTurn(ctx, srv.URL, env.userToken, submitted.Turn.ID)
	defer resp.Body.Close()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				env.t.Fatal("event stream closed before a terminal event")
			}
			if ev.Type.Terminal() {
				return conv, submitted.Turn
			}
		case <-ctx.Done():
			env.t.Fatal("timed out reading events")
		}
	}
}

func forkAssertNoteCount(t *testing.T, env *forkChatEnv, want int) {
	t.Helper()
	recs, err := env.app.FindRecordsByFilter(env.noteCollection, "id != ''", "", 0, 0)
	if err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if len(recs) != want {
		t.Fatalf("stored %d notes, want %d", len(recs), want)
	}
}

// TestChatHTTPProposalApproveCreatesOnce is the reviewed-write path end to end
// on the fork: the model's create becomes a stored proposal instead of a write,
// the user's approval runs it exactly once, and a replayed decision creates
// nothing.
func TestChatHTTPProposalApproveCreatesOnce(t *testing.T) {
	env := newForkChatEnv(t, forkChatEnvOptions{
		allowWrites: true,
		proposals:   true,
		manifest: func(env *forkChatEnv) *pbmcp.Manifest {
			return forkWritableManifest(env.dealCollection, env.noteCollection)
		},
		steps: forkFixedSteps(
			scripted.Step{Chunks: []provider.Chunk{
				{Kind: provider.ChunkToolCall, ToolCall: &provider.ToolCall{
					ID: "call-1", Name: "create_notes", Arguments: json.RawMessage(`{"data":{"title":"Proposed note"}}`),
				}},
				{Kind: provider.ChunkDone},
			}},
			scripted.Step{Chunks: []provider.Chunk{
				{Kind: provider.ChunkText, Text: "Waiting for your review."},
				{Kind: provider.ChunkDone},
			}},
		),
	})

	var status struct {
		Features struct {
			Proposals bool `json:"proposals"`
		} `json:"features"`
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPath+"/status", "", &status); code != http.StatusOK {
		t.Fatalf("status: HTTP %d", code)
	}
	if !status.Features.Proposals {
		t.Fatal("status does not report proposals as enabled")
	}

	conv, _ := env.runTurn("create a note")
	ps, ok := env.store.(chat.ProposalStore)
	if !ok {
		t.Fatal("the enabled store does not implement chat.ProposalStore")
	}
	items, _, err := ps.ListProposals(context.Background(), env.proposalOwner(), conv.ID, nil, chat.Page{})
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("stored %d proposals, want 1", len(items))
	}
	p := items[0]
	if p.State != chat.ProposalStatePending || p.Location != chat.ProposalLocationServer || p.Tool != "create_notes" {
		t.Fatalf("stored proposal = %+v", p)
	}
	forkAssertNoteCount(t, env, 0)

	var decided struct {
		Proposal chat.Proposal `json:"proposal"`
		Replayed bool          `json:"replayed"`
	}
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/proposals/"+p.ID+"/decision",
		fmt.Sprintf(`{"decision":"approve","revision":%d}`, p.Revision), &decided); code != http.StatusOK {
		t.Fatalf("approve: HTTP %d", code)
	}
	if decided.Proposal.State != chat.ProposalStateSucceeded || decided.Replayed {
		t.Fatalf("approved proposal = %+v", decided.Proposal)
	}
	forkAssertNoteCount(t, env, 1)

	decided = struct {
		Proposal chat.Proposal `json:"proposal"`
		Replayed bool          `json:"replayed"`
	}{}
	if code := env.json(env.userToken, http.MethodPost, chatTestPath+"/proposals/"+p.ID+"/decision",
		fmt.Sprintf(`{"decision":"approve","revision":%d}`, p.Revision), &decided); code != http.StatusOK {
		t.Fatalf("replayed approve: HTTP %d", code)
	}
	if !decided.Replayed || decided.Proposal.State != chat.ProposalStateSucceeded {
		t.Fatalf("replayed decision = %+v", decided)
	}
	forkAssertNoteCount(t, env, 1)
}

// TestChatProposalRestartRecoveryMarksExecutingUnknown proves a startup turns a
// proposal that was executing when the previous process stopped into unknown,
// even when proposals are no longer enabled on that server.
func TestChatProposalRestartRecoveryMarksExecutingUnknown(t *testing.T) {
	env := newForkChatEnv(t, forkChatEnvOptions{
		allowWrites: true,
		proposals:   true,
		manifest: func(env *forkChatEnv) *pbmcp.Manifest {
			return forkWritableManifest(env.dealCollection, env.noteCollection)
		},
	})
	ps, ok := env.store.(chat.ProposalStore)
	if !ok {
		t.Fatal("the enabled store does not implement chat.ProposalStore")
	}
	ctx := context.Background()
	owner := env.proposalOwner()
	conv, err := env.store.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "doomed proposal"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := ps.CreateProposal(ctx, owner, chat.Proposal{
		ConversationID: conv.ID,
		Tool:           "create_notes",
		Location:       chat.ProposalLocationServer,
		Arguments:      json.RawMessage(`{"data":{"title":"Never applied"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	executing, err := ps.TransitionProposal(ctx, owner, p.ID, chat.ProposalStatePending, 1, chat.ProposalStateExecuting, chat.ProposalUpdate{Decided: true})
	if err != nil {
		t.Fatal(err)
	}

	// A second instance on the shared database is the next startup of this app.
	app := newChatForkApp(t)
	restarted, err := New(app, pbmcp.Config{
		Path:     "/mcp",
		Auth:     pbmcp.AuthConfig{Collections: []string{"users"}},
		Manifest: forkChatManifest(env.dealCollection),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.EnableChat(ChatConfig{
		Options:          pbmcp.ChatOptions{Namespace: env.namespace, Prefix: chatTestPath, Store: env.store},
		Provider:         scripted.New(),
		Model:            "scripted-model",
		CollectionPrefix: env.prefix,
	}); err != nil {
		t.Fatalf("EnableChat after restart: %v", err)
	}
	t.Cleanup(func() { _ = app.OnTerminate().Trigger(&core.TerminateEvent{App: app}) })

	got, err := ps.GetProposal(ctx, owner, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != chat.ProposalStateUnknown {
		t.Fatalf("state after restart = %s, want unknown", got.State)
	}
	if got.Failure != chat.UnknownOutcomeMessage || got.Revision != executing.Revision+1 {
		t.Fatalf("recovered proposal = %+v", got)
	}
	forkAssertNoteCount(t, env, 0)
}
