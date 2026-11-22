// Package provider defines the model-provider port for the chat runtime.
//
// The port is deliberately small and provider-neutral: a provider translates
// one Request into a stream of Chunks. It knows nothing about turns, stores or
// events, and it must not import the chat package, because chat imports this
// one. A provider is the only place that knows a vendor's wire format.
//
// A Stream call always closes the returned channel: after exactly one done or
// error chunk, and promptly when ctx is cancelled. Consumers therefore never
// need a separate shutdown path, and cancellation reaches the network read.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Role is the author of one message in a request history.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Valid reports whether r names a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleUser, RoleAssistant, RoleTool:
		return true
	}
	return false
}

// Message is one history entry in a request. A user or assistant message
// carries text and, for an assistant message, the tool calls it requested. A
// tool message carries exactly one tool result, addressed by ToolCallID, and
// ToolIsError marks a result that reports a failure rather than data.
//
// Attachments belong to user messages only. A message without attachments must
// serialize exactly as it did before they existed, so the field is omitted
// rather than emitted empty.
type Message struct {
	Role        Role            `json:"role"`
	Text        string          `json:"text,omitempty"`
	Attachments []Attachment    `json:"attachments,omitempty"`
	ToolCalls   []ToolCall      `json:"toolCalls,omitempty"`
	ToolCallID  string          `json:"toolCallId,omitempty"`
	ToolName    string          `json:"toolName,omitempty"`
	ToolResult  json.RawMessage `json:"toolResult,omitempty"`
	ToolIsError bool            `json:"toolIsError,omitempty"`
}

// ToolDef is one tool the model may call, described by JSON Schema.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// ToolCall is one invocation the model requested. Arguments is opaque JSON as
// produced by the model; it is validated by the executor, never here.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Request is one model turn: the conversation so far, the system prompt, the
// tools available and an output bound. MaxOutputTokens zero means the
// provider's default.
type Request struct {
	Model           string    `json:"model"`
	System          string    `json:"system,omitempty"`
	Messages        []Message `json:"messages"`
	Tools           []ToolDef `json:"tools,omitempty"`
	MaxOutputTokens int       `json:"maxOutputTokens,omitempty"`
	// ReasoningEffort asks a reasoning model to think less or more. Empty
	// means the model's default. Only providers that advertise efforts for a
	// model accept it; others refuse the request rather than drop it.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// ParallelToolCalls, when set, allows (true) or forbids (false) several
	// tool calls in one model step. Nil leaves the provider's default.
	// Adapters send it only when the request offers tools.
	ParallelToolCalls *bool `json:"parallelToolCalls,omitempty"`
}

// ReasoningEfforts are the effort levels a request may name, lowest first.
// Which ones a model accepts is per model (catalog Model.Efforts).
var ReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// ValidReasoningEffort reports whether effort is one of ReasoningEfforts.
func ValidReasoningEffort(effort string) bool {
	for _, e := range ReasoningEfforts {
		if e == effort {
			return true
		}
	}
	return false
}

// Usage is the token accounting a provider may report.
type Usage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

// ChunkKind is the type of one streamed chunk.
type ChunkKind string

const (
	// ChunkText carries incremental assistant text.
	ChunkText ChunkKind = "text"
	// ChunkToolCall carries one complete tool invocation.
	ChunkToolCall ChunkKind = "tool_call"
	// ChunkUsage carries token accounting.
	ChunkUsage ChunkKind = "usage"
	// ChunkDone ends a successful stream. StopReason is advisory.
	ChunkDone ChunkKind = "done"
	// ChunkError ends a failed stream. Err is for the server, not the model or
	// the browser: the runner replaces it with a generic failure.
	ChunkError ChunkKind = "error"
)

// Valid reports whether k names a known chunk kind.
func (k ChunkKind) Valid() bool {
	switch k {
	case ChunkText, ChunkToolCall, ChunkUsage, ChunkDone, ChunkError:
		return true
	}
	return false
}

// Chunk is one streamed piece of a model response. Only the fields named by
// Kind are meaningful.
type Chunk struct {
	Kind       ChunkKind `json:"kind"`
	Text       string    `json:"text,omitempty"`
	ToolCall   *ToolCall `json:"toolCall,omitempty"`
	Usage      *Usage    `json:"usage,omitempty"`
	StopReason string    `json:"stopReason,omitempty"`
	Err        error     `json:"-"`
}

// Provider is one model backend.
//
// Stream must close the returned channel after a done or error chunk and
// promptly once ctx is done, without leaking a goroutine. A provider that
// cannot serve a request (unsupported model, missing credential) returns an
// error instead of a channel; the runner reports that generically.
type Provider interface {
	// Name identifies the provider in configuration and diagnostics.
	Name() string
	Stream(ctx context.Context, req Request) (<-chan Chunk, error)
}

// ErrUnsupported reports a feature the provider cannot serve. Providers must
// return it (wrapped is fine) rather than silently dropping the request, so a
// caller can fail explicitly.
var ErrUnsupported = errors.New("provider: unsupported")

// Validate checks the parts of a request every provider relies on. Providers
// may impose stricter rules of their own.
func (r Request) Validate() error {
	if r.Model == "" {
		return fmt.Errorf("provider: request without model")
	}
	if r.ReasoningEffort != "" && !ValidReasoningEffort(r.ReasoningEffort) {
		return fmt.Errorf("provider: unknown reasoning effort %q: %w", r.ReasoningEffort, ErrUnsupported)
	}
	for i, m := range r.Messages {
		if !m.Role.Valid() {
			return fmt.Errorf("provider: message %d has invalid role %q", i, m.Role)
		}
		if m.Role == RoleTool && m.ToolCallID == "" {
			return fmt.Errorf("provider: message %d is a tool result without call id", i)
		}
		if len(m.Attachments) > 0 {
			if m.Role != RoleUser {
				return fmt.Errorf("provider: message %d carries attachments outside a user message: %w", i, ErrUnsupported)
			}
			for j, a := range m.Attachments {
				if a.MIME == "" {
					return fmt.Errorf("provider: message %d attachment %d has no MIME type: %w", i, j, ErrUnsupported)
				}
				if !SupportsAttachment(a.MIME) {
					return fmt.Errorf("provider: message %d attachment %d has unsupported type %q: %w", i, j, a.MIME, ErrUnsupported)
				}
			}
		}
	}
	return nil
}
