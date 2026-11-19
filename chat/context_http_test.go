package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
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
