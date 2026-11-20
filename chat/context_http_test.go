package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

func TestContextEndpointsAreQuietWithoutAWindow(t *testing.T) {
	f := newFixture(t, fixtureOptions{steps: nil})
	var status struct {
		Features map[string]any `json:"features"`
	}
	f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK).decode(&status)
	if _, ok := status.Features["contextManagement"]; ok {
		t.Fatalf("features = %v: contextManagement without a known window", status.Features)
	}
	if _, ok := status.Features["compaction"]; ok {
		t.Fatalf("features = %v: compaction while it is off", status.Features)
	}
	conv := f.createConversation(map[string]any{})
	res := f.call(http.MethodGet, testPrefix+"/conversations/"+conv.ID, nil, nil).expect(http.StatusOK)
	var detail map[string]json.RawMessage
	res.decode(&detail)
	if _, ok := detail["context"]; ok {
		t.Fatal("the detail view gained a context field with nothing managed")
	}
	var ctx ContextStatus
	f.call(http.MethodGet, testPrefix+"/conversations/"+conv.ID+"/context", nil, nil).expect(http.StatusOK).decode(&ctx)
	if ctx.Window != 0 || ctx.CanCompact {
		t.Fatalf("context = %+v, want an unknown window", ctx)
	}
	if code := f.call(http.MethodPost, testPrefix+"/conversations/"+conv.ID+"/compact", nil, nil).
		expect(http.StatusBadRequest).errorCode(); code != CodeUnsupported {
		t.Fatalf("compact with summaries off: code %q", code)
	}
}

func TestContextAndCompactEndpoints(t *testing.T) {
	prov := &summaryProvider{asked: make(chan string, 8)}
	f := newFixture(t, fixtureOptions{provider: prov, contextWindow: 50_000, compaction: true})
	var status struct {
		Features struct {
			ContextManagement bool `json:"contextManagement"`
			Compaction        bool `json:"compaction"`
		} `json:"features"`
	}
	f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK).decode(&status)
	if !status.Features.ContextManagement || !status.Features.Compaction {
		t.Fatalf("features = %+v", status.Features)
	}

	conv := f.createConversation(map[string]any{})
	// Too little to summarize yet.
	var early compactView
	f.call(http.MethodPost, testPrefix+"/conversations/"+conv.ID+"/compact", nil, nil).expect(http.StatusOK).decode(&early)
	if early.Compacted || early.Reason != "nothing_to_compact" {
		t.Fatalf("early compact = %+v", early)
	}

	for i := 0; i < 4; i++ {
		turn := f.submitTurn(conv.ID, fmt.Sprintf("question %d", i), "", nil)
		awaitTerminal(t, f.store, ownerFor("user-a"), turn.Turn.ID)
	}
	detail := f.conversation(conv.ID)
	if detail.Context == nil || detail.Context.Window != 50_000 || detail.Context.Tokens <= 0 || !detail.Context.CanCompact {
		t.Fatalf("detail context = %+v", detail.Context)
	}

	// The early attempt counts for the rate limit.
	if code := f.call(http.MethodPost, testPrefix+"/conversations/"+conv.ID+"/compact", nil, nil).
		expect(http.StatusTooManyRequests).errorCode(); code != CodeTooSoon {
		t.Fatalf("compact right after the last one: code %q", code)
	}

	// Another owner sees nothing.
	f.callAs(tokenB, http.MethodGet, testPrefix+"/conversations/"+conv.ID+"/context", nil, nil).expect(http.StatusNotFound)
	f.callAs(tokenB, http.MethodPost, testPrefix+"/conversations/"+conv.ID+"/compact", nil, nil).expect(http.StatusNotFound)
}

func TestStoredContextsReachSetup(t *testing.T) {
	var seen [][]json.RawMessage
	f := newFixture(t, fixtureOptions{
		steps: []scripted.Step{textStep("one"), textStep("two"), textStep("three")}, turnContext: true, storeContext: true,
		setup: func(_ context.Context, in TurnInput) (TurnSetup, error) {
			seen = append(seen, in.EarlierContexts)
			return TurnSetup{}, nil
		},
	})
	conv := f.createConversation(map[string]any{})
	send := func(text string, ctx any) {
		body := map[string]any{"text": text}
		if ctx != nil {
			body["context"] = ctx
		}
		var out submitTurnView
		f.call(http.MethodPost, testPrefix+"/conversations/"+conv.ID+"/turns", body, nil).expect(http.StatusAccepted).decode(&out)
		awaitTerminal(t, f.store, ownerFor("user-a"), out.Turn.ID)
	}
	send("about contact 1", map[string]any{"refs": []any{"contact:1"}})
	send("no references", nil)
	send("and campaign 7", map[string]any{"refs": []any{"campaign:7"}})

	if len(seen) != 3 || len(seen[0]) != 0 || len(seen[1]) != 1 || len(seen[2]) != 1 {
		t.Fatalf("earlier contexts per turn = %d %v", len(seen), seen)
	}
	if !strings.Contains(string(seen[2][0]), "contact:1") {
		t.Fatalf("third turn saw %s", seen[2][0])
	}
	// The transcript keeps the context on the user message; the model never
	// gets it in the history.
	detail := f.conversation(conv.ID)
	var stored int
	for _, m := range detail.Messages {
		for _, p := range m.Parts {
			if p.Type == PartContext {
				stored++
			}
		}
	}
	if stored != 2 {
		t.Fatalf("stored contexts = %d, want 2", stored)
	}
	for _, req := range f.prov.Requests() {
		for _, m := range req.Messages {
			if strings.Contains(m.Text, "campaign:7") || strings.Contains(m.Text, "contact:1") {
				t.Fatalf("a stored context reached the model's history: %q", m.Text)
			}
		}
	}

	if _, err := NewHandler(HandlerConfig{Prefix: testPrefix, Runner: mustRunner(t, RunnerConfig{Store: f.store, Provider: scripted.New(), Model: "m"}),
		Store: f.store, Auth: newFakeAuth(), StoreContext: true}); err == nil {
		t.Fatal("StoreContext without TurnContext was accepted")
	}
}
