package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

func collectChunks(t *testing.T, ch <-chan provider.Chunk) []provider.Chunk {
	t.Helper()
	var out []provider.Chunk
	for chunk := range ch {
		out = append(out, chunk)
	}
	return out
}

func completedSSE(extra ...string) string {
	var b strings.Builder
	for _, block := range extra {
		b.WriteString(block)
	}
	b.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4},\"output\":[]}}\n\n")
	return b.String()
}

func TestResponsesRequestMapping(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, completedSSE())
	}))
	defer srv.Close()

	p, err := New(Config{BaseURL: srv.URL, APIKey: "key", WireAPI: WireResponses})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{
		Model:  "gpt-codex",
		System: "be brief",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Text: "hello"},
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)}}},
			{Role: provider.RoleTool, ToolCallID: "c1", ToolResult: json.RawMessage(`{"ok":true}`)},
		},
		Tools: []provider.ToolDef{{Name: "search", Description: "find", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	collectChunks(t, ch)

	if got["model"] != "gpt-codex" || got["instructions"] != "be brief" {
		t.Fatalf("body = %+v", got)
	}
	if got["stream"] != true || got["store"] != false {
		t.Fatalf("stream/store = %v/%v", got["stream"], got["store"])
	}
	if _, ok := got["max_output_tokens"]; ok {
		t.Fatal("responses request must not send max_output_tokens")
	}
	input, _ := got["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %+v", input)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "search" || tool["strict"] != false {
		t.Fatalf("tool = %+v", tool)
	}
}

func TestResponsesStreamToolCallAndUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, completedSSE(
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n",
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"search\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n",
		))
	}))
	defer srv.Close()
	p, err := New(Config{BaseURL: srv.URL, WireAPI: WireResponses})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectChunks(t, ch)
	var text, done bool
	var call *provider.ToolCall
	var usage *provider.Usage
	for _, chunk := range chunks {
		switch chunk.Kind {
		case provider.ChunkText:
			text = text || chunk.Text == "Hello"
		case provider.ChunkToolCall:
			call = chunk.ToolCall
		case provider.ChunkUsage:
			usage = chunk.Usage
		case provider.ChunkDone:
			done = true
			if chunk.StopReason != "tool_calls" {
				t.Fatalf("stop reason = %q", chunk.StopReason)
			}
		case provider.ChunkError:
			t.Fatalf("error chunk: %v", chunk.Err)
		}
	}
	if !text || !done || call == nil || call.ID != "c1" || call.Name != "search" {
		t.Fatalf("chunks = %+v", chunks)
	}
	if usage == nil || usage.InputTokens != 3 || usage.OutputTokens != 4 {
		t.Fatalf("usage = %+v", usage)
	}
}

type countingToken struct {
	mu    sync.Mutex
	calls int
}

func (c *countingToken) Token(context.Context, bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == 1 {
		return "old", nil
	}
	return "new", nil
}

func TestResponsesRefreshesOn401(t *testing.T) {
	var calls int
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth = append(auth, r.Header.Get("Authorization"))
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, completedSSE("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
	}))
	defer srv.Close()
	source := &countingToken{}
	p, err := New(Config{BaseURL: srv.URL, WireAPI: WireResponses, TokenSource: source})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectChunks(t, ch)
	for _, chunk := range chunks {
		if chunk.Kind == provider.ChunkError {
			t.Fatalf("error chunk: %v", chunk.Err)
		}
	}
	if calls != 2 || len(auth) != 2 || auth[0] != "Bearer old" || auth[1] != "Bearer new" {
		t.Fatalf("calls = %d auth = %v", calls, auth)
	}
}

func TestResponsesTruncatedStreamIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"half\"}\n\n")
	}))
	defer srv.Close()
	p, err := New(Config{BaseURL: srv.URL, WireAPI: WireResponses})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	sawError := false
	for chunk := range ch {
		if chunk.Kind == provider.ChunkError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("truncated stream did not produce an error chunk")
	}
}

func TestResponsesInvalidToolArgumentsDoNotComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, completedSSE(
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"search\",\"arguments\":\"not json\"}}\n\n",
		))
	}))
	defer srv.Close()
	p, err := New(Config{BaseURL: srv.URL, WireAPI: WireResponses})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	sawError, sawDone := false, false
	for chunk := range ch {
		switch chunk.Kind {
		case provider.ChunkError:
			sawError = true
		case provider.ChunkDone:
			sawDone = true
		}
	}
	if !sawError || sawDone {
		t.Fatalf("error=%v done=%v", sawError, sawDone)
	}
}

func TestResponsesFailedContextOverflow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"Your input exceeds the context window of this model.\"}}}\n\n")
	}))
	defer srv.Close()
	p, err := New(Config{BaseURL: srv.URL, WireAPI: WireResponses})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var failure error
	for chunk := range ch {
		if chunk.Kind == provider.ChunkError {
			failure = chunk.Err
		}
	}
	if !provider.IsContextOverflow(failure) {
		t.Fatalf("failure = %v, want a context overflow", failure)
	}
}

func TestResponsesIncompleteReasonsAreStops(t *testing.T) {
	for _, tc := range []struct{ frame, want string }{
		{`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, "max_output_tokens"},
		{`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`, "content_filter"},
		{`{"type":"response.completed","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, "max_output_tokens"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: "+tc.frame+"\n\n")
		}))
		p, err := New(Config{BaseURL: srv.URL, WireAPI: WireResponses})
		if err != nil {
			t.Fatal(err)
		}
		ch, err := p.Stream(context.Background(), provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}})
		if err != nil {
			t.Fatal(err)
		}
		var last provider.Chunk
		for c := range ch {
			last = c
		}
		srv.Close()
		if last.Kind != provider.ChunkDone || last.StopReason != tc.want {
			t.Fatalf("%s: last chunk = %+v, want done with %q", tc.frame, last, tc.want)
		}
	}
}
