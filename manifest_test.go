package pocketbasemcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestParseAndValidateManifest(t *testing.T) {
	m, err := ParseManifest([]byte(`{"schemaVersion":"1","toolMode":"per_collection","maxGeneratedTools":40,"collections":[{"collection":"properties","alias":"properties","operations":["describe","search","fetch"],"presentation":{"titleFields":["address1"]},"fields":{"read":["id","address1"]},"search":{"textFields":["address1"],"filters":{"price":["gte"]},"sortFields":["price"]},"policy":"authenticated"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateStructural(nil, nil); err != nil {
		t.Fatal(err)
	}
	if m.Revision() == "" || !strings.HasPrefix(m.Revision(), "sha256:") {
		t.Fatalf("revision %q", m.Revision())
	}
	if _, err := ParseManifest([]byte(`{"schemaVersion":"1","collections":[],"bogus":1}`)); err == nil {
		t.Fatal("unknown manifest keys must be rejected")
	}
}

func TestManifestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		m    Manifest
		want string
	}{
		{"schema version", Manifest{SchemaVersion: "2"}, "schemaVersion"},
		{"alias collision", Manifest{SchemaVersion: "1", Collections: []ManifestCollection{{Collection: "a", Alias: "x", Operations: []string{"search"}}, {Collection: "b", Alias: "x", Operations: []string{"search"}}}}, "collides"},
		{"tool name collision with custom", Manifest{SchemaVersion: "1", Collections: []ManifestCollection{{Collection: "a", Alias: "access", Operations: []string{"describe"}}}}, ""}, // placeholder replaced below
		{"unknown operation", Manifest{SchemaVersion: "1", Collections: []ManifestCollection{{Collection: "a", Operations: []string{"peek"}}}}, "unknown operation"},
		{"unknown policy", Manifest{SchemaVersion: "1", Collections: []ManifestCollection{{Collection: "a", Operations: []string{"search"}, Policy: "role:admin"}}}, "unknown policy"},
		{"unknown operator", Manifest{SchemaVersion: "1", Collections: []ManifestCollection{{Collection: "a", Operations: []string{"search"}, Search: SearchSpec{Filters: map[string][]string{"x": {"between"}}}}}}, "unknown operator"},
		{"limit order", Manifest{SchemaVersion: "1", Collections: []ManifestCollection{{Collection: "a", Operations: []string{"search"}, Search: SearchSpec{DefaultLimit: 60, MaxLimit: 50}}}}, "exceeds maxLimit"},
		{"max tools", Manifest{SchemaVersion: "1", MaxGeneratedTools: 1, Collections: []ManifestCollection{{Collection: "a", Operations: []string{"search", "fetch"}}}}, "generic"},
		{"bad tool mode", Manifest{SchemaVersion: "1", ToolMode: "weird"}, "toolMode"},
	}
	for _, tc := range cases {
		if tc.name == "tool name collision with custom" {
			err := tc.m.ValidateStructural(nil, []string{"describe_access"})
			if err == nil || !strings.Contains(err.Error(), "collides") {
				t.Fatalf("%s: %v", tc.name, err)
			}
			continue
		}
		err := tc.m.ValidateStructural(nil, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
	// registered policy passes
	m := Manifest{SchemaVersion: "1", Collections: []ManifestCollection{{Collection: "a", Operations: []string{"search"}, Policy: "can_edit"}}}
	if err := m.ValidateStructural(map[string]PolicyFunc{"can_edit": func(Principal) bool { return true }}, nil); err != nil {
		t.Fatal(err)
	}
	// generic mode does not count per-collection tools against max
	m2 := Manifest{SchemaVersion: "1", ToolMode: "generic", MaxGeneratedTools: 1, Collections: []ManifestCollection{{Collection: "a", Operations: []string{"search", "fetch", "create"}}}}
	if err := m2.ValidateStructural(nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBuildCollectionConfigValidation(t *testing.T) {
	fields := []FieldInfo{
		{Name: "id", Type: "text"}, {Name: "name", Type: "text"}, {Name: "price", Type: "number"},
		{Name: "active", Type: "bool"}, {Name: "owner", Type: "relation", RelCollection: "users"},
		{Name: "secret", Type: "password"}, {Name: "hidden_note", Type: "text", Hidden: true},
		{Name: "created", Type: "autodate"}, {Name: "updated", Type: "autodate"},
	}
	meta := SchemaCollection{Name: "items", ID: "col_items", Fields: fields, ListRule: RuleConditional, ViewRule: RuleConditional}
	users := SchemaCollection{Name: "users", ID: "col_users", Fields: []FieldInfo{{Name: "id", Type: "text"}, {Name: "name", Type: "text"}, {Name: "email", Type: "email"}}}
	resolve := func(n string) (SchemaCollection, bool) {
		if n == "users" || n == "col_users" {
			return users, true
		}
		if n == "items" {
			return meta, true
		}
		return SchemaCollection{}, false
	}
	good := ManifestCollection{Collection: "items", Operations: []string{"describe", "search", "fetch", "create"},
		Presentation: Presentation{TitleFields: []string{"name"}, SummaryFields: []string{"name", "price"}},
		Fields:       FieldAllowlists{Read: []string{"id", "name", "price", "active", "owner"}, Create: []string{"name", "price"}},
		Search:       SearchSpec{TextFields: []string{"name"}, Filters: map[string][]string{"price": {"gte", "lte"}, "active": {"eq"}}, SortFields: []string{"price"}},
		Relations:    map[string]RelationSpec{"owner": {Target: "users", Fields: []string{"id", "name"}}}}
	cfg, err := BuildCollectionConfig(good, meta, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CollectionID != "col_items" || !cfg.HasOperation("create") || len(cfg.Filters) != 3 {
		t.Fatalf("built cfg: %+v", cfg)
	}
	bad := []struct {
		name   string
		mutate func(*ManifestCollection)
		want   string
	}{
		{"unknown read field", func(c *ManifestCollection) { c.Fields.Read = []string{"nope"} }, "unknown field"},
		{"password read", func(c *ManifestCollection) { c.Fields.Read = []string{"secret"} }, "password"},
		{"hidden read", func(c *ManifestCollection) { c.Fields.Read = []string{"hidden_note"} }, "hidden"},
		{"invalid operator for bool", func(c *ManifestCollection) { c.Search.Filters = map[string][]string{"active": {"gte"}} }, "not valid for field type"},
		{"relation target mismatch", func(c *ManifestCollection) {
			c.Relations = map[string]RelationSpec{"owner": {Target: "items", Fields: []string{"id"}}}
		}, "does not match"},
		{"relation unknown field", func(c *ManifestCollection) {
			c.Relations = map[string]RelationSpec{"owner": {Target: "users", Fields: []string{"nope"}}}
		}, "unknown field"},
		{"summary not in read", func(c *ManifestCollection) { c.Presentation.SummaryFields = []string{"created"} }, "not in fields.read"},
		{"sort not sortable", func(c *ManifestCollection) { c.Search.SortFields = []string{"owner"} }, "not sortable"},
		{"default sort unknown", func(c *ManifestCollection) { c.Search.DefaultSort = []string{"-nope"} }, "unknown field"},
		{"expand without relation", func(c *ManifestCollection) { c.Presentation.Expand = []string{"name"} }, "no relations entry"},
		// search/sort/filter must not reach fields omitted from output
		{"text search on unreadable field", func(c *ManifestCollection) {
			c.Fields.Read = []string{"id", "price"}
			c.Presentation = Presentation{}
			c.Search.TextFields = []string{"name"}
		}, "search.textFields field \"name\" is not in fields.read"},
		{"sort on unreadable field", func(c *ManifestCollection) {
			c.Fields.Read = []string{"id", "name"}
			c.Presentation = Presentation{}
			c.Search = SearchSpec{SortFields: []string{"price"}}
		}, "search.sortFields field \"price\" is not in fields.read"},
		{"filter on unreadable field", func(c *ManifestCollection) {
			c.Fields.Read = []string{"id", "name"}
			c.Presentation = Presentation{}
			c.Search = SearchSpec{Filters: map[string][]string{"price": {"gte"}}}
		}, "search.filters field \"price\" is not in fields.read"},
		{"related query field not projected", func(c *ManifestCollection) {
			c.Presentation.QueryFields = []string{"owner.email"}
		}, "must be listed in relations"},
	}
	// Defaults never reach unreadable fields.
	narrow := ManifestCollection{Collection: "items", Operations: []string{"describe", "search"},
		Fields: FieldAllowlists{Read: []string{"id", "price"}}}
	ncfg, err := BuildCollectionConfig(narrow, meta, resolve)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ncfg.SearchFields {
		if f == "name" {
			t.Fatal("default text search must not include unreadable field name")
		}
	}
	for _, f := range ncfg.SortFields {
		if f != "id" && f != "price" {
			t.Fatalf("default sort includes unreadable field %q", f)
		}
	}
	for _, f := range ncfg.Filters {
		if f.Field != "price" {
			t.Fatalf("default filters include unreadable field %q", f.Field)
		}
	}
	for _, tc := range bad {
		c := good
		// deep-ish copy of maps/slices that mutate replaces (assignments above replace fields, fine)
		tc.mutate(&c)
		_, err := BuildCollectionConfig(c, meta, resolve)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestEvaluatePolicy(t *testing.T) {
	p := Principal{ID: "u1", CollectionName: "users", RecordValues: map[string][]string{"role": {"editor", "admin"}}}
	if !EvaluatePolicy("authenticated", p, nil) || !EvaluatePolicy("field:role:admin", p, nil) || EvaluatePolicy("field:role:owner", p, nil) {
		t.Fatal("field policy (multi-select) mismatch")
	}
	if !EvaluatePolicy("collection:users", p, nil) || EvaluatePolicy("collection:customers", p, nil) {
		t.Fatal("collection policy mismatch")
	}
	if EvaluatePolicy("superuser", p, nil) || !EvaluatePolicy("superuser", Principal{ID: "s", IsSuperuser: true}, nil) {
		t.Fatal("superuser policy mismatch")
	}
	if !EvaluatePolicy("custom", p, map[string]PolicyFunc{"custom": func(Principal) bool { return true }}) {
		t.Fatal("custom policy mismatch")
	}
	if err := ValidatePolicyName("field:role", nil); err == nil {
		t.Fatal("malformed field policy must fail")
	}
}

// --- server-level manifest behavior with a fake host ---

type fakeHost2 struct{ cfgs []*CollectionConfig }

func (f *fakeHost2) Authenticate(token string) (Principal, error) {
	switch token {
	case "good":
		return Principal{ID: "u1", CollectionName: "users", RecordValues: map[string][]string{}}, nil
	case "admin":
		return Principal{ID: "u2", CollectionName: "users", RecordValues: map[string][]string{"role": {"admin"}}}, nil
	}
	return Principal{}, ToolError("bad")
}
func (f *fakeHost2) LookupCollection(n string) (*CollectionConfig, bool) {
	for _, c := range f.cfgs {
		if c.Name == n || c.Alias == n {
			return c, true
		}
	}
	return nil, false
}
func (f *fakeHost2) VisibleCollections(Principal) []*CollectionConfig { return f.cfgs }
func (f *fakeHost2) AllCollections() []*CollectionConfig              { return f.cfgs }
func (f *fakeHost2) Search(_ context.Context, _ Principal, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	return map[string]any{"collection": cfg.Name, "results": []any{}, "page": 1, "perPage": 20, "totalItems": 0, "totalPages": 0, "hasMore": false}, nil
}
func (f *fakeHost2) Fetch(_ context.Context, _ Principal, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	return map[string]any{"id": "x", "collection": cfg.Name, "title": "t", "record": map[string]any{}}, nil
}
func (f *fakeHost2) Write(_ context.Context, _ Principal, op string, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	return map[string]any{"id": "x", "collection": cfg.Name, "op": op}, nil
}

func fakeCfg(name, policy string) *CollectionConfig {
	return &CollectionConfig{Name: name, Alias: name, Operations: []string{"describe", "search", "fetch", "create", "update", "delete"}, Policy: policy,
		FieldMap: map[string]FieldInfo{"name": {Name: "name", Type: "text"}}, Fields: []FieldInfo{{Name: "name", Type: "text"}},
		ReadFields: []string{"id", "name"}, CreateFields: []string{"name"}, UpdateFields: []string{"name"},
		SortFields: []string{"id", "name"}, DefaultSort: []string{"-id"}, DefaultLimit: 20, MaxLimit: 50,
		ListRule: RuleConditional, ViewRule: RuleConditional, CreateRule: RuleConditional, UpdateRule: RuleConditional, DeleteRule: RuleConditional}
}

func toolNames(t *testing.T, srv *Server, token string) map[string]bool {
	t.Helper()
	_, out := rpc(t, srv, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	m := map[string]bool{}
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		m[tl.(map[string]any)["name"].(string)] = true
	}
	return m
}

func callTool(t *testing.T, srv *Server, token, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	_, out := rpc(t, srv, token, string(body))
	res := out["result"].(map[string]any)
	isErr, _ := res["isError"].(bool)
	sc, _ := res["structuredContent"].(map[string]any)
	return sc, isErr
}

func TestServerManifestModesAndPolicies(t *testing.T) {
	host := &fakeHost2{cfgs: []*CollectionConfig{fakeCfg("items", "authenticated"), fakeCfg("reports", "field:role:admin")}}
	manifest := &Manifest{SchemaVersion: "1", ToolMode: "per_collection", MaxGeneratedTools: 40, Collections: []ManifestCollection{{Collection: "items", Operations: []string{"describe", "search", "fetch", "create", "update", "delete"}}, {Collection: "reports", Operations: []string{"describe", "search", "fetch"}, Policy: "field:role:admin"}}}
	srv := New(Config{Auth: AuthConfig{Collections: []string{"users"}}, AllowWrites: false, Manifest: manifest}, host)
	userTools := toolNames(t, srv, "good")
	if !userTools["search_items"] || userTools["search_reports"] || userTools["create_items"] || !userTools["describe_access"] || !userTools["describe_collection"] {
		t.Fatalf("user tools (read-only, policy): %v", userTools)
	}
	adminTools := toolNames(t, srv, "admin")
	if !adminTools["search_reports"] {
		t.Fatalf("admin tools: %v", adminTools)
	}
	if _, isErr := callTool(t, srv, "good", "search_reports", map[string]any{}); !isErr {
		t.Fatal("hidden policy-denied tool called directly must be rejected")
	}
	if _, isErr := callTool(t, srv, "good", "create_items", map[string]any{"data": map[string]any{"name": "x"}}); !isErr {
		t.Fatal("write with AllowWrites=false called directly must be rejected")
	}
	if _, isErr := callTool(t, srv, "good", "describe_collection", map[string]any{"collection": "reports"}); !isErr {
		t.Fatal("describe_collection for policy-hidden collection must be rejected")
	}
	sc, isErr := callTool(t, srv, "admin", "describe_access", map[string]any{})
	if isErr || sc["manifestRevision"] == "" || sc["toolMode"] != "per_collection" {
		t.Fatalf("describe_access: %v err=%v", sc, isErr)
	}
	cols := sc["collections"].([]any)
	found := false
	for _, c := range cols {
		cm := c.(map[string]any)
		if cm["collection"] == "reports" {
			found = true
			st := cm["operations"].(map[string]any)["search"].(map[string]any)["state"]
			if st != "conditional" {
				t.Fatalf("reports search state = %v, want conditional", st)
			}
			cst := cm["operations"].(map[string]any)["create"].(map[string]any)["state"]
			if cst != "unavailable" {
				t.Fatalf("reports create state = %v (not declared)", cst)
			}
		}
	}
	if !found {
		t.Fatal("admin describe_access should include reports")
	}
	// user describe_access must not leak hidden collection names
	sc, _ = callTool(t, srv, "good", "describe_access", map[string]any{})
	for _, c := range sc["collections"].([]any) {
		if c.(map[string]any)["collection"] == "reports" {
			t.Fatal("user describe_access leaked policy-hidden collection")
		}
	}
	// generic mode
	manifest2 := &Manifest{SchemaVersion: "1", ToolMode: "generic", Collections: manifest.Collections}
	srv2 := New(Config{Auth: AuthConfig{Collections: []string{"users"}}, AllowWrites: true, Manifest: manifest2}, host)
	gt := toolNames(t, srv2, "admin")
	if !gt["search"] || !gt["fetch"] || !gt["create"] || gt["search_items"] {
		t.Fatalf("generic tools: %v", gt)
	}
	if sc, isErr := callTool(t, srv2, "admin", "search", map[string]any{"collection": "items"}); isErr || sc["collection"] != "items" {
		t.Fatalf("generic search: %v err=%v", sc, isErr)
	}
	if sc, isErr := callTool(t, srv2, "admin", "create", map[string]any{"collection": "items", "data": map[string]any{"name": "x"}}); isErr || sc["op"] != "create" {
		t.Fatalf("generic create: %v err=%v", sc, isErr)
	}
	// empty manifest exposes no data tools
	srv3 := New(Config{Auth: AuthConfig{Collections: []string{"users"}}, Manifest: &Manifest{SchemaVersion: "1", Collections: []ManifestCollection{}}}, &fakeHost2{})
	et := toolNames(t, srv3, "good")
	if et["search_items"] || et["search"] {
		t.Fatalf("empty manifest tools: %v", et)
	}
	sc, _ = callTool(t, srv3, "good", "describe_access", map[string]any{})
	if len(sc["collections"].([]any)) != 0 {
		t.Fatalf("empty manifest describe_access: %v", sc)
	}
	var _ = http.StatusOK
}
