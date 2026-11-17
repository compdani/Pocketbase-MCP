// Package protocol holds the MCP Streamable HTTP / JSON-RPC 2.0 wire types
// and the supported protocol-version table. It is stdlib-only.
package protocol

import "encoding/json"

// Supported protocol versions, newest first. Add a version only after
// implementing what it requires (plan section 8).
var Supported = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Latest is returned when the client requests an unsupported version.
const Latest = "2025-06-18"

// IsSupported reports whether v is a supported protocol version.
func IsSupported(v string) bool {
	for _, s := range Supported {
		if s == v {
			return true
		}
	}
	return false
}

// Negotiate echoes a supported requested version, otherwise returns Latest.
func Negotiate(requested string) string {
	if IsSupported(requested) {
		return requested
	}
	return Latest
}

// JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type Response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

type ToolDefinition struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  map[string]any   `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
}

type ToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolResult struct {
	Content           []any `json:"content"`
	StructuredContent any   `json:"structuredContent,omitempty"`
	IsError           bool  `json:"isError,omitempty"`
}
