package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/sse"
)

// Responses is the OpenAI Responses wire shape used by the ChatGPT/Codex plan
// route. It is also accepted by a few Responses-compatible gateways.
//
// The plan route requires stream=true, store=false, full history in input and
// system content in instructions. It does not accept temperature, top_p or
// max_output_tokens, so those are not sent here.

type responsesRequest struct {
	Model        string `json:"model"`
	Instructions string `json:"instructions,omitempty"`
	Input        []any  `json:"input"`
	Tools        []any  `json:"tools,omitempty"`
	Stream       bool   `json:"stream"`
	Store        bool   `json:"store"`
	// Reasoning carries the effort for reasoning models; nil leaves the
	// model's default.
	Reasoning *responsesReasoning `json:"reasoning,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
}

type responsesInputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesInputImage struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
}

type responsesInputFile struct {
	Type     string `json:"type"`
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

type responsesMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type responsesFunctionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesFunctionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict"`
}

// buildResponsesBody translates a provider request to the Responses shape.
func (p *Provider) buildResponsesBody(req provider.Request) ([]byte, error) {
	input := make([]any, 0, len(req.Messages))
	for i, m := range req.Messages {
		items, err := responsesInputFor(m)
		if err != nil {
			return nil, fmt.Errorf("openai: message %d: %w", i, err)
		}
		input = append(input, items...)
	}

	payload := responsesRequest{
		Model:        req.Model,
		Instructions: req.System,
		Input:        input,
		Stream:       true,
		Store:        false,
	}
	if req.ReasoningEffort != "" {
		payload.Reasoning = &responsesReasoning{Effort: req.ReasoningEffort}
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for i, t := range req.Tools {
			if t.Name == "" {
				return nil, fmt.Errorf("openai: tool %d has no name: %w", i, provider.ErrUnsupported)
			}
			params := t.InputSchema
			if params == nil {
				params = map[string]any{}
			}
			tools = append(tools, responsesTool{
				Type:        "function",
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
				Strict:      false,
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

// responsesInputFor maps one provider message to zero or more Responses input
// items. An assistant message with both text and tool calls becomes a message
// item followed by one function_call item per call; a tool result becomes one
// function_call_output item.
func responsesInputFor(m provider.Message) ([]any, error) {
	switch m.Role {
	case provider.RoleUser:
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" || len(m.ToolResult) > 0 {
			return nil, fmt.Errorf("%w: user message carries tool fields", provider.ErrUnsupported)
		}
		content := make([]any, 0, 1+len(m.Attachments))
		if m.Text != "" || len(m.Attachments) == 0 {
			content = append(content, responsesInputText{Type: "input_text", Text: m.Text})
		}
		for i, a := range m.Attachments {
			kind, ok := provider.AttachmentKind(a.MIME)
			if !ok {
				return nil, fmt.Errorf("%w: attachment %d has unsupported type %q", provider.ErrUnsupported, i, a.MIME)
			}
			switch kind {
			case provider.AttachmentImage:
				content = append(content, responsesInputImage{Type: "input_image", ImageURL: dataURI(a.MIME, a.Data)})
			case provider.AttachmentPDF, provider.AttachmentText:
				content = append(content, responsesInputFile{
					Type:     "input_file",
					Filename: a.Name,
					FileData: dataURI(a.MIME, a.Data),
				})
			default:
				return nil, fmt.Errorf("%w: attachment %d has unrepresentable kind %q", provider.ErrUnsupported, i, kind)
			}
		}
		return []any{responsesMessage{Type: "message", Role: "user", Content: content}}, nil

	case provider.RoleAssistant:
		if m.ToolCallID != "" || len(m.ToolResult) > 0 {
			return nil, fmt.Errorf("%w: assistant message carries a tool result", provider.ErrUnsupported)
		}
		out := make([]any, 0, 1+len(m.ToolCalls))
		if m.Text != "" {
			out = append(out, responsesMessage{
				Type: "message",
				Role: "assistant",
				Content: []any{
					responsesInputText{Type: "output_text", Text: m.Text},
				},
			})
		}
		for i, c := range m.ToolCalls {
			if c.ID == "" || c.Name == "" {
				return nil, fmt.Errorf("%w: tool call %d is missing its id or name", provider.ErrUnsupported, i)
			}
			args, err := toolArguments(c.Arguments)
			if err != nil {
				return nil, fmt.Errorf("tool call %d: %w", i, err)
			}
			out = append(out, responsesFunctionCall{Type: "function_call", CallID: c.ID, Name: c.Name, Arguments: args})
		}
		return out, nil

	case provider.RoleTool:
		result, err := toolResult(m.ToolResult)
		if err != nil {
			return nil, err
		}
		return []any{responsesFunctionOutput{Type: "function_call_output", CallID: m.ToolCallID, Output: result}}, nil

	default:
		return nil, fmt.Errorf("%w: role %q", provider.ErrUnsupported, m.Role)
	}
}

// --- streaming ---

type responsesEvent struct {
	Type     string           `json:"type"`
	Delta    string           `json:"delta"`
	Item     json.RawMessage  `json:"item"`
	Response *responsesObject `json:"response"`
	Error    *responsesError  `json:"error"`
	Message  string           `json:"message"`
	Code     string           `json:"code"`
}

type responsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesObject struct {
	Model             string            `json:"model"`
	Output            []json.RawMessage `json:"output"`
	Usage             *responsesUsage   `json:"usage"`
	Error             *responsesError   `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

type responsesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type responsesItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// consumeResponses parses the Responses SSE stream. Inference counts as
// successful only after response.completed, matching the plan route's contract.
func (p *Provider) consumeResponses(ctx context.Context, body io.Reader, ch chan<- provider.Chunk) {
	// A completed event can carry the full output history, which is larger than
	// the Chat Completions deltas. Bound it generously but still finitely.
	reader := sse.NewReaderSize(body, 16<<20)
	seenCalls := make(map[string]struct{})
	var (
		sawTextDelta bool
		sawTextItem  bool
		sawToolCall  bool
	)

	fail := func(err error) bool {
		sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: err})
		return false
	}

	emitCall := func(item responsesItem) bool {
		if item.CallID == "" || item.Name == "" {
			return fail(fmt.Errorf("openai: function_call item is missing its call id or name"))
		}
		if _, ok := seenCalls[item.CallID]; ok {
			return true
		}
		args := strings.TrimSpace(item.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return fail(fmt.Errorf("openai: tool call %q has invalid JSON arguments: %s", item.Name, truncate(args)))
		}
		call := provider.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(args)}
		if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkToolCall, ToolCall: &call}) {
			return false
		}
		seenCalls[item.CallID] = struct{}{}
		sawToolCall = true
		return true
	}

	emitItem := func(raw json.RawMessage) bool {
		var item responsesItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return fail(fmt.Errorf("openai: decode response item: %w", err))
		}
		switch item.Type {
		case "function_call":
			return emitCall(item)
		case "message":
			if sawTextDelta || sawTextItem {
				return true
			}
			var text strings.Builder
			for _, c := range item.Content {
				if c.Type == "output_text" {
					text.WriteString(c.Text)
				}
			}
			if text.Len() > 0 {
				sawTextItem = true
				return sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkText, Text: text.String()})
			}
		}
		return true
	}

	finish := func(resp *responsesObject) {
		if resp == nil {
			resp = &responsesObject{}
		}
		for _, raw := range resp.Output {
			if !emitItem(raw) {
				return
			}
		}
		if resp.Usage != nil {
			if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkUsage, Usage: &provider.Usage{
				InputTokens:  resp.Usage.InputTokens,
				OutputTokens: resp.Usage.OutputTokens,
			}}) {
				return
			}
		}
		stop := "stop"
		if sawToolCall {
			stop = "tool_calls"
		}
		sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkDone, StopReason: stop})
	}

	for {
		ev, err := reader.Next()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, io.EOF) {
				sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: errors.New(
					"openai: stream ended before response.completed")})
				return
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("openai: read stream: %w", err)})
			return
		}

		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var event responsesEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf("openai: decode stream event: %w", err)})
			return
		}

		switch event.Type {
		case "response.output_text.delta":
			if event.Delta == "" || sawTextItem {
				continue
			}
			sawTextDelta = true
			if !sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkText, Text: event.Delta}) {
				return
			}
		case "response.output_item.done":
			if len(event.Item) > 0 && !emitItem(event.Item) {
				return
			}
		case "response.completed":
			finish(event.Response)
			return
		case "response.failed":
			message, code := "response failed", ""
			if event.Response != nil && event.Response.Error != nil {
				message, code = event.Response.Error.Message, event.Response.Error.Code
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf(
				"openai: %s", truncate(strings.TrimSpace(code+" "+message)))})
			return
		case "response.incomplete":
			reason := "unknown"
			if event.Response != nil && event.Response.IncompleteDetails != nil {
				reason = event.Response.IncompleteDetails.Reason
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf(
				"openai: response incomplete: %s", truncate(reason))})
			return
		case "error":
			message := event.Message
			if event.Error != nil {
				message = strings.TrimSpace(event.Error.Code + " " + event.Error.Message)
			}
			sendChunk(ctx, ch, provider.Chunk{Kind: provider.ChunkError, Err: fmt.Errorf(
				"openai: stream error: %s", truncate(message))})
			return
		}
	}
}
