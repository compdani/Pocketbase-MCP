package pocketbasemcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
	"github.com/compdani/pocketbase-mcp/protocol"
)

// The two goldens below are the JSON-normalized tools array of the MCP
// tools/list response produced by the pre-refactor implementation for the
// fixture in newGoldenServer. They were captured before toolList was split into
// listTools/callTool and must keep matching byte for byte: the refactor may not
// change what an MCP client sees.
const goldenPerCollectionTools = `[{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Describe access"},"description":"List caller-visible collections, per-operation availability, limits and the manifest revision.","inputSchema":{"additionalProperties":false,"properties":{},"type":"object"},"name":"describe_access","title":"Describe access"},{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Describe collection"},"description":"List the available collections, or describe one collection's fields, filters, sort fields and relations.","inputSchema":{"additionalProperties":false,"properties":{"collection":{"description":"Collection to describe. Omit to list all collections.","enum":["deals","contacts"],"type":"string"}},"type":"object"},"name":"describe_collection","title":"Describe collection"},{"annotations":{"destructiveHint":false,"idempotentHint":false,"openWorldHint":false,"readOnlyHint":true,"title":"digest"},"description":"read digest","inputSchema":{"type":"object"},"name":"digest"},{"annotations":{"destructiveHint":false,"idempotentHint":false,"openWorldHint":false,"readOnlyHint":false,"title":"publish"},"description":"write thing","inputSchema":{"type":"object"},"name":"publish"},{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Search deals"},"description":"Search or list deals records by free text and field filters. Deals Returns summaries with paging info; use fetch_deals for full records.","inputSchema":{"additionalProperties":false,"properties":{"limit":{"default":10,"description":"Results per page.","maximum":40,"minimum":1,"type":"integer"},"name":{"description":"name contains this text.","type":"string"},"page":{"default":1,"description":"1-based page number.","minimum":1,"type":"integer"},"query":{"description":"Case-insensitive text matched against: name. Omit to list records.","type":"string"},"sort":{"description":"Comma-separated sort fields, prefix with - for descending. Allowed: name, id. Default: id.","type":"string"}},"type":"object"},"name":"search_deals","outputSchema":{"properties":{"collection":{"type":"string"},"hasMore":{"type":"boolean"},"page":{"type":"integer"},"perPage":{"type":"integer"},"results":{"items":{"properties":{"collection":{"type":"string"},"fields":{"type":"object"},"id":{"type":"string"},"related":{"type":"object"},"title":{"type":"string"},"updated":{"type":"string"},"url":{"type":"string"}},"required":["id","collection","title"],"type":"object"},"type":"array"},"totalItems":{"type":"integer"},"totalPages":{"type":"integer"}},"required":["results","page","perPage","hasMore"],"type":"object"},"title":"Search deals"},{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Fetch deals"},"description":"Fetch one full deals record by id.","inputSchema":{"additionalProperties":false,"properties":{"id":{"description":"Record id.","type":"string"}},"required":["id"],"type":"object"},"name":"fetch_deals","outputSchema":{"properties":{"collection":{"type":"string"},"id":{"type":"string"},"record":{"type":"object"},"title":{"type":"string"},"url":{"type":"string"}},"required":["id","collection","title","record"],"type":"object"},"title":"Fetch deals"},{"annotations":{"destructiveHint":true,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":false,"title":"Update deals"},"description":"Update a deals record. Collection rules, validation and hooks apply.","inputSchema":{"additionalProperties":false,"properties":{"data":{"additionalProperties":false,"description":"Field values keyed by field name. Relations take record ids. File fields cannot be set through MCP.","properties":{"name":{"description":"name","type":"string"}},"type":"object"},"id":{"type":"string"}},"required":["id","data"],"type":"object"},"name":"update_deals","outputSchema":{"properties":{"collection":{"type":"string"},"id":{"type":"string"},"record":{"type":"object"},"title":{"type":"string"},"url":{"type":"string"}},"required":["id","collection","title","record"],"type":"object"},"title":"Update deals"},{"annotations":{"destructiveHint":true,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":false,"title":"Delete deals"},"description":"Permanently delete a deals record.","inputSchema":{"additionalProperties":false,"properties":{"id":{"type":"string"}},"required":["id"],"type":"object"},"name":"delete_deals","title":"Delete deals"},{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Search contacts"},"description":"Search or list contacts records by free text and field filters. Contacts Returns summaries with paging info; use fetch_people for full records.","inputSchema":{"additionalProperties":false,"properties":{"limit":{"default":5,"description":"Results per page.","maximum":10,"minimum":1,"type":"integer"},"page":{"default":1,"description":"1-based page number.","minimum":1,"type":"integer"},"query":{"description":"Case-insensitive text matched against: email. Omit to list records.","type":"string"},"sort":{"description":"Comma-separated sort fields, prefix with - for descending. Allowed: id. Default: id.","type":"string"}},"type":"object"},"name":"search_people","outputSchema":{"properties":{"collection":{"type":"string"},"hasMore":{"type":"boolean"},"page":{"type":"integer"},"perPage":{"type":"integer"},"results":{"items":{"properties":{"collection":{"type":"string"},"fields":{"type":"object"},"id":{"type":"string"},"related":{"type":"object"},"title":{"type":"string"},"updated":{"type":"string"},"url":{"type":"string"}},"required":["id","collection","title"],"type":"object"},"type":"array"},"totalItems":{"type":"integer"},"totalPages":{"type":"integer"}},"required":["results","page","perPage","hasMore"],"type":"object"},"title":"Search contacts"},{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Fetch contacts"},"description":"Fetch one full contacts record by id.","inputSchema":{"additionalProperties":false,"properties":{"id":{"description":"Record id.","type":"string"}},"required":["id"],"type":"object"},"name":"fetch_people","outputSchema":{"properties":{"collection":{"type":"string"},"id":{"type":"string"},"record":{"type":"object"},"title":{"type":"string"},"url":{"type":"string"}},"required":["id","collection","title","record"],"type":"object"},"title":"Fetch contacts"}]`

const goldenGenericTools = `[{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Describe access"},"description":"List caller-visible collections, per-operation availability, limits and the manifest revision.","inputSchema":{"additionalProperties":false,"properties":{},"type":"object"},"name":"describe_access","title":"Describe access"},{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Describe collection"},"description":"List the available collections, or describe one collection's fields, filters, sort fields and relations.","inputSchema":{"additionalProperties":false,"properties":{"collection":{"description":"Collection to describe. Omit to list all collections.","enum":["deals","contacts"],"type":"string"}},"type":"object"},"name":"describe_collection","title":"Describe collection"},{"annotations":{"destructiveHint":false,"idempotentHint":false,"openWorldHint":false,"readOnlyHint":true,"title":"digest"},"description":"read digest","inputSchema":{"type":"object"},"name":"digest"},{"annotations":{"destructiveHint":false,"idempotentHint":false,"openWorldHint":false,"readOnlyHint":false,"title":"publish"},"description":"write thing","inputSchema":{"type":"object"},"name":"publish"},{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Search records"},"description":"Search or list records of a collection by free text and field filters. Returns summaries with paging info; use fetch for full records.","inputSchema":{"additionalProperties":false,"properties":{"collection":{"description":"Collection to use.","enum":["deals","contacts"],"type":"string"},"filters":{"additionalProperties":true,"description":"Field filters keyed by filter argument name; call describe_collection for the filters of a collection.","type":"object"},"limit":{"default":20,"maximum":50,"minimum":1,"type":"integer"},"page":{"default":1,"minimum":1,"type":"integer"},"query":{"description":"Case-insensitive search text. See describe_collection for searchable fields.","type":"string"},"sort":{"description":"Comma-separated sort fields, prefix with - for descending. See describe_collection.","type":"string"}},"required":["collection"],"type":"object"},"name":"search","outputSchema":{"properties":{"collection":{"type":"string"},"hasMore":{"type":"boolean"},"page":{"type":"integer"},"perPage":{"type":"integer"},"results":{"items":{"properties":{"collection":{"type":"string"},"fields":{"type":"object"},"id":{"type":"string"},"related":{"type":"object"},"title":{"type":"string"},"updated":{"type":"string"},"url":{"type":"string"}},"required":["id","collection","title"],"type":"object"},"type":"array"},"totalItems":{"type":"integer"},"totalPages":{"type":"integer"}},"required":["results","page","perPage","hasMore"],"type":"object"},"title":"Search records"},{"annotations":{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"Fetch record"},"description":"Fetch one full record by collection and id, with configured relations expanded.","inputSchema":{"additionalProperties":false,"properties":{"collection":{"description":"Collection to use.","enum":["deals","contacts"],"type":"string"},"id":{"description":"Record id.","type":"string"}},"required":["collection","id"],"type":"object"},"name":"fetch","outputSchema":{"properties":{"collection":{"type":"string"},"id":{"type":"string"},"record":{"type":"object"},"title":{"type":"string"},"url":{"type":"string"}},"required":["id","collection","title","record"],"type":"object"},"title":"Fetch record"},{"annotations":{"destructiveHint":true,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":false,"title":"Update record"},"description":"Update fields of an existing record. Collection rules, validation and hooks apply.","inputSchema":{"additionalProperties":false,"properties":{"collection":{"enum":["deals"],"type":"string"},"data":{"additionalProperties":true,"type":"object"},"id":{"type":"string"}},"required":["collection","id","data"],"type":"object"},"name":"update","outputSchema":{"properties":{"collection":{"type":"string"},"id":{"type":"string"},"record":{"type":"object"},"title":{"type":"string"},"url":{"type":"string"}},"required":["id","collection","title","record"],"type":"object"},"title":"Update record"},{"annotations":{"destructiveHint":true,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":false,"title":"Delete record"},"description":"Permanently delete a record. Collection rules and relation cascade settings apply.","inputSchema":{"additionalProperties":false,"properties":{"collection":{"enum":["deals"],"type":"string"},"id":{"type":"string"}},"required":["collection","id"],"type":"object"},"name":"delete","title":"Delete record"}]`

// --- shared golden fixture (the config the goldens were captured from) ---

type goldenHost struct {
	cols map[string]*CollectionConfig
}

func (h *goldenHost) Authenticate(token string) (Principal, error) {
	if token == "good" {
		return Principal{ID: "u1", CollectionName: "users", CollectionID: "col_users"}, nil
	}
	return Principal{}, ToolError("bad token")
}

func (h *goldenHost) LookupCollection(n string) (*CollectionConfig, bool) {
	for _, c := range h.cols {
		if c.Name == n || c.Alias == n {
			return c, true
		}
	}
	return nil, false
}

func (h *goldenHost) VisibleCollections(Principal) []*CollectionConfig {
	out := []*CollectionConfig{}
	for _, k := range []string{"deals", "contacts"} {
		if c, ok := h.cols[k]; ok {
			out = append(out, c)
		}
	}
	return out
}

func (h *goldenHost) AllCollections() []*CollectionConfig { return h.VisibleCollections(Principal{}) }

func (h *goldenHost) Search(_ context.Context, _ Principal, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	return map[string]any{"collection": cfg.Name}, nil
}

func (h *goldenHost) Fetch(_ context.Context, _ Principal, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	return map[string]any{"collection": cfg.Name}, nil
}

func (h *goldenHost) Write(_ context.Context, _ Principal, _ string, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	return map[string]any{"collection": cfg.Name}, nil
}

func newGoldenServer(toolMode string) *Server {
	deals := &CollectionConfig{
		Name: "deals", Alias: "deals", Description: "Deals",
		Operations:   []string{"describe", "search", "fetch", "create", "update", "delete"},
		Policy:       "authenticated",
		FieldMap:     map[string]FieldInfo{"name": {Name: "name", Type: "text"}},
		SearchFields: []string{"name"}, SortFields: []string{"name", "id"},
		ReadFields: []string{"name"}, CreateFields: []string{"name"}, UpdateFields: []string{"name"},
		DefaultSort: []string{"id"}, DefaultLimit: 10, MaxLimit: 40,
		Filters:  []ResolvedFilter{{Argument: "name", Field: "name", Operator: "contains"}},
		ListRule: RulePublic, ViewRule: RulePublic,
		CreateRule: RuleNil, UpdateRule: RuleConditional, DeleteRule: RuleConditional,
	}
	contacts := &CollectionConfig{
		Name: "contacts", Alias: "people", Description: "Contacts",
		Operations:   []string{"describe", "search", "fetch"},
		Policy:       "authenticated",
		FieldMap:     map[string]FieldInfo{"email": {Name: "email", Type: "email"}},
		SearchFields: []string{"email"}, SortFields: []string{"id"},
		ReadFields: []string{"email"}, DefaultSort: []string{"id"}, DefaultLimit: 5, MaxLimit: 10,
		ListRule: RulePublic, ViewRule: RuleConditional,
	}
	m := &Manifest{SchemaVersion: "1", ToolMode: toolMode, Collections: []ManifestCollection{
		{Collection: "deals", Operations: deals.Operations, Policy: "authenticated"},
		{Collection: "contacts", Alias: "people", Operations: contacts.Operations, Policy: "authenticated"},
	}}
	return New(Config{
		Manifest:    m,
		AllowWrites: true,
		Auth:        AuthConfig{Collections: []string{"users"}},
		CustomTools: []ToolSpec{
			{Name: "digest", Description: "read digest", InputSchema: map[string]any{"type": "object"}, Handler: okHandler},
			{Name: "publish", Description: "write thing", Write: true, InputSchema: map[string]any{"type": "object"}, Handler: okHandler},
		},
	}, &goldenHost{cols: map[string]*CollectionConfig{"deals": deals, "contacts": contacts}})
}

// normalizedToolJSON renders a tool list independently of Go struct field order,
// so the comparison is against the captured JSON value, not its encoding order.
func normalizedToolJSON(t *testing.T, srv *Server, p Principal, channel string) string {
	t.Helper()
	b, err := json.Marshal(srv.listTools(p, channel))
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("normalize tools: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("remarshal tools: %v", err)
	}
	return string(out)
}

func TestMCPToolListUnchangedByRefactor(t *testing.T) {
	p := Principal{ID: "u1", CollectionName: "users", CollectionID: "col_users"}
	cases := []struct{ mode, golden string }{
		{"per_collection", goldenPerCollectionTools},
		{"generic", goldenGenericTools},
	}
	for _, c := range cases {
		srv := newGoldenServer(c.mode)
		if got := normalizedToolJSON(t, srv, p, "mcp"); got != c.golden {
			t.Errorf("%s tools/list changed.\n got: %s\nwant: %s", c.mode, got, c.golden)
		}
		// The same list must come out of the HTTP surface unchanged.
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer good")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var resp struct {
			Result struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: decode tools/list: %v", c.mode, err)
		}
		var v any
		if err := json.Unmarshal(resp.Result.Tools, &v); err != nil {
			t.Fatalf("%s: decode tools array: %v", c.mode, err)
		}
		normalized, _ := json.Marshal(v)
		if string(normalized) != c.golden {
			t.Errorf("%s: HTTP tools/list changed.\n got: %s\nwant: %s", c.mode, normalized, c.golden)
		}
	}
}

// --- chat fixture ---

type auditRecorder struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (r *auditRecorder) add(e AuditEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *auditRecorder) snapshot() []AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AuditEvent(nil), r.events...)
}

func (r *auditRecorder) last(t *testing.T) AuditEvent {
	t.Helper()
	events := r.snapshot()
	if len(events) == 0 {
		t.Fatal("no audit events recorded")
	}
	return events[len(events)-1]
}

type toolCounter struct {
	mu sync.Mutex
	n  int
}

func (c *toolCounter) handler(context.Context, CallContext, map[string]any) (any, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return map[string]any{"ok": true}, nil
}

func (c *toolCounter) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// chatTestHost is a configurable Host: it can hide collections, revoke tokens
// and count writes, which is what the shared-gate tests need.
type chatTestHost struct {
	mu         sync.Mutex
	cols       []*CollectionConfig
	principals map[string]Principal
	revoked    map[string]bool
	hidden     map[string]bool
	writes     int
	writeOps   []string
	searches   int
	visible    int
}

func (h *chatTestHost) Authenticate(token string) (Principal, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.revoked[token] {
		return Principal{}, ToolError("token revoked")
	}
	p, ok := h.principals[token]
	if !ok {
		return Principal{}, ToolError("bad token")
	}
	return p, nil
}

func (h *chatTestHost) LookupCollection(n string) (*CollectionConfig, bool) {
	for _, c := range h.cols {
		if c.Name == n || c.Alias == n {
			return c, true
		}
	}
	return nil, false
}

func (h *chatTestHost) VisibleCollections(Principal) []*CollectionConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.visible++
	out := []*CollectionConfig{}
	for _, c := range h.cols {
		if !h.hidden[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

func (h *chatTestHost) AllCollections() []*CollectionConfig { return h.cols }

func (h *chatTestHost) Search(_ context.Context, _ Principal, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	h.mu.Lock()
	h.searches++
	h.mu.Unlock()
	return map[string]any{"collection": cfg.Name, "results": []any{}, "page": 1, "perPage": 20, "totalItems": 0, "totalPages": 0, "hasMore": false}, nil
}

func (h *chatTestHost) Fetch(_ context.Context, _ Principal, cfg *CollectionConfig, args map[string]any, _ string) (any, error) {
	if getStringArg(args, "id") == "hidden" {
		// The host's own rule hides this record; the message must be the same
		// on both surfaces.
		return nil, NotFound("Record not found.")
	}
	return map[string]any{"id": "r1", "collection": cfg.Name, "title": "t", "record": map[string]any{}}, nil
}

func (h *chatTestHost) Write(_ context.Context, _ Principal, operation string, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	h.mu.Lock()
	h.writes++
	h.writeOps = append(h.writeOps, operation)
	h.mu.Unlock()
	return map[string]any{"id": "r1", "collection": cfg.Name}, nil
}

func (h *chatTestHost) setRevoked(token string) {
	h.mu.Lock()
	h.revoked[token] = true
	h.mu.Unlock()
}

func (h *chatTestHost) hide(name string) {
	h.mu.Lock()
	h.hidden[name] = true
	h.mu.Unlock()
}

func (h *chatTestHost) writeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.writes
}

func newChatTestHost() *chatTestHost {
	deals := &CollectionConfig{
		Name: "deals", Alias: "deals",
		Operations: []string{"describe", "search", "fetch", "create", "update", "delete"},
		Policy:     "authenticated",
		FieldMap:   map[string]FieldInfo{},
		SortFields: []string{"id"},
		ListRule:   RulePublic, ViewRule: RulePublic,
		CreateRule: RulePublic, UpdateRule: RulePublic, DeleteRule: RulePublic,
		DefaultLimit: 20, MaxLimit: 50,
	}
	secrets := &CollectionConfig{
		Name: "secrets", Alias: "secrets",
		Operations: []string{"describe", "search", "fetch"},
		Policy:     "field:role:admin",
		FieldMap:   map[string]FieldInfo{},
		SortFields: []string{"id"},
		ListRule:   RulePublic, ViewRule: RulePublic,
		DefaultLimit: 20, MaxLimit: 50,
	}
	return &chatTestHost{
		cols:    []*CollectionConfig{deals, secrets},
		revoked: map[string]bool{},
		hidden:  map[string]bool{},
		principals: map[string]Principal{
			"good":  {ID: "u1", CollectionName: "users", CollectionID: "col_users"},
			"admin": {ID: "u3", CollectionName: "users", CollectionID: "col_users", RecordValues: map[string][]string{"role": {"admin"}}},
			"super": {ID: "s1", CollectionName: "_superusers", CollectionID: "col_superusers", IsSuperuser: true},
		},
	}
}

func newChatTestServer(h Host, audit *auditRecorder, cfg Config) *Server {
	m := &Manifest{SchemaVersion: "1", ToolMode: "per_collection", Collections: []ManifestCollection{
		{Collection: "deals", Operations: []string{"describe", "search", "fetch", "create", "update", "delete"}, Policy: "authenticated"},
		{Collection: "secrets", Operations: []string{"describe", "search", "fetch"}, Policy: "field:role:admin"},
	}}
	cfg.Manifest = m
	cfg.AllowWrites = true
	cfg.Auth = AuthConfig{Collections: []string{"users"}}
	if audit != nil {
		cfg.AuditFunc = audit.add
	}
	cfg.CustomTools = []ToolSpec{
		{Name: "chat_digest", Description: "chat read", Channels: "chat", InputSchema: map[string]any{"type": "object"}, Handler: okHandler},
		{Name: "both_digest", Description: "shared read", Channels: "both", InputSchema: map[string]any{"type": "object"}, Handler: okHandler},
		{Name: "mcp_digest", Description: "mcp read", Channels: "mcp", InputSchema: map[string]any{"type": "object"}, Handler: okHandler},
		{Name: "both_writer", Description: "shared write", Channels: "both", Write: true, InputSchema: map[string]any{"type": "object"}, Handler: okHandler},
	}
	return New(cfg, h)
}

func chatPrincipal(h *chatTestHost, token string) Principal {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.principals[token]
	p.Token = token
	return p
}

func chatExecution(t *testing.T, srv *Server, p Principal) chat.ToolExecutor {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/chat/v1/conversations", nil)
	return srv.ChatCaller(p, req, "tests").Tools
}

func providerToolNames(t *testing.T, defs []provider.ToolDef) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, d := range defs {
		out[d.Name] = true
	}
	return out
}

func listNames(defs []protocol.ToolDefinition) map[string]bool {
	out := map[string]bool{}
	for _, d := range defs {
		out[d.Name] = true
	}
	return out
}

func TestChatToolListIsReadOnlySubset(t *testing.T) {
	host := newChatTestHost()
	srv := newChatTestServer(host, nil, Config{})
	p := chatPrincipal(host, "good")
	exec := chatExecution(t, srv, p)

	defs, err := exec.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	got := providerToolNames(t, defs)
	for _, want := range []string{"describe_access", "describe_collection", "search_deals", "fetch_deals", "chat_digest", "both_digest"} {
		if !got[want] {
			t.Errorf("chat tools missing %q: %v", want, got)
		}
	}
	for _, absent := range []string{"create_deals", "update_deals", "delete_deals", "search_secrets", "fetch_secrets", "mcp_digest", "both_writer"} {
		if got[absent] {
			t.Errorf("chat tools must not include %q: %v", absent, got)
		}
	}

	mcp := listNames(srv.listTools(p, "mcp"))
	for _, want := range []string{"create_deals", "update_deals", "delete_deals", "mcp_digest", "both_writer", "both_digest"} {
		if !mcp[want] {
			t.Errorf("mcp tools missing %q", want)
		}
	}
	if mcp["chat_digest"] {
		t.Error("mcp tools must not include a chat-only tool")
	}

	// A chat-only tool is not just unlisted over MCP: calling it by name is
	// refused exactly like a hidden tool.
	_, out := rpc(t, srv, "good", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"chat_digest","arguments":{}}}`)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("chat-only tool call over MCP must be a tool error: %v", out)
	}
	if text := toolResultTextOf(res); text != "Tool is unavailable." {
		t.Fatalf("refusal text = %q", text)
	}
}

func toolResultTextOf(result map[string]any) string {
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	part, _ := content[0].(map[string]any)
	text, _ := part["text"].(string)
	return text
}

func TestChatAndMCPSharePolicyGates(t *testing.T) {
	host := newChatTestHost()
	srv := newChatTestServer(host, nil, Config{})
	p := chatPrincipal(host, "good")

	// A collection the policy hides is absent from both lists.
	for _, channel := range []string{"mcp", "chat"} {
		for name := range listNames(srv.listTools(p, channel)) {
			if strings.Contains(name, "secrets") {
				t.Errorf("%s list leaks policy-denied collection tool %q", channel, name)
			}
		}
	}

	exec := chatExecution(t, srv, p)
	if _, err := exec.Call(context.Background(), "search_secrets", json.RawMessage(`{}`)); err == nil {
		t.Fatal("chat allowed a policy-denied collection")
	} else {
		var te chat.ToolError
		if !errors.As(err, &te) || te.Message != "Tool is unavailable." {
			t.Fatalf("chat refusal = %v, want Tool is unavailable.", err)
		}
	}
	_, out := rpc(t, srv, "good", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_secrets","arguments":{}}}`)
	if res, _ := out["result"].(map[string]any); res == nil || res["isError"] != true {
		t.Fatalf("MCP allowed a policy-denied collection: %v", out)
	}

	// A record the host hides yields the identical message on both surfaces.
	_, out = rpc(t, srv, "good", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fetch_deals","arguments":{"id":"hidden"}}}`)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("MCP hidden record = %v", out)
	}
	mcpMsg := toolResultTextOf(res)
	_, err := exec.Call(context.Background(), "fetch_deals", json.RawMessage(`{"id":"hidden"}`))
	var te chat.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("chat hidden record = %v, want a tool error", err)
	}
	if te.Message != mcpMsg || mcpMsg != "Record not found." {
		t.Fatalf("messages differ: mcp=%q chat=%q", mcpMsg, te.Message)
	}
}

func TestChatRefusesWriteToolsAndNeverWrites(t *testing.T) {
	host := newChatTestHost()
	var counter toolCounter
	srv := newChatTestServer(host, nil, Config{})
	// Replace the both_writer handler with one that counts invocations.
	for i := range srv.cfg.CustomTools {
		if srv.cfg.CustomTools[i].Name == "both_writer" {
			srv.cfg.CustomTools[i].Handler = counter.handler
		}
	}
	p := chatPrincipal(host, "good")
	exec := chatExecution(t, srv, p)

	for _, name := range []string{"create_deals", "both_writer"} {
		_, err := exec.Call(context.Background(), name, json.RawMessage(`{"data":{}}`))
		var te chat.ToolError
		if !errors.As(err, &te) || te.Message != "Tool is unavailable." {
			t.Fatalf("%s through chat = %v, want Tool is unavailable.", name, err)
		}
	}
	if host.writeCount() != 0 {
		t.Fatalf("host Write called %d times through chat", host.writeCount())
	}
	if counter.calls() != 0 {
		t.Fatalf("write custom tool handler called through chat %d times", counter.calls())
	}

	// The same write succeeds through MCP, proving the gate (not the host) is
	// what refused the chat call.
	_, out := rpc(t, srv, "good", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_deals","arguments":{"data":{}}}}`)
	if res, _ := out["result"].(map[string]any); res == nil || res["isError"] == true {
		t.Fatalf("MCP create failed: %v", out)
	}
	if host.writeCount() != 1 || host.writeOps[0] != "create" {
		t.Fatalf("MCP write = %d %v", host.writeCount(), host.writeOps)
	}
}

func TestAuditEventsCarryChannel(t *testing.T) {
	host := newChatTestHost()
	audit := &auditRecorder{}
	srv := newChatTestServer(host, audit, Config{})
	p := chatPrincipal(host, "good")

	// MCP: built-in collection tool and a shared custom tool.
	if _, out := rpc(t, srv, "good", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_deals","arguments":{}}}`); out["result"] == nil {
		t.Fatalf("mcp search failed: %v", out)
	}
	if last := audit.last(t); last.Channel != "mcp" || last.Tool != "search_deals" || last.Outcome != "ok" {
		t.Fatalf("mcp audit = %+v", last)
	}
	if _, out := rpc(t, srv, "good", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"both_digest","arguments":{}}}`); out["result"] == nil {
		t.Fatalf("mcp custom call failed: %v", out)
	}
	if last := audit.last(t); last.Channel != "mcp" || last.Tool != "both_digest" {
		t.Fatalf("mcp custom audit = %+v", last)
	}

	// Chat: through the executor, same pipeline, different channel label.
	exec := chatExecution(t, srv, p)
	if _, err := exec.Call(context.Background(), "search_deals", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("chat search: %v", err)
	}
	if last := audit.last(t); last.Channel != "chat" || last.Tool != "search_deals" || last.Outcome != "ok" {
		t.Fatalf("chat audit = %+v", last)
	}
	if _, err := exec.Call(context.Background(), "both_digest", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("chat custom call: %v", err)
	}
	if last := audit.last(t); last.Channel != "chat" || last.Tool != "both_digest" {
		t.Fatalf("chat custom audit = %+v", last)
	}

	// A denied chat call is audited as denied on the chat channel.
	if _, err := exec.Call(context.Background(), "create_deals", json.RawMessage(`{}`)); err == nil {
		t.Fatal("write call through chat succeeded")
	}
	if last := audit.last(t); last.Channel != "chat" || last.Tool != "create_deals" || last.Outcome != "denied" || last.Effect != "write" {
		t.Fatalf("chat denied audit = %+v", last)
	}
}

func TestChatExecutorRevalidatesIdentity(t *testing.T) {
	host := newChatTestHost()
	srv := newChatTestServer(host, nil, Config{})
	p := chatPrincipal(host, "good")
	exec := chatExecution(t, srv, p)

	if _, err := exec.Tools(context.Background()); err != nil {
		t.Fatalf("initial Tools: %v", err)
	}

	// A permission change is visible on the next listing, because nothing is
	// cached between calls.
	host.hide("deals")
	defs, err := exec.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools after hide: %v", err)
	}
	for name := range providerToolNames(t, defs) {
		if strings.HasSuffix(name, "_deals") {
			t.Fatalf("tools/list still offers %q after the host hid the collection", name)
		}
	}
	if _, err := exec.Call(context.Background(), "search_deals", json.RawMessage(`{}`)); err == nil {
		t.Fatal("call succeeded after the collection was hidden")
	}
	host.mu.Lock()
	host.hidden = map[string]bool{}
	host.mu.Unlock()

	// Once the host rejects the token, the very next call reports an expired
	// session and listing fails.
	host.setRevoked("good")
	_, err = exec.Call(context.Background(), "search_deals", json.RawMessage(`{}`))
	var te chat.ToolError
	if !errors.As(err, &te) || te.Message != "Your session has expired. Sign in again." {
		t.Fatalf("revoked Call = %v", err)
	}
	if _, err := exec.Tools(context.Background()); err == nil {
		t.Fatal("Tools succeeded with a revoked token")
	}
}

func TestChatHandlerOptionValidation(t *testing.T) {
	host := newChatTestHost()
	srv := newChatTestServer(host, nil, Config{})
	srv.cfg.Path = "/mcp"
	srv.cfg.AliasPaths = []string{"/alias-mcp"}
	store := chat.NewMemStore(chat.MemStoreOptions{})
	runner, err := chat.NewRunner(chat.RunnerConfig{Store: store, Provider: scripted.New(), Model: "m"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})
	base := ChatOptions{Namespace: "tests", Runner: runner, Store: store}

	cases := []struct {
		name string
		opts ChatOptions
		want string
	}{
		{"missing namespace", ChatOptions{Runner: runner, Store: store}, "namespace"},
		{"prefix equals mcp", func() ChatOptions { o := base; o.Prefix = "/mcp"; return o }(), "collides"},
		{"prefix under mcp", func() ChatOptions { o := base; o.Prefix = "/mcp/chat"; return o }(), "collides"},
		{"prefix equals alias", func() ChatOptions { o := base; o.Prefix = "/alias-mcp"; return o }(), "collides"},
		{"prefix under alias", func() ChatOptions { o := base; o.Prefix = "/alias-mcp/chat/v1"; return o }(), "collides"},
		{"unknown policy", func() ChatOptions { o := base; o.Policy = "nope"; return o }(), "unknown policy"},
	}
	for _, c := range cases {
		if _, err := srv.ChatHandler(c.opts); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want error containing %q", c.name, err, c.want)
		}
	}
	h, err := srv.ChatHandler(base)
	if err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	if h == nil {
		t.Fatal("nil handler")
	}
}

func chatHTTP(t *testing.T, h http.Handler, method, path, token, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestChatAuthenticator(t *testing.T) {
	host := newChatTestHost()
	srv := newChatTestServer(host, nil, Config{})
	store := chat.NewMemStore(chat.MemStoreOptions{})
	runner, err := chat.NewRunner(chat.RunnerConfig{Store: store, Provider: scripted.New(), Model: "m"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})
	h, err := srv.ChatHandler(ChatOptions{Namespace: "tests", Runner: runner, Store: store})
	if err != nil {
		t.Fatalf("ChatHandler: %v", err)
	}
	status := DefaultChatPrefix + "/status"

	if code, _ := chatHTTP(t, h, http.MethodGet, status, "", ""); code != http.StatusUnauthorized {
		t.Errorf("missing token: %d", code)
	}
	if code, _ := chatHTTP(t, h, http.MethodGet, status, "bad", ""); code != http.StatusUnauthorized {
		t.Errorf("invalid token: %d", code)
	}
	// A token in the URL is never accepted, even with no header.
	if code, body := chatHTTP(t, h, http.MethodGet, status+"?token=good", "", ""); code != http.StatusBadRequest || !strings.Contains(string(body), chat.CodeInvalidRequest) {
		t.Errorf("query token: %d %s", code, body)
	}
	// Superusers need the explicit opt-in.
	if code, _ := chatHTTP(t, h, http.MethodGet, status, "super", ""); code != http.StatusUnauthorized {
		t.Errorf("superuser without opt-in: %d", code)
	}
	openServer := newChatTestServer(host, nil, Config{})
	openServer.cfg.Auth.AllowSuperusers = true
	openHandler, err := openServer.ChatHandler(ChatOptions{Namespace: "tests", Runner: runner, Store: store})
	if err != nil {
		t.Fatalf("ChatHandler allowing superusers: %v", err)
	}
	if code, _ := chatHTTP(t, openHandler, http.MethodGet, status, "super", ""); code != http.StatusOK {
		t.Errorf("superuser with opt-in: %d", code)
	}
	if code, _ := chatHTTP(t, h, http.MethodGet, status, "good", ""); code != http.StatusOK {
		t.Errorf("valid token: %d", code)
	}

	// The chat policy is enforced on top of the accepted collection.
	limited, err := srv.ChatHandler(ChatOptions{Namespace: "tests", Runner: runner, Store: store, Policy: "field:role:admin"})
	if err != nil {
		t.Fatalf("ChatHandler with policy: %v", err)
	}
	if code, _ := chatHTTP(t, limited, http.MethodGet, status, "good", ""); code != http.StatusForbidden {
		t.Errorf("policy-denied caller: %d", code)
	}
	if code, _ := chatHTTP(t, limited, http.MethodGet, status, "admin", ""); code != http.StatusOK {
		t.Errorf("policy-satisfying caller: %d", code)
	}

	// The exp claim of a verified JWT is decoded for the runner's deadline.
	exp := time.Unix(1893456000, 0).UTC()
	payload, _ := json.Marshal(map[string]any{"sub": "u1", "exp": exp.Unix()})
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	host.mu.Lock()
	host.principals[token] = Principal{ID: "u1", CollectionName: "users", CollectionID: "col_users"}
	host.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, status, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	caller, err := srv.ChatAuthenticate(req, ChatOptions{Namespace: "tests"})
	if err != nil {
		t.Fatalf("ChatAuthenticate: %v", err)
	}
	if !caller.Expires.Equal(exp) {
		t.Fatalf("Expires = %v, want %v", caller.Expires, exp)
	}
	if caller.Owner.Namespace != "tests" || caller.Owner.Mount != "/mcp" || caller.Owner.AuthCollectionID != "col_users" || caller.Owner.UserID != "u1" {
		t.Fatalf("owner = %+v", caller.Owner)
	}
	// Setup can see who the caller is (TurnInput.Identity).
	p, ok := TurnPrincipal(chat.TurnInput{Identity: caller.Identity})
	if !ok || p.ID != "u1" || p.Token != token {
		t.Fatalf("TurnPrincipal = %+v ok=%v", p, ok)
	}
	if _, ok := TurnPrincipal(chat.TurnInput{}); ok {
		t.Fatal("a turn without an identity has a principal")
	}
}

func toolCallStep(call provider.ToolCall) scripted.Step {
	return scripted.Step{Chunks: []provider.Chunk{
		{Kind: provider.ChunkToolCall, ToolCall: &call},
		{Kind: provider.ChunkDone},
	}}
}

func textStep(text string) scripted.Step {
	return scripted.Step{Chunks: []provider.Chunk{
		{Kind: provider.ChunkText, Text: text},
		{Kind: provider.ChunkDone},
	}}
}

func TestChatEndToEndToolResultMatchesMCP(t *testing.T) {
	host := newChatTestHost()
	srv := newChatTestServer(host, nil, Config{})
	store := chat.NewMemStore(chat.MemStoreOptions{})
	call := provider.ToolCall{ID: "call-1", Name: "search_deals", Arguments: json.RawMessage(`{"query":"x"}`)}
	prov := scripted.New(toolCallStep(call), textStep("done"))
	runner, err := chat.NewRunner(chat.RunnerConfig{Store: store, Provider: prov, Model: "test-model"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})
	h, err := srv.ChatHandler(ChatOptions{Namespace: "tests", Runner: runner, Store: store, Models: []string{"test-model"}})
	if err != nil {
		t.Fatalf("ChatHandler: %v", err)
	}

	code, body := chatHTTP(t, h, http.MethodPost, DefaultChatPrefix+"/conversations", "good", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create conversation: %d %s", code, body)
	}
	var conv chat.Conversation
	if err := json.Unmarshal(body, &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	code, body = chatHTTP(t, h, http.MethodPost, DefaultChatPrefix+"/conversations/"+conv.ID+"/turns", "good", `{"text":"find deals"}`)
	if code != http.StatusAccepted {
		t.Fatalf("submit turn: %d %s", code, body)
	}
	var submitted struct {
		Turn chat.Turn `json:"turn"`
	}
	if err := json.Unmarshal(body, &submitted); err != nil {
		t.Fatalf("decode turn: %v", err)
	}

	owner := chat.OwnerKey{Namespace: "tests", Mount: "/mcp", AuthCollectionID: "col_users", UserID: "u1"}
	deadline := time.Now().Add(10 * time.Second)
	for {
		turn, err := store.GetTurn(context.Background(), owner, submitted.Turn.ID)
		if err == nil && turn.State.Terminal() {
			if turn.State != chat.TurnStateCompleted {
				t.Fatalf("turn ended as %s: %+v", turn.State, turn.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s did not finish: state %s err %v", submitted.Turn.ID, turn.State, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	events, err := store.EventsAfter(context.Background(), owner, submitted.Turn.ID, 0)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	var completed *chat.ToolCompleted
	for _, e := range events {
		if e.Type == chat.EventToolCompleted {
			data := chat.ToolCompleted{}
			if err := json.Unmarshal(e.Data, &data); err != nil {
				t.Fatalf("decode tool.completed: %v", err)
			}
			completed = &data
		}
	}
	if completed == nil {
		t.Fatalf("no tool.completed event in %v", events)
	}

	// What MCP returns for the same principal and arguments.
	p := chatPrincipal(host, "good")
	mcpResult, userErr, rpcErr := srv.callTool(context.Background(), p, "http://test.invalid", "mcp", "search_deals", map[string]any{"query": "x"}, 1)
	if rpcErr != nil || userErr != "" {
		t.Fatalf("MCP call: rpc=%v user=%q", rpcErr, userErr)
	}
	wantJSON, err := json.Marshal(mcpResult)
	if err != nil {
		t.Fatalf("marshal MCP result: %v", err)
	}
	if string(completed.Result) != string(wantJSON) {
		t.Fatalf("chat result %s != mcp result %s", completed.Result, wantJSON)
	}

	// The model was offered the chat surface only.
	reqs := prov.Requests()
	if len(reqs) < 1 {
		t.Fatal("provider never called")
	}
	offered := map[string]bool{}
	for _, d := range reqs[0].Tools {
		offered[d.Name] = true
	}
	if !offered["search_deals"] {
		t.Errorf("model was not offered search_deals: %v", offered)
	}
	for _, absent := range []string{"create_deals", "both_writer", "mcp_digest"} {
		if offered[absent] {
			t.Errorf("model was offered %q", absent)
		}
	}
}
