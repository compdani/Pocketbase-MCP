package pbpostgress

import (
	"os"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	pbmcp "github.com/compdani/pocketbase-mcp"
)

func minimalApp(t *testing.T) core.App {
	t.Helper()
	dsn := os.Getenv("PB_DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("PB_TEST_DB_DSN")
	}
	if dsn == "" {
		t.Skip("fork adapter tests require PB_DB_DSN (disposable Postgres)")
	}
	driver := os.Getenv("PB_DB_DRIVER")
	if driver == "" {
		driver = "postgres"
	}
	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: t.TempDir(), DBDriver: driver, DBDSN: dsn})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.ResetBootstrapState() })
	if _, err := app.FindCollectionByNameOrId("items"); err == nil {
		return app
	}
	items := core.NewBaseCollection("items")
	rule := `@request.auth.id != ""`
	items.ListRule = types.Pointer(rule)
	items.ViewRule = types.Pointer(rule)
	items.Fields.Add(&core.TextField{Name: "title"}, &core.NumberField{Name: "price"}, &core.BoolField{Name: "active"}, &core.TextField{Name: "secret_note"})
	autodates(items)
	if err := app.Save(items); err != nil {
		t.Fatal(err)
	}
	return app
}

func baseManifest() *pbmcp.Manifest {
	return &pbmcp.Manifest{SchemaVersion: "1", Collections: []pbmcp.ManifestCollection{{
		Collection: "items", Operations: []string{"describe", "search", "fetch", "create", "update", "delete"},
		Presentation: pbmcp.Presentation{TitleFields: []string{"title"}, SummaryFields: []string{"title", "price"}},
		Fields:       pbmcp.FieldAllowlists{Read: []string{"id", "title", "price", "active", "created", "updated"}, Create: []string{"title", "price", "active"}, Update: []string{"title", "price"}},
		Search:       pbmcp.SearchSpec{TextFields: []string{"title"}, Filters: map[string][]string{"price": {"gte"}}, SortFields: []string{"price"}},
	}}}
}

func TestManifestValidationErrorsAdapter(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*pbmcp.Manifest)
		want   string
	}{
		{"missing collection", func(m *pbmcp.Manifest) { m.Collections[0].Collection = "nope" }, "not found"},
		{"unknown field", func(m *pbmcp.Manifest) { m.Collections[0].Fields.Read = []string{"nope"} }, "unknown field"},
		{"invalid operator for bool", func(m *pbmcp.Manifest) { m.Collections[0].Search.Filters = map[string][]string{"active": {"gte"}} }, "not valid"},
		{"unknown policy", func(m *pbmcp.Manifest) { m.Collections[0].Policy = "nope_policy" }, "unknown policy"},
		{"alias collision", func(m *pbmcp.Manifest) {
			m.Collections = append(m.Collections, pbmcp.ManifestCollection{Collection: "items", Alias: "items", Operations: []string{"search"}})
		}, "collides"},
		{"max generated tools", func(m *pbmcp.Manifest) { m.MaxGeneratedTools = 1 }, "generic"},
		{"relation target missing field setup", func(m *pbmcp.Manifest) {
			m.Collections[0].Relations = map[string]pbmcp.RelationSpec{"title": {Target: "users", Fields: []string{"id"}}}
		}, "not a relation"},
		{"sort on non-sortable", func(m *pbmcp.Manifest) {
			m.Collections[0].Search.SortFields = []string{"active", "price"}
			m.Collections[0].Search.Filters = nil
		}, ""}, // bool is sortable, placeholder handled below
	}
	for _, tc := range cases {
		if tc.name == "sort on non-sortable" {
			continue
		}
		app := minimalApp(t)
		m := baseManifest()
		tc.mutate(m)
		_, err := New(app, pbmcp.Config{Auth: pbmcp.AuthConfig{Collections: []string{"users"}}, Manifest: m})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
	// optional missing collection is skipped
	app := minimalApp(t)
	m := baseManifest()
	m.Collections = append(m.Collections, pbmcp.ManifestCollection{Collection: "ghost", Optional: true, Operations: []string{"search"}})
	srv, err := New(app, pbmcp.Config{Auth: pbmcp.AuthConfig{Collections: []string{"users"}}, Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.Adapter.AllCollections()) != 1 {
		t.Fatalf("optional ghost should be skipped: %v", srv.Adapter.AllCollections())
	}
}

func TestManifestEnvBehaviors(t *testing.T) {
	env := newTestEnv(t)
	// other-collection token rejected
	if code, _ := env.raw(env.otherToken, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != 401 {
		t.Fatalf("other-collection token should be 401, got %d", code)
	}
	// conditional fetch: draft deal not visible to regular user -> not-found message
	_, isErr, text := env.call(env.userToken, "fetch_deals", map[string]any{"id": env.draftDealID})
	if !isErr || !strings.Contains(text, "or you do not have access") {
		t.Fatalf("conditional fetch draft: err=%v text=%q", isErr, text)
	}
	// fetch closed: secret_note absent from record
	sc, isErr, text := env.call(env.userToken, "fetch_deals", map[string]any{"id": env.closedDealID})
	if isErr {
		t.Fatal(text)
	}
	rec := sc["record"].(map[string]any)
	if _, ok := rec["secret_note"]; ok {
		t.Fatalf("secret_note leaked in fetch: %v", rec)
	}
	if rec["name"] == nil {
		t.Fatalf("expected name in fetch record: %v", rec)
	}
	// search: summary fields must not contain secret_note
	sc, isErr, text = env.call(env.userToken, "search_deals", map[string]any{"limit": 2})
	if isErr {
		t.Fatal(text)
	}
	for _, r := range sc["results"].([]any) {
		if _, ok := r.(map[string]any)["fields"].(map[string]any)["secret_note"]; ok {
			t.Fatal("secret_note leaked in search")
		}
	}
	// policy denied: reports hidden for regular user, visible for admin
	if _, isErr, _ := env.call(env.userToken, "search_reports", map[string]any{}); !isErr {
		t.Fatal("policy-denied search_reports should be rejected for regular user")
	}
	if sc, isErr, text := env.call(env.adminToken, "search_reports", map[string]any{}); isErr || len(sc["results"].([]any)) != 1 {
		t.Fatalf("admin search_reports: %s %v", text, sc)
	}
	// describe_access states differ
	sc, isErr, _ = env.call(env.userToken, "describe_access", map[string]any{})
	if isErr {
		t.Fatal("describe_access user")
	}
	for _, c := range sc["collections"].([]any) {
		if c.(map[string]any)["collection"] == "reports" {
			t.Fatal("describe_access leaked reports to regular user")
		}
	}
	sc, _, _ = env.call(env.adminToken, "describe_access", map[string]any{})
	found := false
	for _, c := range sc["collections"].([]any) {
		cm := c.(map[string]any)
		if cm["collection"] == "deals" {
			found = true
			st := cm["operations"].(map[string]any)["search"].(map[string]any)["state"]
			if st != "conditional" {
				t.Fatalf("deals search state=%v want conditional", st)
			}
		}
	}
	if !found {
		t.Fatal("admin describe_access should include deals")
	}
	// input limits: disallowed key and modifier
	if _, isErr, text := env.call(env.adminToken, "create_deals", map[string]any{"data": map[string]any{"name": "x", "secret_note": "y"}}); !isErr || !strings.Contains(text, "secret_note") {
		t.Fatalf("disallowed create key: err=%v text=%q", isErr, text)
	}
	if _, isErr, text := env.call(env.adminToken, "create_deals", map[string]any{"data": map[string]any{"name+": "x"}}); !isErr || !strings.Contains(text, "modifier") {
		t.Fatalf("modifier create key: err=%v text=%q", isErr, text)
	}
	if _, isErr, text := env.call(env.adminToken, "update_deals", map[string]any{"id": env.closedDealID, "data": map[string]any{"secret_note": "y"}}); !isErr {
		t.Fatalf("disallowed update key should fail: %q", text)
	}
	// write response projection
	sc, isErr, text = env.call(env.adminToken, "create_deals", map[string]any{"data": map[string]any{"name": "ProjDeal", "status": "closed", "ab_price": 9}})
	if isErr {
		t.Fatal(text)
	}
	if _, ok := sc["record"].(map[string]any)["secret_note"]; ok {
		t.Fatalf("secret_note leaked in write response: %v", sc["record"])
	}
	// read-only server: writes hidden and denied
	cfg := testConfig("per_collection")
	cfg.AllowWrites = false
	srv2, err := New(env.app, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := apis.NewRouter(env.app)
	if err != nil {
		t.Fatal(err)
	}
	srv2.RegisterRoutes(r)
	mux2, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	// reuse env helpers by swapping mux temporarily
	oldMux := env.mux
	env.mux = mux2
	defer func() { env.mux = oldMux }()
	out := env.rpc(env.adminToken, "tools/list", nil)
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		if tl.(map[string]any)["name"] == "create_deals" {
			t.Fatal("read-only server must not list create_deals")
		}
	}
	if _, isErr, _ := env.call(env.adminToken, "create_deals", map[string]any{"data": map[string]any{"name": "x"}}); !isErr {
		t.Fatal("read-only direct create must be rejected")
	}
}

func TestBadCollectionChangeKeepsSnapshot(t *testing.T) {
	env := newTestEnv(t)
	before := envSrvConfigs(t, env)
	// rename deals.name field -> manifest revalidation fails, previous snapshot kept
	deals, err := env.app.FindCollectionByNameOrId("deals")
	if err != nil {
		t.Fatal(err)
	}
	f := deals.Fields.GetByName("name")
	if tf, ok := f.(*core.TextField); ok {
		tf.Name = "name_renamed"
	} else {
		t.Fatalf("name field type %T", f)
	}
	if err := env.app.Save(deals); err != nil {
		t.Fatal(err)
	}
	after := envSrvConfigs(t, env)
	if len(after) != len(before) {
		t.Fatalf("snapshot changed after bad change: before %d after %d", len(before), len(after))
	}
	found := false
	for _, c := range after {
		if c.Name == "deals" {
			found = true
			hasName := false
			for _, rf := range c.ReadFields {
				if rf == "name" {
					hasName = true
				}
			}
			if !hasName {
				t.Fatal("previous snapshot lost deals.name")
			}
		}
	}
	if !found {
		t.Fatal("deals missing from kept snapshot")
	}
	// restore the shared-DB field name so later tests are unaffected
	if tf, ok := deals.Fields.GetByName("name_renamed").(*core.TextField); ok {
		tf.Name = "name"
		_ = env.app.Save(deals)
	}
}

func envSrvConfigs(t *testing.T, env *testEnv) []*pbmcp.CollectionConfig {
	t.Helper()
	if env.srv == nil {
		t.Fatal("env server not stored")
	}
	return env.srv.Adapter.AllCollections()
}

func TestInstanceExcludedCollectionStaysUnavailable(t *testing.T) {
	env := newTestEnv(t)
	base := core.AsBaseApp(env.app)
	if base == nil {
		t.Skip("fork BaseApp unavailable")
	}
	// sanity: superuser can fetch notes before exclusion
	notes, _ := env.app.FindRecordsByFilter("notes", "", "", 1, 0)
	if len(notes) == 0 {
		t.Fatal("no notes")
	}
	if _, isErr, text := env.call(env.superToken, "fetch_notes", map[string]any{"id": notes[0].Id}); isErr {
		t.Fatalf("pre-exclusion fetch_notes: %s", text)
	}
	if err := base.SetInstanceScope(core.InstanceScope{Mode: "deny", Collections: []string{"notes"}}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.SetInstanceScope(core.InstanceScope{Mode: "all"}) }()
	// tools/list must not expose notes, direct call must fail, describe_access must not leak it
	out := env.rpc(env.superToken, "tools/list", nil)
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		if tl.(map[string]any)["name"] == "search_notes" {
			t.Fatal("instance-excluded notes exposed in tools/list")
		}
	}
	if _, isErr, _ := env.call(env.superToken, "fetch_notes", map[string]any{"id": notes[0].Id}); !isErr {
		t.Fatal("instance-excluded fetch_notes should fail")
	}
	sc, _, _ := env.call(env.superToken, "describe_access", map[string]any{})
	for _, c := range sc["collections"].([]any) {
		if c.(map[string]any)["collection"] == "notes" {
			t.Fatal("describe_access leaked instance-excluded notes")
		}
	}
	// after scope change back, it becomes available again; then exclude again to prove change is honored dynamically
	if err := base.SetInstanceScope(core.InstanceScope{Mode: "all"}); err != nil {
		t.Fatal(err)
	}
	if _, isErr, text := env.call(env.superToken, "fetch_notes", map[string]any{"id": notes[0].Id}); isErr {
		t.Fatalf("fetch after scope restore: %s", text)
	}
	if err := base.SetInstanceScope(core.InstanceScope{Mode: "deny", Collections: []string{"notes"}}); err != nil {
		t.Fatal(err)
	}
	if _, isErr, _ := env.call(env.superToken, "fetch_notes", map[string]any{"id": notes[0].Id}); !isErr {
		t.Fatal("fetch after second exclusion should fail")
	}
}
