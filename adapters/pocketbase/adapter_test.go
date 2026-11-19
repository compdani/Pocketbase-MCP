package pocketbase

// Ported from customjs mcp/mcp_test.go scenarios (milestone 1 acceptance gate).
// Deviations for milestone 1: writes are gated by Config.AllowWrites (not a
// users.role check), so write tools are listed for any accepted principal when
// AllowWrites is on; PocketBase collection rules still reject non-admin writes.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase"
	"ganigeorgiev.com/pocketbase/pocketbase/apis"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"

	pbmcp "github.com/compdani/pocketbase-mcp"
)

type testEnv struct {
	t            *testing.T
	app          core.App
	mux          http.Handler
	userToken    string
	adminToken   string
	superToken   string
	otherToken   string
	contactID    string
	inquiryID    string
	closedDealID string
	draftDealID  string
	srv          *Server
}

func autodates(c *core.Collection) {
	c.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
}

func testManifest(mode string) *pbmcp.Manifest {
	return &pbmcp.Manifest{
		SchemaVersion: "1", ToolMode: mode, MaxGeneratedTools: 40,
		Collections: []pbmcp.ManifestCollection{
			{Collection: "contact", Operations: []string{"describe", "search", "fetch"},
				Presentation: pbmcp.Presentation{TitleFields: []string{"first_name", "last_name"}, SummaryFields: []string{"first_name", "last_name", "email"}},
				Fields:       pbmcp.FieldAllowlists{Read: []string{"id", "first_name", "last_name", "email", "created", "updated"}},
				Search:       pbmcp.SearchSpec{TextFields: []string{"first_name", "last_name", "email"}, Filters: map[string][]string{"first_name": {"contains"}, "email": {"contains", "eq"}}, SortFields: []string{"first_name", "last_name", "created", "updated"}, DefaultSort: []string{"-updated"}}},
			{Collection: "deals", Operations: []string{"describe", "search", "fetch", "create", "update", "delete"},
				Presentation: pbmcp.Presentation{TitleFields: []string{"name"}, TitleSep: " \u2014 ", SummaryFields: []string{"name", "status", "ab_price"}},
				Fields:       pbmcp.FieldAllowlists{Read: []string{"id", "name", "status", "ab_price", "created", "updated"}, Create: []string{"name", "status", "ab_price"}, Update: []string{"name", "status", "ab_price"}},
				Search:       pbmcp.SearchSpec{TextFields: []string{"name"}, Filters: map[string][]string{"status": {"eq", "in"}, "ab_price": {"eq", "gte", "lte"}, "name": {"contains"}, "created": {"gte", "lte"}}, SortFields: []string{"name", "status", "ab_price", "created", "updated"}, DefaultSort: []string{"-updated"}, DefaultLimit: 20, MaxLimit: 50}},
			{Collection: "inquiries", Operations: []string{"describe", "search", "fetch"},
				Presentation: pbmcp.Presentation{TitleFields: []string{"type", "address"}, SummaryFields: []string{"type", "address", "message", "contact"}, QueryFields: []string{"contact.first_name", "contact.last_name"}, Expand: []string{"contact"}},
				Fields:       pbmcp.FieldAllowlists{Read: []string{"id", "type", "address", "message", "contact", "created", "updated"}},
				Search:       pbmcp.SearchSpec{TextFields: []string{"address", "message"}, Filters: map[string][]string{"type": {"eq", "in"}}, SortFields: []string{"address", "created", "updated"}},
				Relations:    map[string]pbmcp.RelationSpec{"contact": {Target: "contact", Fields: []string{"id", "first_name", "last_name"}, MaxDepth: 1}}},
			{Collection: "notes", Operations: []string{"describe", "search", "fetch", "create", "update", "delete"},
				Presentation: pbmcp.Presentation{TitleFields: []string{"title"}, SummaryFields: []string{"title"}},
				Fields:       pbmcp.FieldAllowlists{Read: []string{"id", "title", "created", "updated"}, Create: []string{"title"}, Update: []string{"title"}},
				Search:       pbmcp.SearchSpec{TextFields: []string{"title"}, Filters: map[string][]string{"title": {"contains"}}}},
			{Collection: "reports", Operations: []string{"describe", "search", "fetch", "create", "update", "delete"}, Policy: "field:role:admin",
				Presentation: pbmcp.Presentation{TitleFields: []string{"title"}, SummaryFields: []string{"title"}},
				Fields:       pbmcp.FieldAllowlists{Read: []string{"id", "title", "created", "updated"}, Create: []string{"title"}, Update: []string{"title"}},
				Search:       pbmcp.SearchSpec{TextFields: []string{"title"}}},
		},
	}
}

func testConfig(mode string) pbmcp.Config {
	return pbmcp.Config{
		AllowWrites: true,
		Auth:        pbmcp.AuthConfig{Collections: []string{"users"}, AllowSuperusers: true},
		Manifest:    testManifest(mode),
	}
}

func newTestEnv(t *testing.T) *testEnv { return newTestEnvMode(t, "per_collection") }

func newTestEnvMode(t *testing.T, mode string) *testEnv {
	t.Helper()
	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: t.TempDir()})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.ResetBootstrapState() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	usersRule := `@request.auth.collectionName = "users"`
	adminRule := `@request.auth.role ~ "admin"`

	users, err := app.FindCollectionByNameOrId("users")
	must(err)
	users.Fields.Add(&core.SelectField{Name: "role", Values: []string{"admin", "propertyview"}, MaxSelect: 2})
	must(app.Save(users))

	contact := core.NewBaseCollection("contact")
	contact.ListRule = types.Pointer(usersRule)
	contact.ViewRule = types.Pointer(adminRule)
	contact.Fields.Add(&core.TextField{Name: "first_name"}, &core.TextField{Name: "last_name"}, &core.EmailField{Name: "email"})
	autodates(contact)
	must(app.Save(contact))

	deals := core.NewBaseCollection("deals")
	deals.ListRule = types.Pointer(`@request.auth.id != "" && status = "closed"`)
	deals.ViewRule = deals.ListRule
	deals.CreateRule = types.Pointer(adminRule)
	deals.UpdateRule = types.Pointer(adminRule)
	deals.DeleteRule = types.Pointer(adminRule)
	deals.Fields.Add(
		&core.TextField{Name: "name"},
		&core.SelectField{Name: "status", Values: []string{"draft", "closed"}, MaxSelect: 1},
		&core.NumberField{Name: "ab_price"}, &core.TextField{Name: "secret_note"},
	)
	autodates(deals)
	must(app.Save(deals))

	inquiries := core.NewBaseCollection("inquiries")
	inquiries.ListRule = types.Pointer(usersRule)
	inquiries.ViewRule = types.Pointer(usersRule)
	inquiries.Fields.Add(
		&core.SelectField{Name: "type", Values: []string{"inquiry", "address_request"}, MaxSelect: 1},
		&core.TextField{Name: "address"},
		&core.TextField{Name: "message"},
		&core.RelationField{Name: "contact", CollectionId: contact.Id, MaxSelect: 1},
	)
	autodates(inquiries)
	must(app.Save(inquiries))

	notes := core.NewBaseCollection("notes")
	notes.Fields.Add(&core.TextField{Name: "title"})
	autodates(notes)
	must(app.Save(notes))

	reports := core.NewBaseCollection("reports")
	reports.ListRule = types.Pointer(usersRule)
	reports.ViewRule = types.Pointer(usersRule)
	reports.CreateRule = types.Pointer(adminRule)
	reports.UpdateRule = types.Pointer(adminRule)
	reports.DeleteRule = types.Pointer(adminRule)
	reports.Fields.Add(&core.TextField{Name: "title"})
	autodates(reports)
	must(app.Save(reports))

	customers := core.NewAuthCollection("customers")
	must(app.Save(customers))

	env := &testEnv{t: t, app: app}
	c := core.NewRecord(contact)
	c.Set("first_name", "Alice")
	c.Set("last_name", "Walker")
	c.Set("email", "alice@example.com")
	must(app.Save(c))
	env.contactID = c.Id

	inq := core.NewRecord(inquiries)
	inq.Set("type", "inquiry")
	inq.Set("address", "12 Oak St")
	inq.Set("message", "Is it still available?")
	inq.Set("contact", c.Id)
	must(app.Save(inq))
	env.inquiryID = inq.Id

	n := core.NewRecord(notes)
	n.Set("title", "secret")
	must(app.Save(n))

	rp := core.NewRecord(reports)
	rp.Set("title", "quarterly")
	must(app.Save(rp))

	for i := range 30 {
		d := core.NewRecord(deals)
		d.Set("name", "Deal "+strconv.Itoa(i))
		d.Set("ab_price", float64(100*(i+1)))
		d.Set("secret_note", "hidden-secret")
		if i < 5 {
			d.Set("status", "closed")
		} else {
			d.Set("status", "draft")
		}
		must(app.Save(d))
		if i == 0 {
			env.closedDealID = d.Id
		}
		if i == 10 {
			env.draftDealID = d.Id
		}
		time.Sleep(2 * time.Millisecond)
	}

	newUser := func(email, role string) string {
		u := core.NewRecord(users)
		u.SetEmail(email)
		u.SetPassword("password123")
		if role != "" {
			u.Set("role", []string{role})
		}
		must(app.Save(u))
		token, err := u.NewAuthToken()
		must(err)
		return token
	}
	env.userToken = newUser("user@example.com", "")
	env.adminToken = newUser("admin@example.com", "admin")

	su, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	must(err)
	s := core.NewRecord(su)
	s.SetEmail("root@example.com")
	s.SetPassword("password123")
	must(app.Save(s))
	env.superToken, err = s.NewAuthToken()
	must(err)

	{
		cu := core.NewRecord(customers)
		cu.SetEmail("other@example.com")
		cu.SetPassword("password123")
		must(app.Save(cu))
		tok, err := cu.NewAuthToken()
		must(err)
		env.otherToken = tok
	}

	app.OnRecordCreateRequest("deals").BindFunc(func(e *core.RecordRequestEvent) error {
		e.Record.Set("name", e.Record.GetString("name")+" (hooked)")
		return e.Next()
	})

	srv, err := New(app, testConfig(mode))
	must(err)
	env.srv = srv
	r, err := apis.NewRouter(app)
	must(err)
	srv.RegisterRoutes(r)
	env.mux, err = r.BuildMux()
	must(err)
	return env
}

func (env *testEnv) raw(token, body string, headers ...string) (int, map[string]any) {
	env.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (env *testEnv) rpc(token, method string, params any) map[string]any {
	env.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	code, out := env.raw(token, string(body))
	if code != http.StatusOK {
		env.t.Fatalf("%s: HTTP %d %v", method, code, out)
	}
	return out
}

func (env *testEnv) call(token, tool string, args map[string]any) (map[string]any, bool, string) {
	env.t.Helper()
	out := env.rpc(token, "tools/call", map[string]any{"name": tool, "arguments": args})
	result, ok := out["result"].(map[string]any)
	if !ok {
		env.t.Fatalf("%s: no result: %v", tool, out)
	}
	isErr, _ := result["isError"].(bool)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	sc, _ := result["structuredContent"].(map[string]any)
	return sc, isErr, text
}

func TestProtocol(t *testing.T) {
	env := newTestEnv(t)
	init := env.rpc(env.userToken, "initialize", map[string]any{"protocolVersion": "2025-03-26"})
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" || res["instructions"] == "" {
		t.Fatalf("initialize: %v", res)
	}
	init = env.rpc(env.userToken, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if v := init["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Fatalf("fallback version = %v", v)
	}
	if out := env.rpc(env.userToken, "ping", nil); out["result"] == nil {
		t.Fatalf("ping: %v", out)
	}
	if code, _ := env.raw(env.userToken, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	if code, _ := env.raw("", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", code)
	}
	if code, _ := env.raw(env.userToken, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`); code != http.StatusBadRequest {
		t.Fatalf("batch: %d", code)
	}
	if code, _ := env.raw(env.userToken, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "MCP-Protocol-Version", "1999-01-01"); code != http.StatusBadRequest {
		t.Fatalf("bad protocol header: %d", code)
	}
	out := env.rpc(env.userToken, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
	if out["error"] == nil {
		t.Fatalf("unknown tool should be a JSON-RPC error: %v", out)
	}
}

func TestToolList(t *testing.T) {
	env := newTestEnv(t)
	names := func(token string) map[string]map[string]any {
		out := env.rpc(token, "tools/list", nil)
		m := map[string]map[string]any{}
		for _, tool := range out["result"].(map[string]any)["tools"].([]any) {
			def := tool.(map[string]any)
			m[def["name"].(string)] = def
		}
		return m
	}
	user := names(env.userToken)
	for _, want := range []string{"describe_collection", "search_deals", "fetch_inquiries"} {
		if _, ok := user[want]; !ok {
			t.Fatalf("missing tool %s in %v", want, user)
		}
	}
	if _, ok := user["search_notes"]; ok {
		t.Fatal("superuser-only collection exposed to a regular user")
	}
	if _, ok := names(env.superToken)["search_notes"]; !ok {
		t.Fatal("superuser should see search_notes")
	}
	def := user["search_deals"]
	if def["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatal("missing readOnlyHint")
	}
	props := def["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if enum := props["status"].(map[string]any)["enum"]; enum == nil {
		t.Fatal("select filter should list enum values")
	}
	for _, p := range []string{"ab_price__gte", "ab_price__lte", "created__gte", "created__lte", "page", "sort"} {
		if _, ok := props[p]; !ok {
			t.Fatalf("missing search property %s", p)
		}
	}
	deals, _ := env.app.FindCollectionByNameOrId("deals")
	deals.Fields.Add(&core.TextField{Name: "county"})
	if err := env.app.Save(deals); err != nil {
		t.Fatal(err)
	}
	props = names(env.userToken)["search_deals"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["county"]; ok {
		t.Fatal("undeclared field county must not appear after collection update (manifest-declared filters only)")
	}
}

func TestSearchAppliesListRuleBeforeLimit(t *testing.T) {
	env := newTestEnv(t)
	sc, isErr, text := env.call(env.userToken, "search_deals", map[string]any{"limit": 5})
	if isErr {
		t.Fatal(text)
	}
	results := sc["results"].([]any)
	if len(results) != 5 || sc["totalItems"].(float64) != 5 || sc["hasMore"] != false {
		t.Fatalf("expected the 5 visible deals, got %d (total %v)", len(results), sc["totalItems"])
	}
	for _, r := range results {
		if r.(map[string]any)["fields"].(map[string]any)["status"] != "closed" {
			t.Fatalf("hidden record leaked: %v", r)
		}
	}
	sc, _, _ = env.call(env.userToken, "search_deals", map[string]any{"limit": 2, "page": 2, "sort": "ab_price"})
	got := sc["results"].([]any)
	if len(got) != 2 || sc["hasMore"] != true || got[0].(map[string]any)["fields"].(map[string]any)["ab_price"].(float64) != 300 {
		t.Fatalf("paging/sort: %v", sc)
	}
	sc, _, _ = env.call(env.userToken, "search_deals", map[string]any{"ab_price__gte": 400})
	if len(sc["results"].([]any)) != 2 {
		t.Fatalf("range filter: %v", sc)
	}
	sc, _, _ = env.call(env.superToken, "search_deals", map[string]any{"limit": 50})
	if sc["totalItems"].(float64) != 30 {
		t.Fatalf("superuser should see all deals: %v", sc["totalItems"])
	}
}

func TestSearchValidation(t *testing.T) {
	env := newTestEnv(t)
	cases := []map[string]any{{"bogus": "x"}, {"status": "nope"}, {"ab_price__gte": "abc"}, {"created__gte": "not a date"}, {"sort": "secret"}, {"status__in": []any{"nope"}}}
	for _, args := range cases {
		if _, isErr, _ := env.call(env.userToken, "search_deals", args); !isErr {
			t.Fatalf("expected tool error for %v", args)
		}
	}
	if _, isErr, _ := env.call(env.userToken, "search_deals", map[string]any{"created__lte": time.Now().UTC().Format("2006-01-02")}); isErr {
		t.Fatal("date-only created__lte should be accepted")
	}
}

func TestSuperuserOnlyCollection(t *testing.T) {
	env := newTestEnv(t)
	if _, isErr, _ := env.call(env.userToken, "search_notes", map[string]any{}); !isErr {
		t.Fatal("regular user searched a superuser-only collection")
	}
	notes, _ := env.app.FindRecordsByFilter("notes", "", "", 1, 0)
	if _, isErr, _ := env.call(env.userToken, "fetch_notes", map[string]any{"id": notes[0].Id}); !isErr {
		t.Fatal("regular user fetched a superuser-only record")
	}
	if sc, isErr, text := env.call(env.superToken, "fetch_notes", map[string]any{"id": notes[0].Id}); isErr || sc["record"].(map[string]any)["title"] != "secret" {
		t.Fatalf("superuser fetch: %s %v", text, sc)
	}
}

func TestInquiriesExpandRespectsRelatedViewRule(t *testing.T) {
	env := newTestEnv(t)
	sc, isErr, text := env.call(env.userToken, "search_inquiries", map[string]any{"query": "alice"})
	if isErr || len(sc["results"].([]any)) != 1 {
		t.Fatalf("query via contact: %s %v", text, sc)
	}
	sc, _, _ = env.call(env.userToken, "fetch_inquiries", map[string]any{"id": env.inquiryID})
	record := sc["record"].(map[string]any)
	if exp, _ := record["expand"].(map[string]any); exp["contact"] != nil {
		t.Fatal("contact expanded for a user who cannot view it")
	}
	if record["contact"] != env.contactID {
		t.Fatal("relation id should still be present")
	}
	sc, _, _ = env.call(env.adminToken, "fetch_inquiries", map[string]any{"id": env.inquiryID})
	exp, _ := sc["record"].(map[string]any)["expand"].(map[string]any)
	if exp == nil || exp["contact"].(map[string]any)["first_name"] != "Alice" {
		t.Fatalf("admin should get the expanded contact: %v", sc["record"])
	}
	if _, hasEmail := exp["contact"].(map[string]any)["email"]; hasEmail {
		t.Fatal("expanded contact must expose only configured relation fields (email leaked)")
	}
	if !strings.Contains(sc["title"].(string), "Alice Walker") {
		t.Fatalf("title: %v", sc["title"])
	}
	if _, isErr, _ := env.call(env.userToken, "fetch_inquiries", map[string]any{"id": "missing"}); !isErr {
		t.Fatal("missing record should be a tool error")
	}
}

func TestGenericToolMode(t *testing.T) {
	env := newTestEnvMode(t, "generic")
	sc, isErr, text := env.call(env.userToken, "search", map[string]any{"collection": "deals", "filters": map[string]any{"ab_price__lte": 200}})
	if isErr || len(sc["results"].([]any)) != 2 {
		t.Fatalf("generic search: %s %v", text, sc)
	}
	if out := env.rpc(env.userToken, "tools/call", map[string]any{"name": "search_deals", "arguments": map[string]any{}}); out["error"] == nil {
		t.Fatal("per-collection tools should not exist in generic mode")
	}
	sc, isErr, _ = env.call(env.userToken, "describe_collection", map[string]any{"collection": "deals"})
	if isErr || sc["filters"] == nil {
		t.Fatalf("describe: %v", sc)
	}
}

func TestWriteTools(t *testing.T) {
	env := newTestEnv(t)
	hasTool := func(token, name string) bool {
		out := env.rpc(token, "tools/list", nil)
		for _, tool := range out["result"].(map[string]any)["tools"].([]any) {
			if tool.(map[string]any)["name"] == name {
				return true
			}
		}
		return false
	}
	if !hasTool(env.adminToken, "create_deals") || !hasTool(env.superToken, "delete_deals") {
		t.Fatal("admin and superuser should see write tools")
	}
	if _, isErr, _ := env.call(env.userToken, "create_deals", map[string]any{"data": map[string]any{"name": "x"}}); !isErr {
		t.Fatal("non-admin write must be rejected by PB rules")
	}
	sc, isErr, text := env.call(env.adminToken, "create_deals", map[string]any{"data": map[string]any{"name": "New", "status": "closed", "ab_price": 5}})
	if isErr {
		t.Fatal(text)
	}
	dealID := sc["id"].(string)
	if sc["record"].(map[string]any)["name"] != "New (hooked)" {
		t.Fatalf("request hook did not run: %v", sc["record"])
	}
	if _, isErr, text := env.call(env.adminToken, "create_deals", map[string]any{"data": map[string]any{"status": "nope"}}); !isErr || !strings.Contains(text, "status") {
		t.Fatalf("expected a field validation error, got %q", text)
	}
	if _, isErr, text := env.call(env.adminToken, "create_deals", map[string]any{"data": map[string]any{"bogus": 1}}); !isErr || !strings.Contains(text, "writable fields") {
		t.Fatalf("expected unknown field error, got %q", text)
	}
	if _, isErr, _ := env.call(env.adminToken, "create_notes", map[string]any{"data": map[string]any{"title": "x"}}); !isErr {
		t.Fatal("admin bypassed a superuser-only create rule")
	}
	if _, isErr, text := env.call(env.superToken, "create_notes", map[string]any{"data": map[string]any{"title": "x"}}); isErr {
		t.Fatal(text)
	}
	sc, isErr, text = env.call(env.adminToken, "update_deals", map[string]any{"id": dealID, "data": map[string]any{"ab_price": 42}})
	if isErr || sc["record"].(map[string]any)["ab_price"].(float64) != 42 {
		t.Fatalf("update: %s %v", text, sc)
	}
	if _, isErr, _ := env.call(env.adminToken, "update_deals", map[string]any{"id": "missing", "data": map[string]any{"ab_price": 1}}); !isErr {
		t.Fatal("update of a missing record should fail")
	}
	sc, isErr, text = env.call(env.adminToken, "delete_deals", map[string]any{"id": dealID})
	if isErr || sc["deleted"] != true {
		t.Fatalf("delete: %s", text)
	}
	if _, err := env.app.FindRecordById("deals", dealID); err == nil {
		t.Fatal("record still exists after delete")
	}
}
