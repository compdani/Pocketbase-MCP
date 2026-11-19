package openai

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

const testKey = "sk-test-secret-key"

// collect drains ch, failing the test if the provider never closes it.
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
	body   []byte
}

// sseServer serves the recorded requests and replies with the given SSE
// payloads, writing each frame in tiny writes so the reader faces arbitrary
// boundaries.
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
		reqs = append(reqs, recordedRequest{header: r.Header.Clone(), body: body})
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range frames {
			for _, b := range []byte("data: " + frame + "\n\n") {
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

func TestRequestShapeAndTextStreaming(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3}}`,
		`[DONE]`,
	}
	srv, requests := sseServer(t, frames...)
	p := newProvider(t, srv.URL)

	req := provider.Request{
		Model:           "gpt-test",
		System:          "be brief",
		MaxOutputTokens: 64,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Text: "hello"},
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)},
			}},
			{Role: provider.RoleTool, ToolCallID: "call_1", ToolName: "lookup", ToolResult: json.RawMessage(`{"ok":true}`)},
		},
		Tools: []provider.ToolDef{{Name: "lookup", Description: "find", InputSchema: toolSchema()}},
	}
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)

	if len(got) != 4 {
		t.Fatalf("chunks = %+v", got)
	}
	if got[0].Kind != provider.ChunkText || got[0].Text != "Hel" || got[1].Text != "lo" {
		t.Fatalf("text chunks = %+v", got[:2])
	}
	if got[2].Kind != provider.ChunkUsage || got[2].Usage == nil ||
		got[2].Usage.InputTokens != 11 || got[2].Usage.OutputTokens != 3 {
		t.Fatalf("usage chunk = %+v", got[2])
	}
	if got[3].Kind != provider.ChunkDone || got[3].StopReason != "stop" {
		t.Fatalf("done chunk = %+v", got[3])
	}

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests", len(reqs))
	}
	if got := reqs[0].header.Get("Authorization"); got != "Bearer "+testKey {
		t.Fatalf("Authorization = %q", got)
	}

	var body struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		MaxTokens int `json:"max_tokens"`
		Messages  []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(reqs[0].body, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Model != "gpt-test" || !body.Stream || body.MaxTokens != 64 {
		t.Fatalf("request = %+v", body)
	}
	if body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options = %+v", body.StreamOptions)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("messages = %+v", body.Messages)
	}
	if body.Messages[0].Role != "system" || string(body.Messages[0].Content) != `"be brief"` {
		t.Fatalf("system message = %+v", body.Messages[0])
	}
	if body.Messages[1].Role != "user" || string(body.Messages[1].Content) != `"hello"` {
		t.Fatalf("user message = %+v", body.Messages[1])
	}
	assistant := body.Messages[2]
	if assistant.Role != "assistant" || string(assistant.Content) != "null" {
		t.Fatalf("assistant message = %+v", assistant)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant tool_calls = %+v", assistant.ToolCalls)
	}
	if c := assistant.ToolCalls[0]; c.ID != "call_1" || c.Type != "function" ||
		c.Function.Name != "lookup" || c.Function.Arguments != `{"q":"x"}` {
		t.Fatalf("assistant tool call = %+v", c)
	}
	tool := body.Messages[3]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || string(tool.Content) != `"{\"ok\":true}"` {
		t.Fatalf("tool message = %+v", tool)
	}
	if len(body.Tools) != 1 || body.Tools[0].Type != "function" ||
		body.Tools[0].Function.Name != "lookup" || body.Tools[0].Function.Description != "find" {
		t.Fatalf("tools = %+v", body.Tools)
	}
	if body.Tools[0].Function.Parameters["type"] != "object" {
		t.Fatalf("tool parameters = %+v", body.Tools[0].Function.Parameters)
	}
}

func TestInterleavedToolCallFragments(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"alpha","arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"beta","arguments":"{\"y\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9}}`,
		`[DONE]`,
	}
	srv, _ := sseServer(t, frames...)
	p := newProvider(t, srv.URL)

	ch, err := p.Stream(context.Background(), provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "go"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)

	want := []struct{ id, name, args string }{
		{"call_a", "alpha", `{"x":1}`},
		{"call_b", "beta", `{"y":2}`},
	}
	var calls []provider.ToolCall
	for _, c := range got {
		switch c.Kind {
		case provider.ChunkToolCall:
			if c.ToolCall == nil {
				t.Fatalf("nil tool call chunk")
			}
			calls = append(calls, *c.ToolCall)
		case provider.ChunkText:
			t.Fatalf("unexpected text chunk %q", c.Text)
		}
	}
	if len(calls) != len(want) {
		t.Fatalf("tool calls = %+v (chunks %+v)", calls, got)
	}
	for i, w := range want {
		if calls[i].ID != w.id || calls[i].Name != w.name || string(calls[i].Arguments) != w.args {
			t.Fatalf("call %d = %+v, want %+v", i, calls[i], w)
		}
	}
	last := got[len(got)-1]
	if last.Kind != provider.ChunkDone || last.StopReason != "tool_calls" {
		t.Fatalf("last chunk = %+v", last)
	}
	// The usage chunk must precede the done chunk so the runner records it.
	sawUsage := false
	for _, c := range got {
		if c.Kind == provider.ChunkUsage {
			if c.Usage.InputTokens != 7 || c.Usage.OutputTokens != 9 {
				t.Fatalf("usage = %+v", c.Usage)
			}
			sawUsage = true
		}
		if c.Kind == provider.ChunkDone && !sawUsage {
			t.Fatal("done arrived before usage")
		}
	}
}

func TestStreamEndingWithoutDoneFails(t *testing.T) {
	srv, _ := sseServer(t, `{"choices":[{"delta":{"content":"partial"}}]}`)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	if len(got) != 2 || got[0].Kind != provider.ChunkText || got[1].Kind != provider.ChunkError {
		t.Fatalf("chunks = %+v", got)
	}
}

func TestStreamEndingAfterFinishReasonCompletes(t *testing.T) {
	// A missing [DONE] after a finish_reason is tolerated: the response was
	// complete, only the sentinel is missing.
	srv, _ := sseServer(t, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	if len(got) != 2 || got[1].Kind != provider.ChunkDone || got[1].StopReason != "stop" {
		t.Fatalf("chunks = %+v", got)
	}
}

func TestInvalidToolArgumentsFail(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}
	srv, _ := sseServer(t, frames...)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	if len(got) == 0 {
		t.Fatal("no chunks")
	}
	last := got[len(got)-1]
	if last.Kind != provider.ChunkError {
		t.Fatalf("chunks = %+v", got)
	}
	if last.Err == nil || !strings.Contains(last.Err.Error(), "invalid JSON") {
		t.Fatalf("error = %v", last.Err)
	}
}

func TestInStreamErrorObject(t *testing.T) {
	srv, _ := sseServer(t, `{"error":{"message":"overloaded","type":"server_error","code":503}}`)
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	if len(got) != 1 || got[0].Kind != provider.ChunkError {
		t.Fatalf("chunks = %+v", got)
	}
	if msg := got[0].Err.Error(); !strings.Contains(msg, "overloaded") || !strings.Contains(msg, "503") {
		t.Fatalf("error = %v", msg)
	}
}

func TestHTTPErrors(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"message":"rejected `+testKey+`","type":"invalid_request_error","code":"invalid_api_key"}}`)
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
			if herr.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", herr.StatusCode, tc.status)
			}
			if herr.Retryable() != tc.retryable {
				t.Fatalf("Retryable() = %v, want %v", herr.Retryable(), tc.retryable)
			}
			if msg := got[0].Err.Error(); strings.Contains(msg, testKey) {
				t.Fatalf("error leaked the API key: %s", msg)
			}
			if msg := got[0].Err.Error(); strings.Contains(msg, `"error"`) {
				t.Fatalf("error leaked the response body: %s", msg)
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
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "x", ToolCallID: "stray"}},
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
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")
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
			// Anything after cancellation is allowed to be an error chunk, but
			// the stream must then close; drain with a bound.
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
	if p.Name() != "openai" {
		t.Fatalf("Name = %q", p.Name())
	}
	named, err := New(Config{Name: "gateway", BaseURL: "http://example.invalid/v1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if named.Name() != "gateway" {
		t.Fatalf("Name = %q", named.Name())
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
			write("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n")
			write("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			write("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
			write("data: [DONE]\n\n")
			return
		}
		write("data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		write("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL)
	store := chat.NewMemStore(chat.MemStoreOptions{})
	runner, err := chat.NewRunner(chat.RunnerConfig{Store: store, Provider: p, Model: "gpt-test", System: "be helpful"})
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
	if calls != 1 {
		t.Fatalf("tool calls = %d", calls)
	}
	if len(args) != 1 || string(args[0]) != `{"q":"x"}` {
		t.Fatalf("tool args = %+v", args)
	}

	mu.Lock()
	reqs := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d requests", len(reqs))
	}

	var second struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqs[1], &second); err != nil {
		t.Fatalf("decode second request: %v", err)
	}
	if len(second.Messages) != 4 {
		t.Fatalf("second request messages = %+v", second.Messages)
	}
	assistant := second.Messages[2]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 ||
		assistant.ToolCalls[0].ID != "call_1" || assistant.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("assistant history = %+v", assistant)
	}
	tool := second.Messages[3]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || string(tool.Content) != `"{\"answer\":42}"` {
		t.Fatalf("tool history = %+v", tool)
	}
}

func TestContextOverflowIsRecognized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`)
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL)
	ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, ch)
	if len(got) != 1 || !provider.IsContextOverflow(got[0].Err) {
		t.Fatalf("chunks = %+v, want a context overflow", got)
	}

	srv2, _ := sseServer(t, `{"error":{"message":"overloaded","type":"server_error","code":503}}`)
	ch, err = newProvider(t, srv2.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := collect(t, ch); len(got) != 1 || provider.IsContextOverflow(got[0].Err) {
		t.Fatalf("an overloaded stream was classified as an overflow: %+v", got)
	}
}
