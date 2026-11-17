package pocketbasemcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/compdani/pocketbase-mcp/protocol"
	"github.com/compdani/pocketbase-mcp/schema"
)

type AuditEvent struct {
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
func (t ToolSpec) writeEffect() bool {
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
		a.server.emitAudit(a.principal, a.tool, "", collection, action, outcome, start, a.requestID, a.server.auditPayload(args))
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
		if t.Handler == nil {
			return fmt.Errorf("tool %q: missing handler", t.Name)
		}
		if err := ValidatePolicyName(t.Policy, c.Policies); err != nil {
			return fmt.Errorf("tool %q: %w", t.Name, err)
		}
		if t.Effects.Kind != "" && t.Effects.Kind != "read" && t.Effects.Kind != "write" {
			return fmt.Errorf("tool %q: invalid effect %q (want read or write)", t.Name, t.Effects.Kind)
		}
		switch t.Channels {
		case "", "mcp", "both":
		case "chat":
			// Nothing serves the chat channel yet; accepting the tool would
			// silently do nothing now and make it reachable later under
			// rules that don't exist yet. Use "both" for tools chat will share.
			return fmt.Errorf("tool %q: the chat channel isn't available yet (use \"mcp\" or \"both\")", t.Name)
		default:
			return fmt.Errorf("tool %q: invalid channels %q (want mcp or both)", t.Name, t.Channels)
		}
		switch t.ExecutionLocation {
		case "", "server":
		case "editor":
			// Editor tools run in the browser and belong to the chat plan
			// (proposals and approval); MCP clients can't run them.
			return fmt.Errorf("tool %q: execution location \"editor\" isn't supported yet (MCP tools run on the server)", t.Name)
		default:
			return fmt.Errorf("tool %q: invalid execution location %q (want server)", t.Name, t.ExecutionLocation)
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
func (s *Server) customAllowed(p Principal, t ToolSpec) bool {
	return t.onMCPChannel() && (!t.writeEffect() || s.cfg.AllowWrites) && EvaluatePolicy(t.Policy, p, s.cfg.Policies)
}
func (s *Server) callCustom(w http.ResponseWriter, r *http.Request, p Principal, id any, t ToolSpec, args map[string]any) {
	start := time.Now()
	outcome := "error"
	requestID := fmt.Sprint(id)
	defer func() {
		if s.cfg.AuditFunc != nil {
			s.cfg.AuditFunc(AuditEvent{PrincipalID: p.ID, PrincipalCollection: p.CollectionName, Tool: t.Name, Effect: t.effectKind(), Outcome: outcome, Duration: time.Since(start), RequestID: requestID, Payload: s.auditPayload(args)})
		}
	}()
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
		writeJSONRPCError(w, 200, id, protocol.CodeInvalidParams, "Invalid params", e.Error())
		return
	}
	timeout := t.Timeout
	if timeout == 0 {
		timeout = s.cfg.MaxToolTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	call := CallContext{Principal: p, Access: &CollectionAccess{server: s, principal: p, base: s.baseURL(r), writes: t.writeEffect(), tool: t.Name, requestID: id}, RequestID: requestID, Context: ctx, Logger: s.cfg.Logger, Audit: s.cfg.AuditFunc}
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
		writeToolError(w, id, "Tool execution timed out.")
		return
	case res = <-done:
	}
	if res.err != nil {
		if ctx.Err() != nil {
			outcome = "timeout"
			writeToolError(w, id, "Tool execution timed out.")
			return
		}
		// ToolErrors and AccessErrors are user-safe by construction (facade
		// not-found, PB rule rejections); everything else is sanitized so
		// handler details (and anything they embed) never leak.
		if msg, ok := UserMessage(res.err); ok {
			outcome = AuditOutcome(res.err)
			writeToolError(w, id, msg)
			return
		}
		s.cfg.Logger.Error("custom tool failed", "tool", t.Name, "requestId", requestID, "error", res.err)
		writeToolError(w, id, "Tool execution failed.")
		return
	}
	if ctx.Err() != nil {
		outcome = "timeout"
		writeToolError(w, id, "Tool execution timed out.")
		return
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
			writeToolError(w, id, "Tool output failed validation.")
			return
		}
	}
	text, e := json.Marshal(tr.StructuredContent)
	if e != nil {
		writeToolError(w, id, "Tool execution failed.")
		return
	}
	tr.Content = append([]any{protocol.ToolContent{Type: "text", Text: string(text)}}, tr.Content...)
	b, e := json.Marshal(tr)
	limit := t.MaxResponseBytes
	if limit == 0 {
		limit = s.cfg.MaxResponseBytes
	}
	if e != nil || int64(len(b)) > limit {
		writeToolError(w, id, "Tool response exceeds limit.")
		return
	}
	outcome = "ok"
	writeJSONRPCResult(w, id, tr)
}

func (s *Server) emitAudit(p Principal, tool, effect, collection, action, outcome string, start time.Time, id any, payload map[string]any) {
	if s.cfg.AuditFunc == nil {
		return
	}
	if effect == "" {
		effect = "read"
		if isWriteOp(action) {
			effect = "write"
		}
	}
	s.cfg.AuditFunc(AuditEvent{PrincipalID: p.ID, PrincipalCollection: p.CollectionName, Tool: tool, Effect: effect, Collection: collection, Action: action, Outcome: outcome, Duration: time.Since(start), RequestID: fmt.Sprint(id), Payload: payload})
}
