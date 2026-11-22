package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// captureBody serves one streamed reply and returns the decoded request body.
func captureBody(t *testing.T, wire string, req provider.Request) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if wire == WireResponses {
			_, _ = fmt.Fprint(w, completedSSE())
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p, err := New(Config{BaseURL: srv.URL, APIKey: "key", WireAPI: wire})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	collectChunks(t, ch)
	return got
}

func TestReasoningEffortOnTheWire(t *testing.T) {
	req := provider.Request{Model: "gpt-5", Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}}

	// Chat Completions: top-level reasoning_effort, absent by default.
	if got := captureBody(t, WireChatCompletions, req); got["reasoning_effort"] != nil {
		t.Fatalf("default request sent reasoning_effort: %v", got["reasoning_effort"])
	}
	req.ReasoningEffort = "high"
	if got := captureBody(t, WireChatCompletions, req); got["reasoning_effort"] != "high" {
		t.Fatalf("chat reasoning_effort = %v, want high", got["reasoning_effort"])
	}

	// Responses (and the Codex plan route): reasoning.effort.
	req.ReasoningEffort = ""
	if got := captureBody(t, WireResponses, req); got["reasoning"] != nil {
		t.Fatalf("default responses request sent reasoning: %v", got["reasoning"])
	}
	req.ReasoningEffort = "low"
	got := captureBody(t, WireResponses, req)
	reasoning, _ := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Fatalf("responses reasoning = %v, want effort low", got["reasoning"])
	}
	req.ReasoningEffort = "max"
	got = captureBody(t, WireResponses, req)
	reasoning, _ = got["reasoning"].(map[string]any)
	if reasoning["effort"] != "max" {
		t.Fatalf("responses reasoning = %v, want effort max", got["reasoning"])
	}
}

func TestUnknownReasoningEffortRefused(t *testing.T) {
	p, err := New(Config{BaseURL: "https://example.test/v1", APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Stream(context.Background(), provider.Request{
		Model: "gpt-5", ReasoningEffort: "maximum",
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}},
	})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}
