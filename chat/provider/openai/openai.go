// Package openai implements the OpenAI-compatible streaming Chat Completions
// protocol as a chat provider.
//
// One Stream call is one HTTP POST with stream:true and
// stream_options.include_usage, decoded as Server-Sent Events. Tool calls
// arrive as fragments keyed by index; the adapter reassembles each one and
// hands the runner complete calls with validated JSON arguments.
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/internal/httperr"
	"github.com/compdani/pocketbase-mcp/chat/provider/sse"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	defaultName    = "openai"

	// MaxRequestBodyBytes bounds one encoded request. A larger request is
	// refused instead of attempted, because a model API will reject it too and
	// the adapter must not hold unbounded memory. It leaves room for the
	// runner's 20 MiB attachment budget after base64 expansion (about 4/3 of
	// the raw bytes) and the JSON envelope.
	MaxRequestBodyBytes = 32 << 20

	// maxErrorBodyBytes bounds how much of a failed response is read.
	maxErrorBodyBytes = 4 << 10
)

// HTTPError is the typed error reported in an error chunk for a non-2xx
// response.
type HTTPError = httperr.Error

// WireAPI values for Config.WireAPI.
const (
	// WireChatCompletions is the OpenAI-compatible Chat Completions shape.
	WireChatCompletions = "chat"
	// WireResponses is the OpenAI Responses shape used by the Codex plan route
	// and by Responses-compatible gateways.
	WireResponses = "responses"
)

// TokenSource supplies a bearer token for one request. forceRefresh asks the
// source to renew even if its cached token has not reached its refresh window;
// the provider uses it once after a 401.
type TokenSource interface {
	Token(ctx context.Context, forceRefresh bool) (string, error)
}

// Config configures a Provider. Only APIKey and BaseURL usually matter; the
// rest exist for compatible gateways and tests.
type Config struct {
	// BaseURL is the API root, for example https://api.openai.com/v1. A
	// trailing slash is allowed. Empty means the OpenAI default.
	BaseURL string
	// APIKey is sent as a bearer token. Empty omits the Authorization header,
	// which local gateways sometimes need; an extra header can supply another
	// scheme. TokenSource, when set, overrides it per request.
	APIKey string
	// TokenSource, when set, supplies a fresh bearer token for each request.
	// It is used by the Codex plan provider and by short-lived gateway tokens.
	TokenSource TokenSource
	// HTTPClient is used for every request. Nil means http.DefaultClient.
	HTTPClient *http.Client
	// Headers are extra request headers, applied before the protocol headers.
	// They may override the protocol headers, including Authorization.
	Headers map[string]string
	// Name identifies the provider. Empty means "openai".
	Name string
	// WireAPI selects the wire shape. Empty means WireChatCompletions.
	WireAPI string
	// ExtraBody is merged into the encoded request body last. It is for
	// non-secret gateway-specific fields.
	ExtraBody map[string]any
}

// Provider is an OpenAI-compatible streaming chat provider. It is safe for
// concurrent Stream calls.
type Provider struct {
	baseURL     string
	apiKey      string
	tokenSource TokenSource
	client      *http.Client
	headers     map[string]string
	name        string
	wire        string
	extraBody   map[string]any
}

var _ provider.Provider = (*Provider)(nil)

// New validates cfg and returns a provider.
func New(cfg Config) (*Provider, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = defaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("openai: invalid base URL %q", cfg.BaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("openai: unsupported base URL scheme %q", u.Scheme)
	}

	name := cfg.Name
	if name == "" {
		name = defaultName
	}
	wire := strings.TrimSpace(cfg.WireAPI)
	if wire == "" {
		wire = WireChatCompletions
	}
	switch wire {
	case WireChatCompletions, WireResponses:
	default:
		return nil, fmt.Errorf("openai: unsupported wire API %q", cfg.WireAPI)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	extra := make(map[string]any, len(cfg.ExtraBody))
	for k, v := range cfg.ExtraBody {
		extra[k] = v
	}
	return &Provider{
		baseURL:     base,
		apiKey:      cfg.APIKey,
		tokenSource: cfg.TokenSource,
		client:      client,
		headers:     headers,
		name:        name,
		wire:        wire,
		extraBody:   extra,
	}, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return p.name }

// SupportsAttachment implements provider.AttachmentSupport. This protocol can
// carry every canonical attachment type: text and PDF as data parts, images as
// image_url data URIs.
func (p *Provider) SupportsAttachment(mimeType string) bool {
	return provider.SupportsAttachment(mimeType)
}

// endpoint is the wire endpoint for this provider.
func (p *Provider) endpoint() string {
	if p.wire == WireResponses {
		return p.baseURL + "/responses"
	}
	return p.baseURL + "/chat/completions"
}

// Stream implements provider.Provider. Request-construction problems are
// returned as an error; transport and protocol failures arrive as an error
// chunk.
func (p *Provider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var body []byte
	var err error
	if p.wire == WireResponses {
		body, err = p.buildResponsesBody(req)
	} else {
		body, err = p.buildBody(req)
	}
	if err != nil {
		return nil, err
	}
	if len(body) > MaxRequestBodyBytes {
		return nil, fmt.Errorf("openai: request body is %d bytes, over the %d byte limit: %w",
			len(body), MaxRequestBodyBytes, provider.ErrUnsupported)
	}

	ch := make(chan provider.Chunk)
	go func() {
		defer close(ch)
		p.stream(ctx, body, ch)
	}()
	return ch, nil
}

// --- request body ---

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	// ReasoningEffort is Chat Completions' reasoning_effort for reasoning
	// models (gpt-5, o-series).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireContentPart is one entry of an array-form content field. It is only used
// when a message carries attachments: a message without them keeps the plain
// string form it always had.
type wireContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
	File     *wireFile     `json:"file,omitempty"`
}

type wireImageURL struct {
	URL string `json:"url"`
}

type wireFile struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Arguments   string         `json:"arguments,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

// buildBody encodes req into the wire request.
func (p *Provider) buildBody(req provider.Request) ([]byte, error) {
	msgs := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: req.System})
	}
	for i, m := range req.Messages {
		wm, err := wireMessageFor(m)
		if err != nil {
			return nil, fmt.Errorf("openai: message %d: %w", i, err)
		}
		msgs = append(msgs, wm)
	}

	payload := chatRequest{
		Model:           req.Model,
		Messages:        msgs,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
		ReasoningEffort: req.ReasoningEffort,
	}
	if req.MaxOutputTokens > 0 {
		payload.MaxTokens = req.MaxOutputTokens
	}
	if len(req.Tools) > 0 {
		tools := make([]wireTool, 0, len(req.Tools))
		for i, t := range req.Tools {
			if t.Name == "" {
				return nil, fmt.Errorf("openai: tool %d has no name: %w", i, provider.ErrUnsupported)
			}
			params := t.InputSchema
			if params == nil {
				params = map[string]any{}
			}
			tools = append(tools, wireTool{
				Type: "function",
				Function: wireFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			})
		}
		payload.Tools = tools
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	return p.mergeExtraBody(body)
}

// mergeExtraBody applies Config.ExtraBody to an already encoded request. It is
// applied last, so a gateway-specific field can override the adapter's default
// without the adapter having to know the gateway.
func (p *Provider) mergeExtraBody(body []byte) ([]byte, error) {
	if len(p.extraBody) == 0 {
		return body, nil
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	for k, v := range p.extraBody {
		fields[k] = v
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	return merged, nil
}

func wireMessageFor(m provider.Message) (wireMessage, error) {
	switch m.Role {
	case provider.RoleUser:
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" || len(m.ToolResult) > 0 {
			return wireMessage{}, fmt.Errorf("%w: user message carries tool fields", provider.ErrUnsupported)
		}
		if len(m.Attachments) > 0 {
			content, err := userContentParts(m)
			if err != nil {
				return wireMessage{}, err
			}
			return wireMessage{Role: "user", Content: content}, nil
		}
		return wireMessage{Role: "user", Content: m.Text}, nil

	case provider.RoleAssistant:
		if m.ToolCallID != "" || len(m.ToolResult) > 0 {
			return wireMessage{}, fmt.Errorf("%w: assistant message carries a tool result", provider.ErrUnsupported)
		}
		wm := wireMessage{Role: "assistant"}
		switch {
		case m.Text != "":
			wm.Content = m.Text
		case len(m.ToolCalls) == 0:
			wm.Content = ""
		default:
			// A tool-call-only assistant turn has no text: content must be
			// explicitly null, not omitted.
			wm.Content = nil
		}
		for i, c := range m.ToolCalls {
			if c.ID == "" || c.Name == "" {
				return wireMessage{}, fmt.Errorf("%w: tool call %d is missing its id or name", provider.ErrUnsupported, i)
			}
			args, err := toolArguments(c.Arguments)
			if err != nil {
				return wireMessage{}, fmt.Errorf("tool call %d: %w", i, err)
			}
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:       c.ID,
				Type:     "function",
				Function: wireFunction{Name: c.Name, Arguments: args},
			})
		}
		return wm, nil

	case provider.RoleTool:
		result, err := toolResult(m.ToolResult)
		if err != nil {
			return wireMessage{}, err
		}
		return wireMessage{Role: "tool", ToolCallID: m.ToolCallID, Content: result}, nil

	default:
		return wireMessage{}, fmt.Errorf("%w: role %q", provider.ErrUnsupported, m.Role)
	}
}

// userContentParts renders the array-of-parts form of a user message that
// carries attachments. Every attachment becomes a data part: nothing is fetched
// from a URL, and a text file is framed so the model treats its content as data
// rather than as new instructions.
func userContentParts(m provider.Message) ([]wireContentPart, error) {
	parts := make([]wireContentPart, 0, 1+len(m.Attachments))
	if m.Text != "" {
		parts = append(parts, wireContentPart{Type: "text", Text: m.Text})
	}
	for i, a := range m.Attachments {
		kind, ok := provider.AttachmentKind(a.MIME)
		if !ok {
			return nil, fmt.Errorf("%w: attachment %d has unsupported type %q", provider.ErrUnsupported, i, a.MIME)
		}
		switch kind {
		case provider.AttachmentText:
			parts = append(parts, wireContentPart{
				Type: "text",
				Text: provider.FrameTextAttachment(a.Name, a.MIME, a.Data),
			})
		case provider.AttachmentImage:
			parts = append(parts, wireContentPart{
				Type:     "image_url",
				ImageURL: &wireImageURL{URL: dataURI(a.MIME, a.Data)},
			})
		case provider.AttachmentPDF:
			parts = append(parts, wireContentPart{
				Type: "file",
				File: &wireFile{
					Filename: a.Name,
					FileData: dataURI("application/pdf", a.Data),
				},
			})
		default:
			return nil, fmt.Errorf("%w: attachment %d has unrepresentable kind %q", provider.ErrUnsupported, i, kind)
		}
	}
	return parts, nil
}

// dataURI encodes one attachment as a base64 data URI, the only way this
// protocol accepts inline file content.
func dataURI(mimeType string, data []byte) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func toolArguments(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "{}", nil
	}
	if !json.Valid(trimmed) {
		return "", fmt.Errorf("%w: tool arguments are not valid JSON", provider.ErrUnsupported)
	}
	return string(trimmed), nil
}

// toolResult renders a tool result as the JSON string the protocol wants. A
// missing result becomes JSON null rather than an empty message.
func toolResult(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "null", nil
	}
	if !json.Valid(trimmed) {
		return "", fmt.Errorf("%w: tool result is not valid JSON", provider.ErrUnsupported)
	}
	return string(trimmed), nil
}

// --- streaming ---

func (p *Provider) stream(ctx context.Context, body []byte, ch chan<- provider.Chunk) {
	for attempt := 0; ; attempt++ {
		token, err := p.requestToken(ctx, attempt > 0)
		if err != nil {
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("openai: token: %w", err)})
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
		if err != nil {
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("openai: build request: %w", err)})
			return
		}
		for k, v := range p.headers {
			httpReq.Header.Set(k, v)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if token != "" && httpReq.Header.Get("Authorization") == "" {
			httpReq.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := p.client.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("openai: request failed: %w", err)})
			return
		}

		if resp.StatusCode == http.StatusUnauthorized && p.tokenSource != nil && attempt == 0 {
			// A stale access token is expected for the plan route. Force one
			// refresh and retry the same body; anything else is a real error.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
			resp.Body.Close()
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
			resp.Body.Close()
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: httperr.New(p.name, resp.StatusCode, detail, token)})
			return
		}

		defer resp.Body.Close()
		if p.wire == WireResponses {
			p.consumeResponses(ctx, resp.Body, ch)
		} else {
			p.consume(ctx, resp.Body, ch)
		}
		return
	}
}

// requestToken returns the bearer token for one attempt. A nil source uses the
// static API key.
func (p *Provider) requestToken(ctx context.Context, forceRefresh bool) (string, error) {
	if p.tokenSource == nil {
		return p.apiKey, nil
	}
	return p.tokenSource.Token(ctx, forceRefresh)
}

// toolAccum is one tool call being reassembled from fragments.
type toolAccum struct {
	id   string
	name string
	args strings.Builder
}

func (p *Provider) consume(ctx context.Context, body io.Reader, ch chan<- provider.Chunk) {
	reader := sse.NewReader(body)
	acc := make(map[int]*toolAccum)
	var (
		stopReason string
		sawFinish  bool
		flushed    bool
	)

	// flushTools emits the accumulated calls in index order. It returns false
	// when the stream must stop.
	flushTools := func() bool {
		if flushed {
			return true
		}
		flushed = true
		indices := make([]int, 0, len(acc))
		for i := range acc {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, i := range indices {
			a := acc[i]
			args := strings.TrimSpace(a.args.String())
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf(
					"openai: tool call %q has invalid JSON arguments: %s", a.name, truncate(args))})
				return false
			}
			call := provider.ToolCall{ID: a.id, Name: a.name, Arguments: json.RawMessage(args)}
			if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkToolCall, ToolCall: &call}) {
				return false
			}
		}
		return true
	}

	for {
		ev, err := reader.Next()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, io.EOF) {
				if !sawFinish && !flushed {
					sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: errors.New(
						"openai: stream ended before [DONE] or a finish_reason")})
					return
				}
				if !flushTools() {
					return
				}
				sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkDone, StopReason: stopReason})
				return
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("openai: read stream: %w", err)})
			return
		}

		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			if !flushTools() {
				return
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkDone, StopReason: stopReason})
			return
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("openai: decode stream chunk: %w", err)})
			return
		}
		if chunk.Error != nil {
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: responsesFailure(
				fmt.Sprintf("openai: stream error %s/%s: %s", chunk.Error.Type, chunk.Error.code(), chunk.Error.Message),
				chunk.Error.code(), chunk.Error.Message)})
			return
		}
		if chunk.Usage != nil {
			usage := &provider.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
			if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkUsage, Usage: usage}) {
				return
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.Delta != nil {
			if choice.Delta.Content != "" {
				if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkText, Text: choice.Delta.Content}) {
					return
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				a := acc[tc.Index]
				if a == nil {
					a = &toolAccum{}
					acc[tc.Index] = a
				}
				if tc.ID != "" {
					a.id = tc.ID
				}
				if tc.Function.Name != "" {
					a.name += tc.Function.Name
				}
				a.args.WriteString(tc.Function.Arguments)
			}
		}
		if choice.FinishReason != nil {
			sawFinish = true
			stopReason = *choice.FinishReason
			if !flushTools() {
				return
			}
		}
	}
}

// streamChunk is one decoded "data:" payload.
type streamChunk struct {
	Choices []struct {
		Delta *struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error *streamError `json:"error"`
}

// streamError is the in-stream error object some gateways send instead of
// failing the HTTP response.
type streamError struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

// code renders the error code, which may be a string, a number or absent.
func (e *streamError) code() string {
	raw := bytes.TrimSpace(e.Code)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// sendChunk delivers c unless ctx is done, and reports whether it did.
func sendChunk(ctx context.Context, ch chan<- provider.Chunk, c provider.Chunk) bool {
	select {
	case ch <- c:
		return true
	case <-ctx.Done():
		return false
	}
}

// truncate bounds a value quoted in an error message.
func truncate(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
