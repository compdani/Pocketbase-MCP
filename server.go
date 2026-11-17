package pocketbasemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/compdani/pocketbase-mcp/protocol"
)

var reservedArgs = []string{"query", "limit", "page", "sort", "collection", "filters", "id"}

// Server is a per-instance MCP server. No package-level state.
type Server struct {
	cfg  Config
	host Host
}

// New creates a Server bound to a Host implementation.
func New(cfg Config, host Host) *Server {
	return &Server{cfg: cfg.withDefaults(), host: host}
}

// Config returns the effective config.
func (s *Server) Config() Config { return s.cfg }

// Paths returns canonical + alias paths.
func (s *Server) Paths() []string {
	out := []string{s.cfg.Path}
	out = append(out, s.cfg.AliasPaths...)
	return out
}

func (s *Server) manifest() Manifest { return s.cfg.EffectiveManifest() }
func (s *Server) toolMode() string   { return s.manifest().ToolMode }

func (s *Server) policyAllows(p Principal, cfg *CollectionConfig) bool {
	if cfg == nil {
		return false
	}
	return EvaluatePolicy(cfg.Policy, p, s.cfg.Policies)
}

// opState returns unavailable|available|conditional with a non-sensitive reason.
func (s *Server) opState(p Principal, cfg *CollectionConfig, op string) (string, string) {
	if cfg == nil || !cfg.HasOperation(op) {
		return "unavailable", "operation not declared in the manifest"
	}
	if (op == "create" || op == "update" || op == "delete") && !s.cfg.AllowWrites {
		return "unavailable", "writes are disabled"
	}
	if !s.policyAllows(p, cfg) {
		return "unavailable", PolicyReason(cfg.Policy)
	}
	if p.IsSuperuser {
		return "available", "PocketBase rules permit this operation"
	}
	switch cfg.RuleFor(op) {
	case RuleNil:
		return "unavailable", "PocketBase rule restricts this operation"
	case RulePublic:
		return "available", "PocketBase rule allows authenticated callers"
	default:
		return "conditional", "PocketBase rule applies to each request or record"
	}
}

// callerVisibleCollections is host-visible collections also passing the manifest policy.
func (s *Server) callerVisibleCollections(p Principal) []*CollectionConfig {
	var out []*CollectionConfig
	for _, c := range s.host.VisibleCollections(p) {
		if s.policyAllows(p, c) {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) hostVisible(p Principal, cfg *CollectionConfig) bool {
	for _, c := range s.host.VisibleCollections(p) {
		if c.Name == cfg.Name {
			return true
		}
	}
	return false
}

// canInvoke enforces the shared policy engine for direct calls to hidden tools.
func (s *Server) canInvoke(p Principal, cfg *CollectionConfig, op string) (bool, string) {
	if cfg == nil {
		return false, "Unknown or unavailable collection."
	}
	if !s.hostVisible(p, cfg) {
		return false, "Unknown or unavailable collection."
	}
	state, reason := s.opState(p, cfg, op)
	if state == "unavailable" {
		return false, "This operation is unavailable: " + reason + "."
	}
	return true, ""
}

func (s *Server) acceptPrincipal(p Principal) bool {
	if p.IsSuperuser || p.CollectionName == "_superusers" {
		return s.cfg.Auth.AllowSuperusers
	}
	accepted := s.cfg.EffectiveAuthCollections()
	if len(accepted) == 0 {
		return false // fail closed: accepted collections must be explicit
	}
	// Accept by collection name or stable ID.
	return slices.Contains(accepted, p.CollectionName) || (p.CollectionID != "" && slices.Contains(accepted, p.CollectionID))
}

func extractToken(r *http.Request) string {
	t := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(t) > 7 && strings.EqualFold(t[:7], "Bearer ") {
		return strings.TrimSpace(t[7:])
	}
	return t
}

// Handler returns a stdlib http.Handler that authenticates via the Host.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractToken(r)
		if token == "" {
			s.writeUnauthorized(w, r)
			return
		}
		p, err := s.host.Authenticate(token)
		if err != nil {
			s.writeUnauthorized(w, r)
			return
		}
		p.Token = token
		s.ServeWithPrincipal(w, r, p)
	})
}

// ServeWithPrincipal serves one request for an already-authenticated principal
// (used by PocketBase adapters where e.Auth is already resolved).
func (s *Server) ServeWithPrincipal(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.acceptPrincipal(p) {
		s.writeUnauthorized(w, r)
		return
	}
	if !s.allowOrigin(r) {
		writeJSONRPCError(w, http.StatusForbidden, nil, protocol.CodeInvalidRequest, "Invalid origin", nil)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if v := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version")); v != "" && !protocol.IsSupported(v) {
		writeJSONRPCError(w, http.StatusBadRequest, nil, protocol.CodeInvalidRequest, "Unsupported MCP-Protocol-Version", v)
		return
	}
	var body []byte
	var err error
	if s.cfg.MaxBodyBytes > 0 {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	} else {
		body, err = io.ReadAll(r.Body)
	}
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, protocol.CodeParseError, "Parse error", nil)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		writeJSONRPCError(w, http.StatusBadRequest, nil, protocol.CodeInvalidRequest, "Batch requests are not supported", nil)
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, protocol.CodeParseError, "Parse error", nil)
		return
	}
	var req protocol.Request
	_ = json.Unmarshal(trimmed, &req)
	if _, hasID := raw["id"]; !hasID || req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSONRPCError(w, http.StatusBadRequest, req.ID, protocol.CodeInvalidRequest, "Invalid Request", nil)
		return
	}
	switch req.Method {
	case "initialize":
		writeJSONRPCResult(w, req.ID, s.initializeResult(req.Params))
	case "ping":
		writeJSONRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeJSONRPCResult(w, req.ID, map[string]any{"tools": s.toolList(p)})
	case "tools/call":
		s.handleToolCall(w, r, p, req.ID, req.Params)
	case "resources/list":
		writeJSONRPCResult(w, req.ID, map[string]any{"resources": []any{}})
	case "resources/templates/list":
		writeJSONRPCResult(w, req.ID, map[string]any{"resourceTemplates": []any{}})
	case "prompts/list":
		writeJSONRPCResult(w, req.ID, map[string]any{"prompts": []any{}})
	default:
		writeJSONRPCError(w, http.StatusOK, req.ID, protocol.CodeMethodNotFound, "Method not found", req.Method)
	}
}

func (s *Server) allowOrigin(r *http.Request) bool {
	if len(s.cfg.AllowedOrigins) == 0 {
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	return slices.Contains(s.cfg.AllowedOrigins, origin)
}

func (s *Server) initializeResult(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	info := map[string]any{"name": s.cfg.ServerName, "version": s.cfg.ServerVersion}
	if s.cfg.ServerTitle != "" {
		info["title"] = s.cfg.ServerTitle
	}
	return map[string]any{
		"protocolVersion": protocol.Negotiate(p.ProtocolVersion),
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      info,
		"instructions":    s.cfg.Instructions,
	}
}

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	scheme := "http"
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// --- tools/list ---

func readOnlyAnnotations(title string) *protocol.ToolAnnotations {
	return &protocol.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

func writeAnnotations(title string, destructive bool) *protocol.ToolAnnotations {
	return &protocol.ToolAnnotations{Title: title, DestructiveHint: destructive}
}

func (s *Server) searchPropsFor(cfg *CollectionConfig) map[string]any {
	maxL := cfg.MaxLimit
	if maxL == 0 {
		maxL = 50
	}
	defL := cfg.DefaultLimit
	if defL == 0 {
		defL = 20
	}
	props := map[string]any{
		"query": map[string]any{"type": "string", "description": "Case-insensitive text matched against: " + strings.Join(cfg.SearchFields, ", ") + ". Omit to list records."},
		"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxL, "default": defL, "description": "Results per page."},
		"page":  map[string]any{"type": "integer", "minimum": 1, "default": 1, "description": "1-based page number."},
		"sort":  map[string]any{"type": "string", "description": "Comma-separated sort fields, prefix with - for descending. Allowed: " + strings.Join(cfg.SortFields, ", ") + ". Default: " + strings.Join(cfg.DefaultSort, ", ") + "."},
	}
	for _, f := range FiltersFor(cfg) {
		props[f.Argument] = f.Schema()
	}
	return props
}

func (s *Server) dataSchemaFor(cfg *CollectionConfig, op string) map[string]any {
	fields := cfg.CreateFields
	if op == "update" {
		fields = cfg.UpdateFields
	}
	props := map[string]any{}
	for _, name := range fields {
		if f, ok := cfg.FieldMap[name]; ok {
			props[name] = fieldValueSchema(f)
		}
	}
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false, "description": "Field values keyed by field name. Relations take record ids. File fields cannot be set through MCP."}
}

func (s *Server) toolList(p Principal) []protocol.ToolDefinition {
	visible := s.callerVisibleCollections(p)
	names := make([]string, 0, len(visible))
	for _, c := range visible {
		names = append(names, c.Name)
	}
	tools := []protocol.ToolDefinition{describeAccessToolDefinition(), describeToolDefinition(names)}
	for _, ct := range s.cfg.CustomTools {
		if !s.customAllowed(p, ct) {
			continue
		}
		sch := ct.InputSchema
		if sch == nil {
			sch = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		ann := ct.Annotations
		if ann == nil {
			// Annotations are hints only; they never grant permission.
			// Synthesize them from the declared effect when unset.
			title := ct.Title
			if title == "" {
				title = ct.Name
			}
			if ct.effectKind() == "write" {
				ann = &protocol.ToolAnnotations{Title: title, IdempotentHint: ct.Effects.Idempotent, OpenWorldHint: ct.Effects.ExternalAccess}
			} else {
				a := readOnlyAnnotations(title)
				a.IdempotentHint = ct.Effects.Idempotent
				a.OpenWorldHint = ct.Effects.ExternalAccess
				ann = a
			}
		}
		tools = append(tools, protocol.ToolDefinition{Name: ct.Name, Title: ct.Title, Description: ct.Description, InputSchema: sch, OutputSchema: ct.OutputSchema, Annotations: ann})
	}
	if len(visible) == 0 {
		return tools
	}
	if s.toolMode() == "generic" {
		// per-op collection enums
		enumFor := func(op string) []string {
			var out []string
			for _, c := range visible {
				if st, _ := s.opState(p, c, op); st != "unavailable" {
					out = append(out, c.Name)
				}
			}
			return out
		}
		if e := enumFor("search"); len(e) > 0 {
			props := baseSearchPropsGeneric()
			props["collection"] = map[string]any{"type": "string", "enum": e, "description": "Collection to use."}
			tools = append(tools, protocol.ToolDefinition{Name: "search", Title: "Search records", Description: "Search or list records of a collection by free text and field filters. Returns summaries with paging info; use fetch for full records.", InputSchema: map[string]any{"type": "object", "properties": props, "required": []string{"collection"}, "additionalProperties": false}, OutputSchema: searchOutputSchema(), Annotations: readOnlyAnnotations("Search records")})
		}
		if e := enumFor("fetch"); len(e) > 0 {
			tools = append(tools, protocol.ToolDefinition{Name: "fetch", Title: "Fetch record", Description: "Fetch one full record by collection and id, with configured relations expanded.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"collection": map[string]any{"type": "string", "enum": e, "description": "Collection to use."}, "id": map[string]any{"type": "string", "description": "Record id."}}, "required": []string{"collection", "id"}, "additionalProperties": false}, OutputSchema: fetchOutputSchema(), Annotations: readOnlyAnnotations("Fetch record")})
		}
		if e := enumFor("create"); len(e) > 0 {
			tools = append(tools, protocol.ToolDefinition{Name: "create", Title: "Create record", Description: "Create a record. Collection rules, validation and hooks apply. Returns the created record.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"collection": map[string]any{"type": "string", "enum": e}, "data": map[string]any{"type": "object", "additionalProperties": true, "description": "Field values (see describe_collection)."}}, "required": []string{"collection", "data"}, "additionalProperties": false}, OutputSchema: fetchOutputSchema(), Annotations: writeAnnotations("Create record", false)})
		}
		if e := enumFor("update"); len(e) > 0 {
			tools = append(tools, protocol.ToolDefinition{Name: "update", Title: "Update record", Description: "Update fields of an existing record. Collection rules, validation and hooks apply.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"collection": map[string]any{"type": "string", "enum": e}, "id": map[string]any{"type": "string"}, "data": map[string]any{"type": "object", "additionalProperties": true}}, "required": []string{"collection", "id", "data"}, "additionalProperties": false}, OutputSchema: fetchOutputSchema(), Annotations: &protocol.ToolAnnotations{Title: "Update record", DestructiveHint: true, IdempotentHint: true}})
		}
		if e := enumFor("delete"); len(e) > 0 {
			tools = append(tools, protocol.ToolDefinition{Name: "delete", Title: "Delete record", Description: "Permanently delete a record. Collection rules and relation cascade settings apply.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"collection": map[string]any{"type": "string", "enum": e}, "id": map[string]any{"type": "string"}}, "required": []string{"collection", "id"}, "additionalProperties": false}, Annotations: &protocol.ToolAnnotations{Title: "Delete record", DestructiveHint: true, IdempotentHint: true}})
		}
		return tools
	}
	for _, cfg := range visible {
		alias := cfg.Alias
		if alias == "" {
			alias = cfg.Name
		}
		desc := cfg.Description
		if desc != "" {
			desc = " " + desc
		}
		if st, _ := s.opState(p, cfg, "search"); st != "unavailable" {
			title := "Search " + cfg.Name
			tools = append(tools, protocol.ToolDefinition{Name: "search_" + alias, Title: title, Description: "Search or list " + cfg.Name + " records by free text and field filters." + desc + " Returns summaries with paging info; use fetch_" + alias + " for full records.", InputSchema: map[string]any{"type": "object", "properties": s.searchPropsFor(cfg), "additionalProperties": false}, OutputSchema: searchOutputSchema(), Annotations: readOnlyAnnotations(title)})
		}
		if st, _ := s.opState(p, cfg, "fetch"); st != "unavailable" {
			title := "Fetch " + cfg.Name
			d := "Fetch one full " + cfg.Name + " record by id."
			if len(cfg.Expand) > 0 {
				d += " Expands: " + strings.Join(cfg.Expand, ", ") + "."
			}
			tools = append(tools, protocol.ToolDefinition{Name: "fetch_" + alias, Title: title, Description: d, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "description": "Record id."}}, "required": []string{"id"}, "additionalProperties": false}, OutputSchema: fetchOutputSchema(), Annotations: readOnlyAnnotations(title)})
		}
		if st, _ := s.opState(p, cfg, "create"); st != "unavailable" {
			title := "Create " + cfg.Name
			tools = append(tools, protocol.ToolDefinition{Name: "create_" + alias, Title: title, Description: "Create a " + cfg.Name + " record. Collection rules, validation and hooks apply.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"data": s.dataSchemaFor(cfg, "create")}, "required": []string{"data"}, "additionalProperties": false}, OutputSchema: fetchOutputSchema(), Annotations: writeAnnotations(title, false)})
		}
		if st, _ := s.opState(p, cfg, "update"); st != "unavailable" {
			title := "Update " + cfg.Name
			tools = append(tools, protocol.ToolDefinition{Name: "update_" + alias, Title: title, Description: "Update a " + cfg.Name + " record. Collection rules, validation and hooks apply.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "data": s.dataSchemaFor(cfg, "update")}, "required": []string{"id", "data"}, "additionalProperties": false}, OutputSchema: fetchOutputSchema(), Annotations: &protocol.ToolAnnotations{Title: title, DestructiveHint: true, IdempotentHint: true}})
		}
		if st, _ := s.opState(p, cfg, "delete"); st != "unavailable" {
			title := "Delete " + cfg.Name
			tools = append(tools, protocol.ToolDefinition{Name: "delete_" + alias, Title: title, Description: "Permanently delete a " + cfg.Name + " record.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}, Annotations: &protocol.ToolAnnotations{Title: title, DestructiveHint: true, IdempotentHint: true}})
		}
	}
	return tools
}

func baseSearchPropsGeneric() map[string]any {
	return map[string]any{
		"query":   map[string]any{"type": "string", "description": "Case-insensitive search text. See describe_collection for searchable fields."},
		"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 20},
		"page":    map[string]any{"type": "integer", "minimum": 1, "default": 1},
		"sort":    map[string]any{"type": "string", "description": "Comma-separated sort fields, prefix with - for descending. See describe_collection."},
		"filters": map[string]any{"type": "object", "additionalProperties": true, "description": "Field filters keyed by filter argument name; call describe_collection for the filters of a collection."},
	}
}

func describeAccessToolDefinition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name: "describe_access", Title: "Describe access",
		Description: "List caller-visible collections, per-operation availability, limits and the manifest revision.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		Annotations: readOnlyAnnotations("Describe access"),
	}
}

func describeToolDefinition(names []string) protocol.ToolDefinition {
	collectionProp := map[string]any{"type": "string", "description": "Collection to describe. Omit to list all collections."}
	if len(names) > 0 {
		collectionProp["enum"] = names
	}
	return protocol.ToolDefinition{
		Name: "describe_collection", Title: "Describe collection",
		Description: "List the available collections, or describe one collection's fields, filters, sort fields and relations.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"collection": collectionProp}, "additionalProperties": false},
		Annotations: readOnlyAnnotations("Describe collection"),
	}
}

func searchOutputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"collection": map[string]any{"type": "string"},
		"results":    map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "collection": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "url": map[string]any{"type": "string"}, "updated": map[string]any{"type": "string"}, "fields": map[string]any{"type": "object"}, "related": map[string]any{"type": "object"}}, "required": []string{"id", "collection", "title"}}},
		"page":       map[string]any{"type": "integer"}, "perPage": map[string]any{"type": "integer"},
		"totalItems": map[string]any{"type": "integer"}, "totalPages": map[string]any{"type": "integer"},
		"hasMore": map[string]any{"type": "boolean"},
	}, "required": []string{"results", "page", "perPage", "hasMore"}}
}

func fetchOutputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"id": map[string]any{"type": "string"}, "collection": map[string]any{"type": "string"},
		"title": map[string]any{"type": "string"}, "url": map[string]any{"type": "string"},
		"record": map[string]any{"type": "object"},
	}, "required": []string{"id", "collection", "title", "record"}}
}

func isWriteOp(op string) bool { return op == "create" || op == "update" || op == "delete" }

// --- tools/call ---

func (s *Server) handleToolCall(w http.ResponseWriter, r *http.Request, p Principal, id any, params json.RawMessage) {
	var call protocol.ToolCallParams
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		writeJSONRPCError(w, http.StatusOK, id, protocol.CodeInvalidParams, "Invalid params", "expected {name, arguments}")
		return
	}
	if call.Arguments == nil {
		call.Arguments = map[string]any{}
	}
	ctx := r.Context()
	base := s.baseURL(r)
	toolErr := func(msg string) { writeToolError(w, id, msg) }
	dispatchErr := func(err error) {
		if msg, ok := UserMessage(err); ok {
			toolErr(msg)
			return
		}
		writeToolError(w, id, "Internal error while processing the request.")
	}
	if call.Name == "describe_access" {
		s.handleDescribeAccess(w, p, id)
		return
	}
	if call.Name == "describe_collection" {
		s.handleDescribe(w, p, id, call.Arguments)
		return
	}
	for _, ct := range s.cfg.CustomTools {
		if ct.Name == call.Name {
			if !s.customAllowed(p, ct) {
				s.emitAudit(p, call.Name, ct.effectKind(), "", "", "denied", time.Now(), id, nil)
				toolErr("Tool is unavailable.")
				return
			}
			s.callCustom(w, r, p, id, ct, call.Arguments)
			return
		}
	}
	dispatch := func(cfg *CollectionConfig, op string) {
		start := time.Now()
		outcome := "error"
		defer func() {
			s.emitAudit(p, call.Name, "", cfg.Name, op, outcome, start, id, s.auditPayload(call.Arguments))
		}()

		if ok, msg := s.canInvoke(p, cfg, op); !ok {
			outcome = "denied"
			toolErr(msg)
			return
		}
		var out any
		var err error
		switch op {
		case "search":
			out, err = s.host.Search(ctx, p, cfg, call.Arguments, base)
		case "fetch":
			out, err = s.host.Fetch(ctx, p, cfg, call.Arguments, base)
		default:
			out, err = s.host.Write(ctx, p, op, cfg, call.Arguments, base)
		}
		if err != nil {
			outcome = AuditOutcome(err)
			dispatchErr(err)
			return
		}
		outcome = "ok"
		writeToolResult(w, id, out)
	}
	if s.toolMode() == "generic" {
		switch call.Name {
		case "search", "fetch", "create", "update", "delete":
			cfg, ok := s.host.LookupCollection(strings.TrimSpace(getStringArg(call.Arguments, "collection")))
			if !ok {
				toolErr("Unknown or unavailable collection. Call describe_access to list collections.")
				return
			}
			dispatch(cfg, call.Name)
			return
		}
	} else {
		for _, op := range []string{"search", "fetch", "create", "update", "delete"} {
			if name, ok := strings.CutPrefix(call.Name, op+"_"); ok {
				if cfg, ok2 := s.host.LookupCollection(name); ok2 {
					dispatch(cfg, op)
					return
				}
				break
			}
		}
	}
	writeJSONRPCError(w, http.StatusOK, id, protocol.CodeInvalidParams, "Unknown tool", call.Name)
}

func (s *Server) operationsMap(p Principal, cfg *CollectionConfig) map[string]any {
	out := map[string]any{}
	for _, op := range []string{"describe", "search", "fetch", "create", "update", "delete"} {
		st, reason := s.opState(p, cfg, op)
		// describe op state mirrors fetch/view but is available if collection is caller-visible and describe declared
		if op == "describe" && cfg.HasOperation("describe") && s.policyAllows(p, cfg) {
			st, reason = "available", "collection is visible to the caller"
			if cfg.RuleFor("fetch") == RuleConditional {
				st, reason = "conditional", "PocketBase rule applies to each request or record"
			}
		}
		out[op] = map[string]any{"state": st, "reason": reason}
	}
	return out
}

func (s *Server) handleDescribeAccess(w http.ResponseWriter, p Principal, id any) {
	cols := []map[string]any{}
	for _, cfg := range s.callerVisibleCollections(p) {
		cols = append(cols, map[string]any{
			"collection": cfg.Name, "alias": cfg.Alias, "description": cfg.Description,
			"operations": s.operationsMap(p, cfg),
			"limits":     map[string]any{"defaultLimit": cfg.DefaultLimit, "maxLimit": cfg.MaxLimit},
		})
	}
	custom := []string{}
	for _, ct := range s.cfg.CustomTools {
		if !s.customAllowed(p, ct) {
			continue
		}
		custom = append(custom, ct.Name)
	}
	writeToolResult(w, id, map[string]any{
		"manifestRevision": s.manifest().Revision(),
		"toolMode":         s.toolMode(),
		"allowWrites":      s.cfg.AllowWrites,
		"collections":      cols,
		"customTools":      custom,
	})
}

func (s *Server) handleDescribe(w http.ResponseWriter, p Principal, id any, args map[string]any) {
	name := strings.TrimSpace(getStringArg(args, "collection"))
	if name == "" {
		list := []map[string]any{}
		for _, cfg := range s.callerVisibleCollections(p) {
			list = append(list, map[string]any{"name": cfg.Name, "alias": cfg.Alias, "description": cfg.Description, "searchFields": cfg.SearchFields, "expand": cfg.Expand})
		}
		writeToolResult(w, id, map[string]any{"collections": list})
		return
	}
	cfg, ok := s.host.LookupCollection(name)
	if !ok || !s.hostVisible(p, cfg) || !s.policyAllows(p, cfg) {
		writeToolError(w, id, "Unknown or unavailable collection \""+name+"\".")
		return
	}
	if cfg.HasOperation("describe") == false {
		// describe_collection itself requires the describe operation when declared set is restrictive;
		// collections without describe are treated as unavailable for detailed description
		// (list-level info remains in describe_access). Fall through only if no operations declared (legacy fake configs).
		if len(cfg.Operations) > 0 {
			writeToolError(w, id, "Unknown or unavailable collection \""+name+"\".")
			return
		}
	}
	fieldDesc := func(names []string) []map[string]any {
		out := []map[string]any{}
		for _, n := range names {
			f, ok := cfg.FieldMap[n]
			if !ok {
				out = append(out, map[string]any{"name": n, "type": "text"})
				continue
			}
			e := map[string]any{"name": f.Name, "type": f.Type}
			if len(f.Values) > 0 {
				e["values"] = f.Values
			}
			if f.Multiple {
				e["multiple"] = true
			}
			if f.RelCollection != "" {
				e["relatedCollection"] = f.RelCollection
			}
			out = append(out, e)
		}
		return out
	}
	filters := []map[string]any{}
	for _, f := range FiltersFor(cfg) {
		filters = append(filters, map[string]any{"name": f.Argument, "field": f.Field, "operator": f.Operator, "match": f.MatchDescription()})
	}
	relations := []map[string]any{}
	for _, name := range cfg.Expand {
		if rel, ok := cfg.Relations[name]; ok {
			relations = append(relations, map[string]any{"field": name, "target": rel.Target, "fields": rel.Fields, "maxDepth": rel.MaxDepth})
		} else {
			relations = append(relations, map[string]any{"field": name})
		}
	}
	out := map[string]any{
		"name": cfg.Name, "alias": cfg.Alias, "description": cfg.Description,
		"operations": s.operationsMap(p, cfg),
		"fields":     map[string]any{"read": fieldDesc(cfg.ReadFields), "create": fieldDesc(cfg.CreateFields), "update": fieldDesc(cfg.UpdateFields)},
		// legacy convenience keys
		"writableFields": cfg.CreateFields,
		"filters":        filters, "searchFields": cfg.SearchFields, "sortFields": cfg.SortFields,
		"defaultSort": cfg.DefaultSort, "expand": cfg.Expand, "relations": relations,
		"limits": map[string]any{"defaultLimit": cfg.DefaultLimit, "maxLimit": cfg.MaxLimit},
	}
	// also expose flat fields list for compatibility
	out["fieldList"] = fieldDesc(cfg.ReadFields)
	writeToolResult(w, id, out)
}

// --- helpers ---

func getStringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func writeJSONRPCResult(w http.ResponseWriter, id any, result any) {
	writeJSONRPC(w, http.StatusOK, protocol.Response{JSONRPC: "2.0", ID: id, Result: result})
}

func writeJSONRPCError(w http.ResponseWriter, status int, id any, code int, message string, data any) {
	writeJSONRPC(w, status, protocol.Response{JSONRPC: "2.0", ID: id, Error: &protocol.Error{Code: code, Message: message, Data: data}})
}

func writeJSONRPC(w http.ResponseWriter, status int, payload protocol.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writePlainError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"message": msg})
}

func writeToolResult(w http.ResponseWriter, id any, data any) {
	text, err := json.Marshal(data)
	if err != nil {
		writeToolError(w, id, "Internal error while processing the request.")
		return
	}
	writeJSONRPCResult(w, id, protocol.ToolResult{Content: []any{protocol.ToolContent{Type: "text", Text: string(text)}}, StructuredContent: data})
}

func writeToolError(w http.ResponseWriter, id any, message string) {
	writeJSONRPCResult(w, id, protocol.ToolResult{Content: []any{protocol.ToolContent{Type: "text", Text: message}}, IsError: true})
}

// IsWritableField reports whether a field may be set via write tools.
func IsWritableField(f FieldInfo) bool {
	switch f.Type {
	case "file", "autodate", "password":
		return false
	}
	return true
}

// ContextWithPrincipal / PrincipalFromContext helpers for custom tools.
type principalCtxKey struct{}

func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}

var _ = reservedArgs
