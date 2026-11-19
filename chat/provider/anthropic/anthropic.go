// Package anthropic implements the Anthropic Messages streaming protocol as a
// chat provider.
//
// One Stream call is one HTTP POST to /v1/messages with stream:true, decoded
// from Server-Sent Events. Text arrives as text deltas and tool calls arrive as
// partly accumulated JSON strings, which the adapter validates before handing
// the runner a complete call.
package anthropic

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
	"strings"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/internal/httperr"
	"github.com/compdani/pocketbase-mcp/chat/provider/sse"
)

const (
	defaultBaseURL   = "https://api.anthropic.com"
	defaultVersion   = "2023-06-01"
	defaultMaxTokens = 4096
	defaultName      = "anthropic"

	// MaxRequestBodyBytes bounds one encoded request, as in the OpenAI adapter.
	// It leaves room for the runner's 20 MiB attachment budget after base64
	// expansion and the JSON envelope.
	MaxRequestBodyBytes = 32 << 20

	maxErrorBodyBytes = 4 << 10
)

// HTTPError is the typed error reported in an error chunk for a non-2xx
// response.
type HTTPError = httperr.Error

// Config configures a Provider.
type Config struct {
	// BaseURL is the API root, for example https://api.anthropic.com. A
	// trailing slash is allowed. Empty means the Anthropic default.
	BaseURL string
	// APIKey is sent as x-api-key. Empty omits the header, which lets a proxy
	// supply its own credential through Headers.
	APIKey string
	// HTTPClient is used for every request. Nil means http.DefaultClient.
	HTTPClient *http.Client
	// Version is the anthropic-version header. Empty means "2023-06-01".
	Version string
	// DefaultMaxTokens is sent when a Request leaves MaxOutputTokens zero. The
	// API requires max_tokens. Zero means 4096.
	DefaultMaxTokens int
	// Headers are extra request headers, applied before the protocol headers.
	// They may override the protocol headers, including x-api-key.
	Headers map[string]string
	// Name identifies the provider. Empty means "anthropic".
	Name string
}

// Provider is an Anthropic streaming provider. It is safe for concurrent
// Stream calls.
type Provider struct {
	baseURL   string
	apiKey    string
	client    *http.Client
	version   string
	maxTokens int
	headers   map[string]string
	name      string
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
		return nil, fmt.Errorf("anthropic: invalid base URL %q", cfg.BaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("anthropic: unsupported base URL scheme %q", u.Scheme)
	}

	name := cfg.Name
	if name == "" {
		name = defaultName
	}
	version := cfg.Version
	if version == "" {
		version = defaultVersion
	}
	maxTokens := cfg.DefaultMaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	return &Provider{
		baseURL:   base,
		apiKey:    cfg.APIKey,
		client:    client,
		version:   version,
		maxTokens: maxTokens,
		headers:   headers,
		name:      name,
	}, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return p.name }

// SupportsAttachment implements provider.AttachmentSupport: text and PDF travel
// as document blocks and images as base64 image blocks.
func (p *Provider) SupportsAttachment(mimeType string) bool {
	return provider.SupportsAttachment(mimeType)
}

// endpoint is the messages URL.
func (p *Provider) endpoint() string { return p.baseURL + "/v1/messages" }

// Stream implements provider.Provider. Request-construction problems are
// returned as an error; transport and protocol failures arrive as an error
// chunk.
func (p *Provider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body, err := p.buildBody(req)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxRequestBodyBytes {
		return nil, fmt.Errorf("anthropic: request body is %d bytes, over the %d byte limit: %w",
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

type messagesRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
}

type wireMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// contentBlock is the union of the block shapes this adapter emits. Fields
// that do not belong to a block's type stay at their zero value and are
// omitted.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// Source and Title belong to image and document blocks, the shapes an
	// attachment becomes.
	Source *blockSource `json:"source,omitempty"`
	Title  string       `json:"title,omitempty"`
}

// blockSource is the base64 or plain-text payload of an image/document block.
type blockSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

type wireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// buildBody encodes req into the wire request.
func (p *Provider) buildBody(req provider.Request) ([]byte, error) {
	if req.ReasoningEffort != "" {
		// Extended thinking is not wired up (it needs thinking blocks kept
		// across tool calls), so an effort is refused rather than ignored.
		return nil, fmt.Errorf("anthropic: reasoning effort is not supported: %w", provider.ErrUnsupported)
	}
	messages, err := wireMessages(req.Messages)
	if err != nil {
		return nil, err
	}

	maxTokens := req.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = p.maxTokens
	}
	payload := messagesRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  messages,
		Stream:    true,
	}
	if len(req.Tools) > 0 {
		tools := make([]wireTool, 0, len(req.Tools))
		for i, t := range req.Tools {
			if t.Name == "" {
				return nil, fmt.Errorf("anthropic: tool %d has no name: %w", i, provider.ErrUnsupported)
			}
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{}
			}
			tools = append(tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: schema})
		}
		payload.Tools = tools
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode request: %w", err)
	}
	return body, nil
}

// wireMessages converts the port's one-message-per-entry history into the
// protocol's block form. Roles must alternate, so consecutive messages of the
// same role are merged; in particular every tool result answering one
// assistant turn lands in a single user message.
func wireMessages(msgs []provider.Message) ([]wireMessage, error) {
	out := make([]wireMessage, 0, len(msgs))
	for i, m := range msgs {
		role, blocks, err := blocksFor(m)
		if err != nil {
			return nil, fmt.Errorf("anthropic: message %d: %w", i, err)
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("anthropic: message %d has no representable content: %w", i, provider.ErrUnsupported)
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, wireMessage{Role: role, Content: blocks})
	}
	// Tool results must precede the rest of a user turn's content.
	for i := range out {
		if out[i].Role == "user" {
			out[i].Content = toolResultsFirst(out[i].Content)
		}
	}
	return out, nil
}

func blocksFor(m provider.Message) (string, []contentBlock, error) {
	switch m.Role {
	case provider.RoleUser:
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" || len(m.ToolResult) > 0 {
			return "", nil, fmt.Errorf("%w: user message carries tool fields", provider.ErrUnsupported)
		}
		var blocks []contentBlock
		if m.Text != "" {
			blocks = append(blocks, contentBlock{Type: "text", Text: m.Text})
		}
		for i, a := range m.Attachments {
			block, err := attachmentBlock(a)
			if err != nil {
				return "", nil, fmt.Errorf("attachment %d: %w", i, err)
			}
			blocks = append(blocks, block)
		}
		if len(blocks) == 0 {
			return "user", nil, nil
		}
		return "user", blocks, nil

	case provider.RoleAssistant:
		if m.ToolCallID != "" || len(m.ToolResult) > 0 {
			return "", nil, fmt.Errorf("%w: assistant message carries a tool result", provider.ErrUnsupported)
		}
		var blocks []contentBlock
		if m.Text != "" {
			blocks = append(blocks, contentBlock{Type: "text", Text: m.Text})
		}
		for i, c := range m.ToolCalls {
			if c.ID == "" || c.Name == "" {
				return "", nil, fmt.Errorf("%w: tool call %d is missing its id or name", provider.ErrUnsupported, i)
			}
			input, err := toolInput(c.Arguments)
			if err != nil {
				return "", nil, fmt.Errorf("tool call %d: %w", i, err)
			}
			blocks = append(blocks, contentBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: input})
		}
		return "assistant", blocks, nil

	case provider.RoleTool:
		if m.ToolCallID == "" {
			return "", nil, fmt.Errorf("%w: tool result without call id", provider.ErrUnsupported)
		}
		content, err := toolResultContent(m.ToolResult)
		if err != nil {
			return "", nil, err
		}
		return "user", []contentBlock{{
			Type:      "tool_result",
			ToolUseID: m.ToolCallID,
			Content:   content,
			IsError:   m.ToolIsError,
		}}, nil

	default:
		return "", nil, fmt.Errorf("%w: role %q", provider.ErrUnsupported, m.Role)
	}
}

// attachmentBlock renders one attachment as the block the Messages protocol
// accepts for it. Text becomes a plain-text document so its bytes travel once,
// images and PDFs become base64 sources. Nothing is fetched or linked.
func attachmentBlock(a provider.Attachment) (contentBlock, error) {
	kind, ok := provider.AttachmentKind(a.MIME)
	if !ok {
		return contentBlock{}, fmt.Errorf("%w: unsupported type %q", provider.ErrUnsupported, a.MIME)
	}
	switch kind {
	case provider.AttachmentText:
		// A text source only accepts text/plain; the frame still names the
		// real type, so a Markdown or JSON file keeps its meaning.
		return contentBlock{
			Type:  "document",
			Title: a.Name,
			Source: &blockSource{
				Type:      "text",
				MediaType: "text/plain",
				Data:      provider.FrameTextAttachment(a.Name, a.MIME, a.Data),
			},
		}, nil
	case provider.AttachmentImage:
		return contentBlock{
			Type: "image",
			Source: &blockSource{
				Type:      "base64",
				MediaType: a.MIME,
				Data:      base64.StdEncoding.EncodeToString(a.Data),
			},
		}, nil
	case provider.AttachmentPDF:
		return contentBlock{
			Type:  "document",
			Title: a.Name,
			Source: &blockSource{
				Type:      "base64",
				MediaType: "application/pdf",
				Data:      base64.StdEncoding.EncodeToString(a.Data),
			},
		}, nil
	default:
		return contentBlock{}, fmt.Errorf("%w: unrepresentable kind %q", provider.ErrUnsupported, kind)
	}
}

// toolInput renders call arguments as the JSON object the protocol wants.
func toolInput(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("%w: tool arguments are not valid JSON", provider.ErrUnsupported)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("%w: tool arguments are not a JSON object", provider.ErrUnsupported)
	}
	return json.RawMessage(append([]byte(nil), trimmed...)), nil
}

// toolResultContent renders a tool result as the JSON text of the block.
func toolResultContent(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "null", nil
	}
	if !json.Valid(trimmed) {
		return "", fmt.Errorf("%w: tool result is not valid JSON", provider.ErrUnsupported)
	}
	return string(trimmed), nil
}

// toolResultsFirst moves tool_result blocks to the front of a user message,
// preserving the order within each group.
func toolResultsFirst(blocks []contentBlock) []contentBlock {
	first := true
	for _, b := range blocks {
		if b.Type != "tool_result" {
			first = false
			break
		}
	}
	if first || len(blocks) < 2 {
		return blocks
	}
	out := make([]contentBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "tool_result" {
			out = append(out, b)
		}
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			out = append(out, b)
		}
	}
	return out
}

// --- streaming ---

func (p *Provider) stream(ctx context.Context, body []byte, ch chan<- provider.Chunk) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("anthropic: build request: %w", err)})
		return
	}
	for k, v := range p.headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" && httpReq.Header.Get("x-api-key") == "" {
		httpReq.Header.Set("x-api-key", p.apiKey)
	}
	if httpReq.Header.Get("anthropic-version") == "" {
		httpReq.Header.Set("anthropic-version", p.version)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("anthropic: request failed: %w", err)})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: httperr.New(p.name, resp.StatusCode, detail, p.apiKey)})
		return
	}

	p.consume(ctx, resp.Body, ch)
}

// blockState tracks one open content block of the response.
type blockState struct {
	kind    string
	id      string
	name    string
	input   strings.Builder
	emitted bool
}

func (p *Provider) consume(ctx context.Context, body io.Reader, ch chan<- provider.Chunk) {
	reader := sse.NewReader(body)
	blocks := make(map[int]*blockState)
	var (
		usage      provider.Usage
		sawUsage   bool
		stopReason string
	)

	for {
		ev, err := reader.Next()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, io.EOF) {
				sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: errors.New(
					"anthropic: stream ended before message_stop")})
				return
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("anthropic: read stream: %w", err)})
			return
		}

		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("anthropic: decode stream event: %w", err)})
			return
		}

		eventType := event.Type
		if eventType == "" {
			// Some proxies populate only the SSE event name; use it when the
			// payload does not repeat the type.
			eventType = ev.Event
		}

		switch eventType {
		case "message_start":
			if event.Message != nil && event.Message.Usage != nil {
				usage.InputTokens = event.Message.Usage.InputTokens
				usage.OutputTokens = event.Message.Usage.OutputTokens
				sawUsage = true
			}

		case "content_block_start":
			if event.ContentBlock == nil {
				continue
			}
			state := &blockState{
				kind: event.ContentBlock.Type,
				id:   event.ContentBlock.ID,
				name: event.ContentBlock.Name,
			}
			blocks[event.Index] = state
			if state.kind == "text" && event.ContentBlock.Text != "" {
				if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkText, Text: event.ContentBlock.Text}) {
					return
				}
			}

		case "content_block_delta":
			state := blocks[event.Index]
			if state == nil || event.Delta == nil {
				continue
			}
			switch state.kind {
			case "text":
				if event.Delta.Type != "text_delta" || event.Delta.Text == "" {
					continue
				}
				if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkText, Text: event.Delta.Text}) {
					return
				}
			case "tool_use":
				if event.Delta.Type != "input_json_delta" {
					continue
				}
				state.input.WriteString(event.Delta.PartialJSON)
			}

		case "content_block_stop":
			state := blocks[event.Index]
			if state == nil || state.kind != "tool_use" || state.emitted {
				continue
			}
			state.emitted = true
			args := strings.TrimSpace(state.input.String())
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf(
					"anthropic: tool call %q has invalid JSON input: %s", state.name, truncate(args))})
				return
			}
			call := provider.ToolCall{ID: state.id, Name: state.name, Arguments: json.RawMessage(args)}
			if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkToolCall, ToolCall: &call}) {
				return
			}

		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				stopReason = event.Delta.StopReason
			}
			if event.Usage != nil {
				usage.OutputTokens = event.Usage.OutputTokens
				if event.Usage.InputTokens != 0 {
					usage.InputTokens = event.Usage.InputTokens
				}
				sawUsage = true
			}

		case "message_stop":
			if sawUsage {
				u := usage
				if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkUsage, Usage: &u}) {
					return
				}
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkDone, StopReason: stopReason})
			return

		case "ping":
			// Keep-alive; nothing to report.

		case "error":
			msg := ""
			typ := ""
			if event.Error != nil {
				msg, typ = event.Error.Message, event.Error.Type
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf(
				"anthropic: stream error %s: %s", typ, msg)})
			return

		default:
			// Unknown event types are ignored so a newer protocol version does
			// not break the adapter. Unknown content block types are handled the
			// same way: their start is recorded and their deltas are dropped.
		}
	}
}

// streamEvent is one decoded event payload.
type streamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message *struct {
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`

	ContentBlock *struct {
		Type string `json:"type"`
		Text string `json:"text"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`

	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
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
