package pbpostgress

// Multi-instance chat tests: two fork apps ("instances") share one PostgreSQL
// database, exactly like the production deployments the fork targets. Chat
// collections are shared (System) collections partitioned by the owner key, so
// the two invariants to prove are:
//
//   - one app's start-up recovery never ends another app's running turn; and
//   - one app's chat API never exposes another app's conversations, even for
//     the very same authenticated user, when their namespaces differ.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"

	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

func forkChatConfig() pbmcp.Config {
	return pbmcp.Config{
		Path:     "/mcp",
		Auth:     pbmcp.AuthConfig{Collections: []string{"users"}},
		Manifest: &pbmcp.Manifest{SchemaVersion: "1"},
	}
}

func TestChatMultiInstanceScoping(t *testing.T) {
	appA := newChatForkApp(t)
	appB := newChatForkApp(t)
	// Exercise the same OnTerminate hooks production uses, so the chat
	// shutdown (and the proposal sweep) stops with each app.
	t.Cleanup(func() {
		_ = appA.OnTerminate().Trigger(&core.TerminateEvent{App: appA})
		_ = appB.OnTerminate().Trigger(&core.TerminateEvent{App: appB})
	})

	idA, err := core.AsBaseApp(appA).InstanceID()
	if err != nil {
		t.Fatal(err)
	}
	idB, err := core.AsBaseApp(appB).InstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if idA == idB {
		t.Fatal("expected two distinct instances")
	}

	// B sees only the auth collection. Chat collections are System
	// collections, so the store must still reach them although B's scope
	// would hide an ordinary collection.
	if err := core.AsBaseApp(appB).SetInstanceScope(core.InstanceScope{Mode: "allow", Collections: []string{"users"}}); err != nil {
		t.Fatal(err)
	}
	users, err := appA.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	salt := strings.ToLower(security.RandomString(6))
	user := core.NewRecord(users)
	user.SetEmail("multi-chat-" + salt + "@example.com")
	user.SetPassword("multi-password-12345")
	if err := appA.Save(user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteWithoutHooks(appA, user) })
	token, err := user.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	prefix := chatTestPrefix(t)
	nsA, nsB := "app-a-"+salt, "app-b-"+salt
	srvA, err := New(appA, forkChatConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := srvA.EnableChat(ChatConfig{
		Options:          pbmcp.ChatOptions{Namespace: nsA},
		Provider:         scripted.New(),
		Model:            "scripted-model",
		CollectionPrefix: prefix,
	}); err != nil {
		t.Fatalf("EnableChat on A: %v", err)
	}
	srvB, err := New(appB, forkChatConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := srvB.EnableChat(ChatConfig{
		Options:          pbmcp.ChatOptions{Namespace: nsB},
		Provider:         scripted.New(),
		Model:            "scripted-model",
		CollectionPrefix: prefix,
	}); err != nil {
		t.Fatalf("EnableChat on B: %v", err)
	}
	muxA := forkChatMux(t, srvA, appA)
	muxB := forkChatMux(t, srvB, appB)

	jsonPost := func(mux http.Handler, method, path, body, tok string) (int, []byte) {
		rec := forkChatServe(mux, method, path, body, tok)
		return rec.Code, rec.Body.Bytes()
	}

	// The same user identity is accepted by both apps.
	for name, mux := range map[string]http.Handler{"A": muxA, "B": muxB} {
		if code, body := jsonPost(mux, http.MethodGet, chatTestPath+"/status", "", token); code != http.StatusOK {
			t.Fatalf("%s status = HTTP %d %s", name, code, body)
		}
	}

	// A creates a conversation; B must not see it, read it or list it.
	var convA chat.Conversation
	if code, body := jsonPost(muxA, http.MethodPost, chatTestPath+"/conversations", `{"title":"A only"}`, token); code != http.StatusCreated {
		t.Fatalf("A create = HTTP %d %s", code, body)
	} else if err := json.Unmarshal(body, &convA); err != nil {
		t.Fatal(err)
	}
	if code, _ := jsonPost(muxB, http.MethodGet, chatTestPath+"/conversations/"+convA.ID, "", token); code != http.StatusNotFound {
		t.Fatalf("B read of A's conversation = HTTP %d, want 404", code)
	}
	var listB struct {
		Items []chat.Conversation `json:"items"`
	}
	if code, body := jsonPost(muxB, http.MethodGet, chatTestPath+"/conversations", "", token); code != http.StatusOK {
		t.Fatalf("B list = HTTP %d %s", code, body)
	} else if err := json.Unmarshal(body, &listB); err != nil {
		t.Fatal(err)
	}
	if len(listB.Items) != 0 {
		t.Fatalf("B sees %d of A's conversations", len(listB.Items))
	}

	// B can create its own; A must not see that one either.
	var convB chat.Conversation
	if code, body := jsonPost(muxB, http.MethodPost, chatTestPath+"/conversations", `{"title":"B only"}`, token); code != http.StatusCreated {
		t.Fatalf("B create = HTTP %d %s", code, body)
	} else if err := json.Unmarshal(body, &convB); err != nil {
		t.Fatal(err)
	}
	if code, _ := jsonPost(muxA, http.MethodGet, chatTestPath+"/conversations/"+convB.ID, "", token); code != http.StatusNotFound {
		t.Fatalf("A read of B's conversation = HTTP %d, want 404", code)
	}

	// Chat collections stay unreachable through B's records REST API.
	for _, name := range []string{prefix + "Conversations", prefix + "Turns", prefix + "Events"} {
		code, body := jsonPost(muxB, http.MethodGet, "/api/collections/"+name+"/records", "", token)
		if code != http.StatusNotFound && code != http.StatusForbidden {
			t.Fatalf("B records %s = HTTP %d %s, want a refusal", name, code, body)
		}
		if strings.Contains(string(body), convA.ID) || strings.Contains(string(body), "A only") {
			t.Fatalf("B records %s leaked A's chat data: %s", name, body)
		}
	}
	// And they remain absent from B's MCP manifest.
	rec := forkChatServe(muxB, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, token)
	var rpcOut struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rpcOut); err != nil {
		t.Fatalf("tools/list: %v: %s", err, rec.Body.String())
	}
	for _, tool := range rpcOut.Result.Tools {
		name, _ := tool["name"].(string)
		if strings.Contains(strings.ToLower(name), "chat") {
			t.Fatalf("chat collection leaked into B's MCP tools as %q", name)
		}
	}

	// --- restart recovery is scoped ---

	storeA, ok := srvA.ChatStorage()
	if !ok {
		t.Fatal("A has no chat store")
	}
	ownerA := chat.OwnerKey{Namespace: nsA, Mount: "/mcp", AuthCollectionID: users.Id, UserID: user.Id}
	conv, err := storeA.CreateConversation(context.Background(), chat.Conversation{Owner: ownerA, Title: "running on A"})
	if err != nil {
		t.Fatal(err)
	}
	turn, created, err := storeA.CreateTurn(context.Background(), ownerA, chat.Turn{ConversationID: conv.ID, IdempotencyKey: "a-restart"})
	if err != nil || !created {
		t.Fatalf("A CreateTurn = %v, %v", created, err)
	}
	if _, err := storeA.TransitionTurn(context.Background(), ownerA, turn.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil); err != nil {
		t.Fatal(err)
	}

	// B "restarts": a fresh server over B runs its scoped recovery. It must
	// leave A's running turn untouched.
	restartedB, err := New(appB, forkChatConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedB.EnableChat(ChatConfig{
		Options:          pbmcp.ChatOptions{Namespace: nsB},
		Provider:         scripted.New(),
		Model:            "scripted-model",
		CollectionPrefix: prefix,
	}); err != nil {
		t.Fatalf("B restart EnableChat: %v", err)
	}
	if stored, err := storeA.GetTurn(context.Background(), ownerA, turn.ID); err != nil {
		t.Fatal(err)
	} else if stored.State != chat.TurnStateRunning {
		t.Fatalf("B's restart interrupted A's turn: state = %s", stored.State)
	}

	// A's own restart does interrupt it (and only it).
	restartedA, err := New(appA, forkChatConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedA.EnableChat(ChatConfig{
		Options:          pbmcp.ChatOptions{Namespace: nsA},
		Provider:         scripted.New(),
		Model:            "scripted-model",
		CollectionPrefix: prefix,
	}); err != nil {
		t.Fatalf("A restart EnableChat: %v", err)
	}
	stored, err := storeA.GetTurn(context.Background(), ownerA, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != chat.TurnStateInterrupted {
		t.Fatalf("state after A's restart = %s, want interrupted", stored.State)
	}
	if stored.Error == nil || stored.Error.Code != chat.FailureInterrupted {
		t.Fatalf("failure after A's restart = %+v", stored.Error)
	}
	events, err := storeA.EventsAfter(context.Background(), ownerA, turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, ev := range events {
		if ev.Type.Terminal() {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly 1", terminals)
	}
}

func forkChatMux(t *testing.T, srv *Server, app core.App) http.Handler {
	t.Helper()
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterRoutes(r); err != nil {
		t.Fatal(err)
	}
	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	return mux
}

func forkChatServe(mux http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}
