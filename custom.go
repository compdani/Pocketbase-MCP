package pocketbasemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/compdani/pocketbase-mcp/protocol"
	"github.com/compdani/pocketbase-mcp/schema"
)

type AuditEvent struct {
	// Channel is the invocation surface that emitted the event: "mcp" or
	// "chat". The same tool called through either surface is audited the same
	// way apart from this field.
	Channel             string
	PrincipalID         string
	PrincipalCollection string
	Tool                string
	Effect              string
	Collection          string
	Action              string
	Outcome             string
	Duration            time.Duration
	RequestID           string
	// Payload holds a redacted copy of the call arguments. It is nil unless
	// Config.AuditIncludePayload is enabled; tokens, passwords and secrets
	// are never included (see redactAuditValue).
	Payload map[string]any `json:",omitempty"`
}

// writeEffect reports whether the tool performs writes (Effects.Kind wins,
// the deprecated Write bool is honoured when Kind is unset).
//
// An editor tool is always a write: its effect is a draft patch the browser
// applies after approval. It writes nothing on the server, so it is a write for
// review purposes only and Config.AllowWrites never gates it.
func (t ToolSpec) writeEffect() bool {
	if t.executionLocation() == "editor" {
		return true
	}
	return t.Write || t.Effects.Kind == "write"
}

// effectKind normalizes the declared effect to "read" or "write".
func (t ToolSpec) effectKind() string {
	if t.writeEffect() {
		return "write"
	}
	if t.Effects.Kind != "" {
		return t.Effects.Kind
	}
	return "read"
}

// onMCPChannel reports whether the tool is exposed on the MCP channel
// (Channels defaults to "mcp").
func (t ToolSpec) onMCPChannel() bool {
	return t.Channels == "" || t.Channels == "mcp" || t.Channels == "both"
}

// onChatChannel reports whether the tool declares the chat channel. An empty
// Channels value means "mcp", never chat.
func (t ToolSpec) onChatChannel() bool {
	return t.Channels == "chat" || t.Channels == "both"
}

// approvalAuto reports whether the host may run the tool without review when
// its chat options also enable auto-approval. The default is review.
func (t ToolSpec) approvalAuto() bool { return t.Approval == "auto" }

// executionLocation normalizes the declared execution location to "server".
func (t ToolSpec) executionLocation() string {
	if t.ExecutionLocation == "" {
		return "server"
	}
	return t.ExecutionLocation
}

// editorTool reports whether the tool's effect is a draft patch the browser
// applies, rather than a server-side call.
func (t ToolSpec) editorTool() bool {
	return t.executionLocation() == "editor"
}

func redactAuditValue(key string, v any) any {
	lk := strings.ToLower(key)
	if strings.Contains(lk, "token") || strings.Contains(lk, "password") || strings.Contains(lk, "secret") || strings.Contains(lk, "authorization") {
		return "[redacted]"
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = redactAuditValue(k, val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redactAuditValue("", val)
		}
		return out
	case string:
		if len(x) > 256 {
			return x[:256] + "..."
		}
		return x
	default:
		return v
	}
}

// auditPayload returns a redacted copy of args, or nil when payload capture
// is disabled (the default) or there are no arguments.
func (s *Server) auditPayload(args map[string]any) map[string]any {
	if !s.cfg.AuditIncludePayload || len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = redactAuditValue(k, v)
	}
	return out
}

type AuditFunc func(AuditEvent)
type CallContext struct {
	Principal Principal
	Access    *CollectionAccess
	RequestID string
	Context   context.Context
	Logger    *slog.Logger
	Audit     AuditFunc
}

// CollectionAccess dispatches through precisely the generated-tool gate and host path.
type CollectionAccess struct {
	server    *Server
	principal Principal
	base      string
	// channel is the invocation surface this access was built for ("mcp" or
	// "chat"); it labels the audit events.
	channel string
	// writes is true only for tools that declare a write effect; read tools
	// can't create, update or delete through the facade.
	writes bool
	// tool and requestID label the per-call audit events.
	tool      string
	requestID any
}

// Call runs one collection operation and records it in the audit log under
// the calling tool, so the log shows what a custom tool touched.
func (a *CollectionAccess) Call(ctx context.Context, collection, action string, args map[string]any) (out any, err error) {
	start := time.Now()
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = AuditOutcome(err)
		}
		a.server.emitAudit(a.channel, a.principal, a.tool, "", collection, action, outcome, start, a.requestID, a.server.auditPayload(args))
	}()
	cfg, ok := a.server.host.LookupCollection(collection)
	if !ok {
		return nil, NotFound("Unknown or unavailable collection.")
	}
	if isWriteOp(action) && !a.writes {
		return nil, Denied("This tool is read-only.")
	}
	if yes, msg := a.server.canInvoke(a.principal, cfg, action); !yes {
		return nil, Denied(msg)
	}
	switch action {
	case "search":
		return a.server.host.Search(ctx, a.principal, cfg, args, a.base)
	case "fetch":
		return a.server.host.Fetch(ctx, a.principal, cfg, args, a.base)
	case "create", "update", "delete":
		return a.server.host.Write(ctx, a.principal, action, cfg, args, a.base)
	}
	return nil, Invalid("Unknown operation.")
}
func (a *CollectionAccess) Fetch(ctx context.Context, collection, id string) (any, error) {
	return a.Call(ctx, collection, "fetch", map[string]any{"id": id})
}
func (a *CollectionAccess) Search(ctx context.Context, collection string, args map[string]any) (any, error) {
	return a.Call(ctx, collection, "search", args)
}
func (a *CollectionAccess) Create(ctx context.Context, collection string, data map[string]any) (any, error) {
	return a.Call(ctx, collection, "create", map[string]any{"data": data})
}
func (a *CollectionAccess) Update(ctx context.Context, collection, id string, data map[string]any) (any, error) {
	return a.Call(ctx, collection, "update", map[string]any{"id": id, "data": data})
}
func (a *CollectionAccess) Delete(ctx context.Context, collection, id string) (any, error) {
	return a.Call(ctx, collection, "delete", map[string]any{"id": id})
}
func (c Config) ValidateCustomTools() error {
	c = c.withDefaults()
	names := []string{}
	for _, t := range c.CustomTools {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("custom tool: name is required")
		}
		names = append(names, t.Name)
		if err := validateToolExecution(t); err != nil {
			return err
		}
		if err := ValidatePolicyName(t.Policy, c.Policies); err != nil {
			return fmt.Errorf("tool %q: %w", t.Name, err)
		}
		if t.Effects.Kind != "" && t.Effects.Kind != "read" && t.Effects.Kind != "write" {
			return fmt.Errorf("tool %q: invalid effect %q (want read or write)", t.Name, t.Effects.Kind)
		}
		switch t.Channels {
		case "", "mcp", "both", "chat":
			// "chat" is served by the read-only chat surface (v1.1) plus the
			// proposal surface (v1.2). A chat tool is never listed or callable
			// over MCP. Editor tools are chat-only and validated above.
		default:
			return fmt.Errorf("tool %q: invalid channels %q (want mcp, chat or both)", t.Name, t.Channels)
		}
		switch t.Approval {
		case "":
		case "auto":
			if !t.writeEffect() {
				return fmt.Errorf("tool %q: approval \"auto\" requires a write effect", t.Name)
			}
		default:
			return fmt.Errorf("tool %q: invalid approval %q (want auto)", t.Name, t.Approval)
		}
		if t.Timeout < 0 || t.Timeout > c.MaxToolTimeout || t.MaxResponseBytes < 0 || t.MaxResponseBytes > c.MaxResponseBytes {
			return fmt.Errorf("tool %q: timeout/response limit exceeds server maximum", t.Name)
		}
		if t.InputSchema != nil {
			if _, e := schema.Compile(t.InputSchema); e != nil {
				return fmt.Errorf("tool %q input: %w", t.Name, e)
			}
		}
		if t.OutputSchema != nil {
			if _, e := schema.Compile(t.OutputSchema); e != nil {
				return fmt.Errorf("tool %q output: %w", t.Name, e)
			}
		}
	}
	return c.EffectiveManifest().ValidateStructural(c.Policies, names)
}

// validateToolExecution enforces the location-specific contract. A server tool
// runs here and needs a handler. An editor tool is a chat-only draft patch: it
// has no server handler at all, every change is reviewed, and its schema names
// the editor and revision the patch was computed against.
func validateToolExecution(t ToolSpec) error {
	switch t.executionLocation() {
	case "server":
		if t.Handler == nil {
			return fmt.Errorf("tool %q: missing handler", t.Name)
		}
		return nil
	case "editor":
		return validateEditorTool(t)
	default:
		return fmt.Errorf("tool %q: invalid execution location %q (want server or editor)", t.Name, t.ExecutionLocation)
	}
}

// validateEditorTool enforces the editor ToolSpec contract:
//
//   - Channels is exactly "chat": an MCP client cannot run a browser action.
//   - Handler is nil: nothing about the tool runs on the server.
//   - Approval is "" (the default review): an editor patch always needs the
//     user's approval before the browser may apply it.
//   - The effect is a write. Effects.Kind may be empty, because an editor tool
//     is a write by definition; a declared "read" is refused.
//   - InputSchema is an object that REQUIRES string properties "editorId" (the
//     editor/document the patch targets) and "baseRevision" (the editor
//     revision the patch was computed against).
func validateEditorTool(t ToolSpec) error {
	if t.Channels != "chat" {
		return fmt.Errorf("tool %q: an editor tool is chat-only and must declare channels \"chat\" (an MCP client cannot run a browser editor action)", t.Name)
	}
	if t.Handler != nil {
		return fmt.Errorf("tool %q: an editor tool cannot declare a handler; its effect is a draft patch the browser applies", t.Name)
	}
	if t.Approval != "" {
		return fmt.Errorf("tool %q: an editor tool cannot declare approval %q; every editor change needs review", t.Name, t.Approval)
	}
	if t.Effects.Kind == "read" {
		return fmt.Errorf("tool %q: an editor tool is a write effect; remove Effects.Kind \"read\"", t.Name)
	}
	return validateEditorSchema(t)
}

// validateEditorSchema requires the two properties every editor patch binds.
// Without them a proposal could not say which document it edits or which
// revision it was computed against, so it could never be applied safely.
func validateEditorSchema(t ToolSpec) error {
	if t.InputSchema == nil {
		return fmt.Errorf("tool %q: an editor tool requires an input schema of type object with required string properties \"editorId\" and \"baseRevision\"", t.Name)
	}
	if typ, _ := t.InputSchema["type"].(string); typ != "object" {
		return fmt.Errorf("tool %q: an editor tool's input schema must be of type object", t.Name)
	}
	props, _ := t.InputSchema["properties"].(map[string]any)
	for _, field := range []string{"editorId", "baseRevision"} {
		prop, ok := props[field].(map[string]any)
		if !ok {
			return fmt.Errorf("tool %q: an editor tool's input schema must declare a %q property", t.Name, field)
		}
		if typ, _ := prop["type"].(string); typ != "string" {
			return fmt.Errorf("tool %q: editor property %q must be a string", t.Name, field)
		}
		if !schemaRequires(t.InputSchema, field) {
			return fmt.Errorf("tool %q: an editor tool's input schema must require %q", t.Name, field)
		}
	}
	return nil
}

// schemaRequires reports whether a JSON-schema object lists field in required.
// The schema is a decoded map, so required may be []string (built in Go) or
// []any (decoded from JSON).
func schemaRequires(sch map[string]any, field string) bool {
	switch req := sch["required"].(type) {
	case []string:
		for _, name := range req {
			if name == field {
				return true
			}
		}
	case []any:
		for _, v := range req {
			if name, ok := v.(string); ok && name == field {
				return true
			}
		}
	}
	return false
}

// customAllowedOnChannel applies the tool gate for one channel. Chat only
// offers custom tools that declare that channel and have a read effect; the
// chat-writes variant additionally offers a write-effect tool when the host
// enabled proposals and writes, and policy still applies. The variant is
// internal: no request can ask for it. MCP keeps its existing rule (mcp/both
// channels, writes need AllowWrites).
//
// An editor tool is its own case: it is chat-only and offered exactly when the
// chat-writes variant is on (which requires ChatOptions.Proposals). It writes
// nothing on the server, so Config.AllowWrites does not gate it; policy does.
func (s *Server) customAllowedOnChannel(p Principal, t ToolSpec, channel string, chatWrites bool) bool {
	if t.editorTool() {
		return channel == "chat" && t.onChatChannel() && chatWrites && EvaluatePolicy(t.Policy, p, s.cfg.Policies)
	}
	if channel == "chat" {
		if !t.onChatChannel() {
			return false
		}
		if t.writeEffect() && !(chatWrites && s.cfg.AllowWrites) {
			return false
		}
		return EvaluatePolicy(t.Policy, p, s.cfg.Policies)
	}
	return t.onMCPChannel() && (!t.writeEffect() || s.cfg.AllowWrites) && EvaluatePolicy(t.Policy, p, s.cfg.Policies)
}

// customToolTimeoutMessage is the user-safe text of a custom tool whose handler
// outlived its deadline. It is a named constant because a proposal executor has
// to recognize it: after dispatch, a timeout may still have applied.
const customToolTimeoutMessage = "Tool execution timed out."

// outcomeUnknownFallback is the message of an OutcomeUnknown("").
const outcomeUnknownFallback = "The tool may or may not have applied this change; check before retrying."

// callCustom runs one custom tool and returns its outcome without touching a
// ResponseWriter. The writer (callTool's caller) keeps the MCP wire shape.
func (s *Server) callCustom(ctx context.Context, p Principal, baseURL, channel string, t ToolSpec, args map[string]any, requestID any) (any, string, *protocol.Error) {
	start := time.Now()
	outcome := "error"
	requestIDStr := requestIDString(requestID)
	audit := func() {
		if s.cfg.AuditFunc != nil {
			s.cfg.AuditFunc(AuditEvent{Channel: channel, PrincipalID: p.ID, PrincipalCollection: p.CollectionName, Tool: t.Name, Effect: t.effectKind(), Outcome: outcome, Duration: time.Since(start), RequestID: requestIDStr, Payload: s.auditPayload(args)})
		}
	}
	defer audit()
	inputSchema := t.InputSchema
	if inputSchema == nil {
		inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	input, e := schema.Compile(inputSchema)
	if e == nil {
		e = input.Validate(args)
	}
	if e != nil {
		outcome = "invalid"
		return nil, "", &protocol.Error{Code: protocol.CodeInvalidParams, Message: "Invalid params", Data: e.Error()}
	}
	if msg := runToolValidate(ctx, &t, p, args); msg != "" {
		outcome = "invalid"
		return nil, "", &protocol.Error{Code: protocol.CodeInvalidParams, Message: "Invalid params", Data: msg}
	}
	timeout := t.Timeout
	if timeout == 0 {
		timeout = s.cfg.MaxToolTimeout
		if d, ok := ctx.Value(chatToolTimeoutKey{}).(time.Duration); ok && d > 0 && channel == "chat" {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	call := CallContext{Principal: p, Access: &CollectionAccess{server: s, principal: p, base: baseURL, channel: channel, writes: t.writeEffect(), tool: t.Name, requestID: requestID}, RequestID: requestIDStr, Context: ctx, Logger: s.cfg.Logger, Audit: s.cfg.AuditFunc}
	type result struct {
		value any
		err   error
	}
	done := make(chan result, 1)
	go func() { v, e := t.Handler(ctx, call, args); done <- result{v, e} }()
	var res result
	select {
	case <-ctx.Done():
		outcome = "timeout"
		return nil, customToolTimeoutMessage, nil
	case res = <-done:
	}
	if res.err != nil {
		if ctx.Err() != nil {
			outcome = "timeout"
			return nil, customToolTimeoutMessage, nil
		}
		// ToolErrors and AccessErrors are user-safe by construction (facade
		// not-found, PB rule rejections); everything else is sanitized so
		// handler details (and anything they embed) never leak.
		if msg, ok := UserMessage(res.err); ok {
			outcome = AuditOutcome(res.err)
			var ae AccessError
			if strings.TrimSpace(msg) == "" {
				// An empty user-safe message must never read as success:
				// callers treat an empty userErr as "no error".
				msg = "Tool execution failed."
				if errors.As(res.err, &ae) && ae.Kind == "unknown" {
					msg = outcomeUnknownFallback
				}
			}
			if errors.As(res.err, &ae) && ae.Kind == "refused" {
				// The chat executor turns this into chat.RefusedError; MCP
				// only reads msg.
				return toolRefusal{}, msg, nil
			}
			if errors.As(res.err, &ae) && ae.Kind == "unknown" {
				// The chat executor records an approved change as "outcome
				// unknown"; MCP only reads msg.
				return toolOutcomeUnknown{}, msg, nil
			}
			return nil, msg, nil
		}
		s.cfg.Logger.Error("custom tool failed", "tool", t.Name, "requestId", requestIDStr, "error", res.err)
		return nil, "Tool execution failed.", nil
	}
	if ctx.Err() != nil {
		outcome = "timeout"
		return nil, customToolTimeoutMessage, nil
	}
	value := res.value
	tr, ok := value.(protocol.ToolResult)
	if !ok {
		tr = protocol.ToolResult{StructuredContent: value}
	}
	if t.OutputSchema != nil && !s.cfg.DisableOutputValidation {
		sc, e := schema.Compile(t.OutputSchema)
		if e == nil {
			e = sc.Validate(tr.StructuredContent)
		}
		if e != nil {
			return nil, "Tool output failed validation.", nil
		}
	}
	text, e := json.Marshal(tr.StructuredContent)
	if e != nil {
		return nil, "Tool execution failed.", nil
	}
	tr.Content = append([]any{protocol.ToolContent{Type: "text", Text: string(text)}}, tr.Content...)
	b, e := json.Marshal(tr)
	limit := t.MaxResponseBytes
	if limit == 0 {
		limit = s.cfg.MaxResponseBytes
	}
	if e != nil || int64(len(b)) > limit {
		return nil, "Tool response exceeds limit.", nil
	}
	outcome = "ok"
	return tr, "", nil
}

// requestIDString renders an audit request id. A missing id (chat calls carry
// no JSON-RPC id) is the empty string rather than "<nil>".
// runToolValidate runs a tool's Validate hook and returns the refusal to show
// the model, or "" when the arguments pass. A host's ToolError or
// AccessError message is shown as-is; any other error stays generic, so hook
// internals never reach the model.
func runToolValidate(ctx context.Context, t *ToolSpec, p Principal, args map[string]any) string {
	if t == nil || t.Validate == nil {
		return ""
	}
	err := t.Validate(ctx, p, args)
	if err == nil {
		return ""
	}
	if msg, ok := UserMessage(err); ok && strings.TrimSpace(msg) != "" {
		return msg
	}
	return "the arguments were refused"
}

func requestIDString(id any) string {
	if id == nil {
		return ""
	}
	return fmt.Sprint(id)
}

func (s *Server) emitAudit(channel string, p Principal, tool, effect, collection, action, outcome string, start time.Time, id any, payload map[string]any) {
	if s.cfg.AuditFunc == nil {
		return
	}
	if channel == "" {
		channel = "mcp"
	}
	if effect == "" {
		effect = "read"
		if isWriteOp(action) {
			effect = "write"
		}
	}
	s.cfg.AuditFunc(AuditEvent{Channel: channel, PrincipalID: p.ID, PrincipalCollection: p.CollectionName, Tool: tool, Effect: effect, Collection: collection, Action: action, Outcome: outcome, Duration: time.Since(start), RequestID: requestIDString(id), Payload: payload})
}

// maxToolWarnings and maxToolWarningRunes bound what a Warn hook may store.
const (
	maxToolWarnings     = 10
	maxToolWarningRunes = 500
)

// runToolPrepare runs a tool's Prepare hook. The refusal is shown like
// Validate's: a host's ToolError or AccessError message as-is, anything else
// generic.
func runToolPrepare(ctx context.Context, t *ToolSpec, p Principal, args map[string]any) (map[string]any, string) {
	if t == nil || t.Prepare == nil {
		return args, ""
	}
	out, err := t.Prepare(ctx, p, args)
	if err != nil {
		if msg, ok := UserMessage(err); ok && strings.TrimSpace(msg) != "" {
			return nil, msg
		}
		return nil, "the arguments could not be prepared"
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, ""
}

// runToolWarn runs a tool's Warn hook and keeps its non-empty notes, bounded.
func runToolWarn(ctx context.Context, t *ToolSpec, p Principal, args map[string]any) []string {
	if t == nil || t.Warn == nil {
		return nil
	}
	var out []string
	for _, w := range t.Warn(ctx, p, args) {
		w = strings.Join(strings.Fields(w), " ")
		if w == "" {
			continue
		}
		if r := []rune(w); len(r) > maxToolWarningRunes {
			w = string(r[:maxToolWarningRunes]) + "…"
		}
		out = append(out, w)
		if len(out) == maxToolWarnings {
			break
		}
	}
	return out
}

// toolRefusal marks a custom tool's Refused error in the result slot of
// callTool, which MCP ignores when there is a user error.
type toolRefusal struct{}

// toolOutcomeUnknown marks a custom tool's OutcomeUnknown error the same way.
type toolOutcomeUnknown struct{}
