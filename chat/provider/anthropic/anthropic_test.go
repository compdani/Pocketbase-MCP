package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
)

const testKey = "sk-ant-test-secret-key"

func collect(t *testing.T, ch <-chan provider.Chunk) []provider.Chunk {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	var out []provider.Chunk
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, c)
		case <-deadline.C:
			t.Fatalf("stream did not close, saw %d chunks: %+v", len(out), out)
		}
	}
}

type recordedRequest struct {
	header http.Header
	path   string
	body   []byte
}

// sseServer records requests and serves the given frames, each written in
// tiny writes so the reader faces arbitrary read boundaries.
func sseServer(t *testing.T, frames ...string) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		mu.Lock()
		reqs = append(reqs, recordedRequest{header: r.Header.Clone(), path: r.URL.Path, body: body})
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range frames {
			for _, b := range []byte(frame) {
				if _, err := w.Write([]byte{b}); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]recordedRequest, len(reqs))
		copy(out, reqs)
		return out
	}
}

// event renders one SSE frame with its event name.
func event(typ, data string) string {
	return "event: " + typ + "\ndata: " + data + "\n\n"
}

func newProvider(t *testing.T, baseURL string) *Provider {
	t.Helper()
	p, err := New(Config{BaseURL: baseURL + "/", APIKey: testKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func toolSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
		"required":   []any{"q"},
	}
}

// requestBody mirrors the parts of the wire request the tests assert.
type requestBody struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	System    string `json:"system"`
	Stream    bool   `json:"stream"`
	Messages  []struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   string          `json:"content"`
			IsError   bool            `json:"is_error"`
		} `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	} `json:"tools"`
}

func decodeBody(t *testing.T, raw []byte) requestBody {
	t.Helper()
	var body requestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return body
}

func TestRequestShape(t *testing.T) {
	srv, requests := sseServer(t, event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`),
		event("message_stop", `{"type":"message_stop"}`))
	p := newProvider(t, srv.URL)

	req := provider.Request{
		Model:  "claude-test",
		System: "sys prompt",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Text: "first"},
			{Role: provider.RoleUser, Text: "second"},
			{Role: provider.RoleAssistant, Text: "calling", ToolCalls: []provider.ToolCall{
				{ID: "tu_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)},
				{ID: "tu_2", Name: "lookup", Arguments: json.RawMessage(`{"q":"y"}`)},
			}},
			{Role: provider.RoleTool, ToolCallID: "tu_1", ToolName: "lookup", ToolResult: json.RawMessage(`{"a":1}`)},
			{Role: provider.RoleTool, ToolCallID: "tu_2", ToolName: "lookup", ToolResult: json.RawMessage(`{"error":"nope"}`), ToolIsError: true},
			{Role: provider.RoleUser, Text: "after"},
		},
		Tools: []provider.ToolDef{{Name: "lookup", Description: "find", InputSchema: toolSchema()}},
	}
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	if len(got) != 2 || got[0].Kind != provider.ChunkUsage || got[1].Kind != provider.ChunkDone {
		t.Fatalf("chunks = %+v", got)
	}

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests", len(reqs))
	}
	if reqs[0].path != "/v1/messages" {
		t.Fatalf("path = %q", reqs[0].path)
	}
	if got := reqs[0].header.Get("x-api-key"); got != testKey {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := reqs[0].header.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got)
	}
	if got := reqs[0].header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}

	body := decodeBody(t, reqs[0].body)
	if body.Model != "claude-test" || body.System != "sys prompt" || !body.Stream {
		t.Fatalf("request = %+v", body)
	}
	if body.MaxTokens != defaultMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", body.MaxTokens, defaultMaxTokens)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %d: %+v", len(body.Messages), body.Messages)
	}

	user := body.Messages[0]
	if user.Role != "user" || len(user.Content) != 2 ||
		user.Content[0].Type != "text" || user.Content[0].Text != "first" ||
		user.Content[1].Text != "second" {
		t.Fatalf("merged user message = %+v", user)
	}

	assistant := body.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 3 {
		t.Fatalf("assistant message = %+v", assistant)
	}
	if assistant.Content[0].Type != "text" || assistant.Content[0].Text != "calling" {
		t.Fatalf("assistant text block = %+v", assistant.Content[0])
	}
	for i, want := range []struct{ id, name, input string }{
		{"tu_1", "lookup", `{"q":"x"}`},
		{"tu_2", "lookup", `{"q":"y"}`},
	} {
		block := assistant.Content[i+1]
		if block.Type != "tool_use" || block.ID != want.id || block.Name != want.name {
			t.Fatalf("tool_use block %d = %+v", i, block)
		}
		if string(block.Input) != want.input {
			t.Fatalf("tool_use input %d = %s, want %s", i, block.Input, want.input)
		}
	}

	results := body.Messages[2]
	if results.Role != "user" || len(results.Content) != 3 {
		t.Fatalf("tool result message = %+v", results)
	}
	// Both results must be in one user message, ahead of the following text.
	if results.Content[0].Type != "tool_result" || results.Content[0].ToolUseID != "tu_1" ||
		results.Content[0].Content != `{"a":1}` || results.Content[0].IsError {
		t.Fatalf("first tool result = %+v", results.Content[0])
	}
	if results.Content[1].Type != "tool_result" || results.Content[1].ToolUseID != "tu_2" ||
		results.Content[1].Content != `{"error":"nope"}` || !results.Content[1].IsError {
		t.Fatalf("second tool result = %+v", results.Content[1])
	}
	if results.Content[2].Type != "text" || results.Content[2].Text != "after" {
		t.Fatalf("trailing text = %+v", results.Content[2])
	}

	if len(body.Tools) != 1 || body.Tools[0].Name != "lookup" || body.Tools[0].Description != "find" {
		t.Fatalf("tools = %+v", body.Tools)
	}
	if body.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("input_schema = %+v", body.Tools[0].InputSchema)
	}
}

func TestMaxTokensOverride(t *testing.T) {
	srv, requests := sseServer(t, event("message_stop", `{"type":"message_stop"}`))
	p, err := New(Config{BaseURL: srv.URL, APIKey: testKey, DefaultMaxTokens: 123})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m", MaxOutputTokens: 7})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, ch)
	if body := decodeBody(t, requests()[0].body); body.MaxTokens != 7 {
		t.Fatalf("max_tokens = %d, want 7", body.MaxTokens)
	}

	ch, err = p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, ch)
	if body := decodeBody(t, requests()[1].body); body.MaxTokens != 123 {
		t.Fatalf("max_tokens = %d, want 123", body.MaxTokens)
	}
}

func TestTextStreaming(t *testing.T) {
	srv, _ := sseServer(t,
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":1}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		event("ping", `{"type":"ping"}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`),
		event("message_stop", `{"type":"message_stop"}`),
	)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)

	if len(got) != 4 {
		t.Fatalf("chunks = %+v", got)
	}
	if got[0].Kind != provider.ChunkText || got[0].Text != "Hel" ||
		got[1].Kind != provider.ChunkText || got[1].Text != "lo" {
		t.Fatalf("text chunks = %+v", got[:2])
	}
	if got[2].Kind != provider.ChunkUsage || got[2].Usage.InputTokens != 9 || got[2].Usage.OutputTokens != 4 {
		t.Fatalf("usage chunk = %+v", got[2])
	}
	if got[3].Kind != provider.ChunkDone || got[3].StopReason != "end_turn" {
		t.Fatalf("done chunk = %+v", got[3])
	}
}

func TestToolUseStreaming(t *testing.T) {
	srv, _ := sseServer(t,
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"lookup","input":{}}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_2","name":"other","input":{}}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":1}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`),
		event("message_stop", `{"type":"message_stop"}`),
	)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)

	var calls []provider.ToolCall
	for _, c := range got {
		if c.Kind == provider.ChunkToolCall {
			if c.ToolCall == nil {
				t.Fatal("nil tool call")
			}
			calls = append(calls, *c.ToolCall)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls = %+v (chunks %+v)", calls, got)
	}
	if calls[0].ID != "tu_1" || calls[0].Name != "lookup" || string(calls[0].Arguments) != `{"q":"x"}` {
		t.Fatalf("first call = %+v", calls[0])
	}
	if calls[1].ID != "tu_2" || calls[1].Name != "other" || string(calls[1].Arguments) != `{}` {
		t.Fatalf("empty input call = %+v", calls[1])
	}

	last := got[len(got)-1]
	if last.Kind != provider.ChunkDone || last.StopReason != "tool_use" {
		t.Fatalf("last chunk = %+v", last)
	}
	var usage *provider.Usage
	for _, c := range got {
		if c.Kind == provider.ChunkUsage {
			usage = c.Usage
		}
	}
	if usage == nil || usage.InputTokens != 3 || usage.OutputTokens != 12 {
		t.Fatalf("usage = %+v", usage)
	}
	if got[len(got)-2].Kind != provider.ChunkUsage {
		t.Fatalf("usage must precede done: %+v", got)
	}
}

func TestUnknownContentBlockTypeIgnored(t *testing.T) {
	srv, _ := sseServer(t,
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":2}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"internal"}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"visible"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":1}`),
		event("future_event", `{"type":"future_event","payload":{"x":1}}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`),
		event("message_stop", `{"type":"message_stop"}`),
	)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	if len(got) != 3 {
		t.Fatalf("chunks = %+v", got)
	}
	if got[0].Kind != provider.ChunkText || got[0].Text != "visible" {
		t.Fatalf("text = %+v", got[0])
	}
	if got[2].Kind != provider.ChunkDone {
		t.Fatalf("last chunk = %+v", got[2])
	}
}

func TestErrorEvent(t *testing.T) {
	srv, _ := sseServer(t,
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
		event("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	if len(got) != 1 || got[0].Kind != provider.ChunkError {
		t.Fatalf("chunks = %+v", got)
	}
	msg := got[0].Err.Error()
	if !strings.Contains(msg, "overloaded_error") || !strings.Contains(msg, "Overloaded") {
		t.Fatalf("error = %v", msg)
	}
}

func TestTruncatedStreamFails(t *testing.T) {
	srv, _ := sseServer(t,
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
	)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	last := got[len(got)-1]
	if last.Kind != provider.ChunkError {
		t.Fatalf("chunks = %+v", got)
	}
}

func TestInvalidToolInputFails(t *testing.T) {
	srv, _ := sseServer(t,
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"f","input":{}}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("message_stop", `{"type":"message_stop"}`),
	)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	last := got[len(got)-1]
	if last.Kind != provider.ChunkError || !strings.Contains(last.Err.Error(), "invalid JSON") {
		t.Fatalf("chunks = %+v", got)
	}
}

func TestHTTPErrors(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key `+testKey+`"}}`)
			}))
			defer srv.Close()

			p := newProvider(t, srv.URL)
			ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			got := collect(t, ch)
			if len(got) != 1 || got[0].Kind != provider.ChunkError {
				t.Fatalf("chunks = %+v", got)
			}
			var herr *HTTPError
			if !errors.As(got[0].Err, &herr) {
				t.Fatalf("error is %T, want *HTTPError", got[0].Err)
			}
			if herr.StatusCode != tc.status || herr.Retryable() != tc.retryable {
				t.Fatalf("error = %+v retryable=%v", herr, herr.Retryable())
			}
			if msg := got[0].Err.Error(); strings.Contains(msg, testKey) {
				t.Fatalf("error leaked the API key: %s", msg)
			}
		})
	}
}

func TestStreamRejectsInvalidRequest(t *testing.T) {
	p, err := New(Config{APIKey: testKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Stream(context.Background(), provider.Request{}); err == nil {
		t.Fatal("request without model accepted")
	}
	_, err = p.Stream(context.Background(), provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: provider.RoleTool, ToolCallID: "c", ToolResult: json.RawMessage(`not json`)}},
	})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Stream error = %v, want ErrUnsupported", err)
	}
}

func TestEmptyMessageRejected(t *testing.T) {
	p, err := New(Config{APIKey: testKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Stream(context.Background(), provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: provider.RoleUser}},
	})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Stream error = %v, want ErrUnsupported", err)
	}
}

func TestOversizeRequestRejected(t *testing.T) {
	p, err := New(Config{APIKey: testKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Stream(context.Background(), provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: provider.RoleUser, Text: strings.Repeat("a", MaxRequestBodyBytes)}},
	})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Stream error = %v, want ErrUnsupported", err)
	}
}

func TestContextCancellationClosesStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		_, _ = io.WriteString(w, event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"one"}}`))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Stream(ctx, provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	select {
	case c := <-ch:
		if c.Kind != provider.ChunkText || c.Text != "one" {
			t.Fatalf("first chunk = %+v", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first chunk never arrived")
	}

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
				case <-time.After(time.Second):
					t.Fatal("stream did not close after cancellation")
				}
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close promptly after cancellation")
	}
}

func TestNameDefaultAndOverride(t *testing.T) {
	p, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "anthropic" {
		t.Fatalf("Name = %q", p.Name())
	}
	named, err := New(Config{Name: "proxy", Version: "2024-01-01"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if named.Name() != "proxy" || named.version != "2024-01-01" {
		t.Fatalf("provider = %+v", named)
	}
	if _, err := New(Config{BaseURL: "ftp://example.com"}); err == nil {
		t.Fatal("non-HTTP base URL accepted")
	}
}

// --- end to end through the chat runner ---

type fakeTools struct {
	mu    sync.Mutex
	calls int
	args  []json.RawMessage
}

func (f *fakeTools) Tools(context.Context) ([]provider.ToolDef, error) {
	return []provider.ToolDef{{Name: "lookup", Description: "find", InputSchema: toolSchema()}}, nil
}

func (f *fakeTools) Call(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.args = append(f.args, append(json.RawMessage(nil), args...))
	return json.RawMessage(`{"answer":42}`), nil
}

func TestEndToEndToolRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		n := step
		step++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if n == 0 {
			write(event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5}}}`))
			write(event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"lookup","input":{}}}`))
			write(event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`))
			write(event("content_block_stop", `{"type":"content_block_stop","index":0}`))
			write(event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`))
			write(event("message_stop", `{"type":"message_stop"}`))
			return
		}
		write(event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":8}}}`))
		write(event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		write(event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`))
		write(event("content_block_stop", `{"type":"content_block_stop","index":0}`))
		write(event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`))
		write(event("message_stop", `{"type":"message_stop"}`))
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL)
	store := chat.NewMemStore(chat.MemStoreOptions{})
	runner, err := chat.NewRunner(chat.RunnerConfig{Store: store, Provider: p, Model: "claude-test", System: "be helpful"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})
	owner := chat.OwnerKey{Namespace: "ns", Mount: "mnt", AuthCollectionID: "users", UserID: "u1"}
	conv, err := store.CreateConversation(context.Background(), chat.Conversation{Owner: owner, Title: "t"})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	tools := &fakeTools{}
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "find x", "key-1", tools, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	var final chat.Turn
	deadline := time.Now().Add(10 * time.Second)
	for {
		final, err = store.GetTurn(context.Background(), owner, turn.ID)
		if err != nil {
			t.Fatalf("GetTurn: %v", err)
		}
		if final.State.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn did not finish, state %s", final.State)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if final.State != chat.TurnStateCompleted {
		t.Fatalf("turn = %s (%+v)", final.State, final.Error)
	}

	tools.mu.Lock()
	calls, args := tools.calls, tools.args
	tools.mu.Unlock()
	if calls != 1 || len(args) != 1 || string(args[0]) != `{"q":"x"}` {
		t.Fatalf("tool calls = %d args = %+v", calls, args)
	}

	mu.Lock()
	reqs := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d requests", len(reqs))
	}
	body := decodeBody(t, reqs[1])
	if len(body.Messages) != 3 {
		t.Fatalf("second request messages = %+v", body.Messages)
	}
	assistant := body.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 || assistant.Content[0].Type != "tool_use" {
		t.Fatalf("assistant history = %+v", assistant)
	}
	if assistant.Content[0].ID != "tu_1" || assistant.Content[0].Name != "lookup" ||
		string(assistant.Content[0].Input) != `{"q":"x"}` {
		t.Fatalf("tool_use history = %+v", assistant.Content[0])
	}
	result := body.Messages[2]
	if result.Role != "user" || len(result.Content) != 1 || result.Content[0].Type != "tool_result" ||
		result.Content[0].ToolUseID != "tu_1" || result.Content[0].Content != `{"answer":42}` ||
		result.Content[0].IsError {
		t.Fatalf("tool_result history = %+v", result.Content)
	}
}

// Extended thinking is not wired up, so an effort is refused instead of
// silently dropped.
func TestReasoningEffortRefused(t *testing.T) {
	p, err := New(Config{BaseURL: "https://example.invalid", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Stream(context.Background(), provider.Request{
		Model: "claude", ReasoningEffort: "high",
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}},
	})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}
