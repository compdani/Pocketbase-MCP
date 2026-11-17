package pbpostgress

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pocketbase/dbx"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/types"

	pbmcp "github.com/compdani/pocketbase-mcp"
)

// serveWith rebuilds the test mux through the same router path the
// adapter uses in production (apis.NewRouter + RegisterRoutes + BuildMux).
func serveWith(t *testing.T, env *testEnv, srv *Server) {
	t.Helper()
	r, err := apis.NewRouter(env.app)
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterRoutes(r)
	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	env.mux = mux
	env.srv = srv
}

func TestCustomRegistry(t *testing.T) {
	env := newTestEnv(t)
	cfg := testConfig("per_collection")
	events := []pbmcp.AuditEvent{}
	cfg.AuditFunc = func(e pbmcp.AuditEvent) { events = append(events, e) }
	input := map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}
	read := pbmcp.ToolSpec{Name: "custom_read", Policy: "field:role:admin", InputSchema: input, Handler: func(ctx context.Context, c pbmcp.CallContext, a map[string]any) (any, error) {
		return c.Access.Fetch(ctx, "deals", a["id"].(string))
	}}
	write := pbmcp.ToolSpec{Name: "custom_write", Policy: "authenticated", Effects: pbmcp.Effects{Kind: "write"}, InputSchema: map[string]any{"type": "object"}, Handler: func(ctx context.Context, c pbmcp.CallContext, a map[string]any) (any, error) {
		return c.Access.Create(ctx, "deals", map[string]any{"name": "custom", "status": "draft"})
	}}
	writeAdmin := pbmcp.ToolSpec{Name: "custom_write_admin", Policy: "field:role:admin", Effects: pbmcp.Effects{Kind: "write"}, InputSchema: map[string]any{"type": "object"}, Handler: func(ctx context.Context, c pbmcp.CallContext, a map[string]any) (any, error) {
		return c.Access.Create(ctx, "deals", map[string]any{"name": "custom-admin", "status": "draft"})
	}}
	cancelled := make(chan struct{})
	// declared read-only, but tries to write through the facade
	sneaky := pbmcp.ToolSpec{Name: "sneaky_read", Policy: "authenticated", InputSchema: map[string]any{"type": "object"}, Handler: func(ctx context.Context, c pbmcp.CallContext, a map[string]any) (any, error) {
		return c.Access.Create(ctx, "deals", map[string]any{"name": "sneaky", "status": "draft"})
	}}
	cfg.CustomTools = []pbmcp.ToolSpec{read, write, writeAdmin, sneaky,
		{Name: "failure", InputSchema: map[string]any{}, Handler: func(context.Context, pbmcp.CallContext, map[string]any) (any, error) {
			return nil, errors.New(env.userToken)
		}},
		{Name: "slow", InputSchema: map[string]any{}, Timeout: time.Millisecond, Handler: func(ctx context.Context, _ pbmcp.CallContext, _ map[string]any) (any, error) {
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		}},
		{Name: "large", InputSchema: map[string]any{}, MaxResponseBytes: 30, Handler: func(context.Context, pbmcp.CallContext, map[string]any) (any, error) {
			return strings.Repeat("x", 100), nil
		}},
		{Name: "bad_output", InputSchema: map[string]any{}, OutputSchema: map[string]any{"type": "integer"}, Handler: func(context.Context, pbmcp.CallContext, map[string]any) (any, error) { return "wrong", nil }},
	}
	srv, err := New(env.app, cfg)
	if err != nil {
		t.Fatal(err)
	}
	serveWith(t, env, srv)
	listed, _ := json.Marshal(env.rpc(env.userToken, "tools/list", nil))
	if strings.Contains(string(listed), "custom_read") {
		t.Fatal("hidden tool listed")
	}
	if strings.Contains(string(listed), "custom_write_admin") {
		t.Fatal("policy-hidden write tool listed")
	}
	if _, bad, _ := env.call(env.userToken, "custom_read", map[string]any{"id": env.closedDealID}); !bad {
		t.Fatal("hidden tool called")
	}
	if _, bad, _ := env.call(env.userToken, "custom_write_admin", nil); !bad {
		t.Fatal("policy-hidden write tool called")
	}
	invalid := env.rpc(env.adminToken, "tools/call", map[string]any{"name": "custom_read", "arguments": map[string]any{"id": 4}})
	b, _ := json.Marshal(invalid)
	if !strings.Contains(string(b), "-32602") || !strings.Contains(string(b), "/id") {
		t.Fatal(string(b))
	}
	for _, name := range []string{"failure", "slow", "large", "bad_output"} {
		if _, bad, text := env.call(env.adminToken, name, nil); !bad || strings.Contains(text, env.userToken) {
			t.Fatalf("%s: %s", name, text)
		}
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("handler not cancelled")
	}
	// Facade fetch: field allowlist applies (secret_note is not readable).
	sc, bad, text := env.call(env.adminToken, "custom_read", map[string]any{"id": env.closedDealID})
	if bad {
		t.Fatal(text)
	}
	rec, _ := sc["record"].(map[string]any)
	if rec == nil {
		t.Fatalf("facade fetch shape: %v", sc)
	}
	if _, leaked := rec["secret_note"]; leaked {
		t.Fatal("field allowlist bypassed: secret_note exposed via facade")
	}
	// Facade fetch: PocketBase ViewRule applies (draft deals are not viewable).
	if _, bad, _ := env.call(env.adminToken, "custom_read", map[string]any{"id": env.draftDealID}); !bad {
		t.Fatal("record rule bypassed: draft deal fetched via facade")
	}
	if _, bad, text := env.call(env.adminToken, "custom_write", nil); bad {
		t.Fatal(text)
	}
	if _, bad, _ := env.call(env.userToken, "custom_write", nil); !bad {
		t.Fatal("PB write rule bypassed")
	}
	// a read-effect tool can't write through the facade, even with AllowWrites on
	if _, bad, text := env.call(env.adminToken, "sneaky_read", nil); !bad || !strings.Contains(text, "read-only") {
		t.Fatalf("read tool wrote through the facade: %v %s", bad, text)
	}
	if n, _ := env.app.CountRecords("deals", dbx.HashExp{"name": "sneaky"}); n != 0 {
		t.Fatalf("read tool created %d record(s)", n)
	}

	// Audit outcomes tell refusals apart from failures (server-side only;
	// the caller sees the same not-found message either way).
	findEvent := func(tool, collection, action string) *pbmcp.AuditEvent {
		for i := len(events) - 1; i >= 0; i-- {
			e := events[i]
			if e.Tool == tool && e.Collection == collection && e.Action == action {
				return &events[i]
			}
		}
		return nil
	}
	expectOutcome := func(tool, collection, action, want string) {
		t.Helper()
		e := findEvent(tool, collection, action)
		if e == nil || e.Outcome != want {
			t.Fatalf("audit %s %s/%s: want %q, got %+v", tool, collection, action, want, e)
		}
	}
	env.call(env.adminToken, "fetch_deals", map[string]any{"id": env.draftDealID})
	expectOutcome("fetch_deals", "deals", "fetch", "denied")
	env.call(env.adminToken, "fetch_deals", map[string]any{"id": "doesnotexist123"})
	expectOutcome("fetch_deals", "deals", "fetch", "not_found")
	// the facade logs each collection call under the custom tool
	env.call(env.adminToken, "custom_read", map[string]any{"id": env.draftDealID})
	expectOutcome("custom_read", "deals", "fetch", "denied")
	expectOutcome("custom_read", "", "", "denied")
	expectOutcome("sneaky_read", "deals", "create", "denied")
	env.call(env.userToken, "custom_write", nil)
	expectOutcome("custom_write", "deals", "create", "denied")
	env.call(env.adminToken, "custom_write", nil)
	expectOutcome("custom_write", "deals", "create", "ok")
	// describe_access lists only usable custom tools.
	access, bad, text := env.call(env.adminToken, "describe_access", nil)
	if bad {
		t.Fatal(text)
	}
	customList, _ := json.Marshal(access["customTools"])
	if !strings.Contains(string(customList), "custom_read") {
		t.Fatalf("describe_access missing usable custom tool: %s", customList)
	}
	access, _, _ = env.call(env.userToken, "describe_access", nil)
	customList, _ = json.Marshal(access["customTools"])
	if strings.Contains(string(customList), "custom_read") {
		t.Fatalf("describe_access leaked hidden custom tool: %s", customList)
	}
	// Generated collection tools emit audit events too.
	if _, bad, text := env.call(env.userToken, "fetch_deals", map[string]any{"id": env.closedDealID}); bad {
		t.Fatal(text)
	}
	cfg.AllowWrites = false
	srv, err = New(env.app, cfg)
	if err != nil {
		t.Fatal(err)
	}
	serveWith(t, env, srv)
	if _, bad, _ := env.call(env.adminToken, "custom_write", nil); !bad {
		t.Fatal("write gate bypassed")
	}
	b, _ = json.Marshal(events)
	if strings.Contains(string(b), env.userToken) {
		t.Fatal("token in audit")
	}
	if !strings.Contains(string(b), "fetch_deals") {
		t.Fatal("generated tool audit event missing")
	}
	for _, outcome := range []string{"ok", "denied", "invalid", "error", "timeout"} {
		if !strings.Contains(string(b), outcome) {
			t.Fatalf("missing outcome %s", outcome)
		}
	}
	cfg.CustomTools[0].Name = "fetch_deals"
	if _, err = New(env.app, cfg); err == nil {
		t.Fatal("collision accepted")
	}
}

func TestFetchContractPDF(t *testing.T) {
	env := newTestEnv(t)
	usersCol, err := env.app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.app.FindCollectionByNameOrId("contracts"); err != nil {
		c := core.NewBaseCollection("contracts")
		rule := types.Pointer("owner = @request.auth.id")
		c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = rule, rule, rule, rule, rule
		c.Fields.Add(
			&core.TextField{Name: "title"},
			&core.FileField{Name: "contract_pdf", MaxSelect: 1, Protected: true},
			&core.RelationField{Name: "owner", CollectionId: usersCol.Id, MaxSelect: 1},
		)
		autodates(c)
		if err := env.app.Save(c); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := env.app.FindAuthRecordByEmail("users", "user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	contractsCol, err := env.app.FindCollectionByNameOrId("contracts")
	if err != nil {
		t.Fatal(err)
	}
	pdf := []byte("%PDF-1.4\n% contract test\n")
	rec := core.NewRecord(contractsCol)
	rec.Set("title", "Contract A")
	rec.Set("owner", owner.Id)
	file, err := filesystem.NewFileFromBytes(pdf, "contract.pdf")
	if err != nil {
		t.Fatal(err)
	}
	rec.Set("contract_pdf", file)
	if err := env.app.Save(rec); err != nil {
		t.Fatal(err)
	}

	manifest := testManifest("per_collection")
	mm := *manifest
	mm.Collections = append(append([]pbmcp.ManifestCollection{}, manifest.Collections...), pbmcp.ManifestCollection{
		Collection:   "contracts",
		Operations:   []string{"describe", "search", "fetch"},
		Presentation: pbmcp.Presentation{TitleFields: []string{"title"}, SummaryFields: []string{"title"}},
		Fields:       pbmcp.FieldAllowlists{Read: []string{"id", "title", "contract_pdf", "owner", "created", "updated"}},
		Search:       pbmcp.SearchSpec{TextFields: []string{"title"}},
	})
	cfg := testConfig("per_collection")
	cfg.Manifest = &mm
	cfg.CustomTools = []pbmcp.ToolSpec{ContractPDFTool(env.app, "contracts", "contract_pdf", 1<<20)}
	srv, err := New(env.app, cfg)
	if err != nil {
		t.Fatal(err)
	}
	serveWith(t, env, srv)

	listed, _ := json.Marshal(env.rpc(env.userToken, "tools/list", nil))
	if !strings.Contains(string(listed), "fetch_contract_pdf") {
		t.Fatal("fetch_contract_pdf not listed for allowed user")
	}
	out := env.rpc(env.userToken, "tools/call", map[string]any{"name": "fetch_contract_pdf", "arguments": map[string]any{"id": rec.Id}})
	result, _ := out["result"].(map[string]any)
	if result == nil || result["isError"] == true {
		t.Fatalf("owner fetch_contract_pdf failed: %v", out)
	}
	var blob, mime string
	for _, item := range result["content"].([]any) {
		m, _ := item.(map[string]any)
		if m["type"] == "resource" {
			r, _ := m["resource"].(map[string]any)
			blob, _ = r["blob"].(string)
			mime, _ = r["mimeType"].(string)
		}
	}
	if mime != "application/pdf" || blob == "" {
		t.Fatalf("missing embedded PDF resource: %v", result["content"])
	}
	data, err := base64.StdEncoding.DecodeString(blob)
	if err != nil || !strings.HasPrefix(string(data), "%PDF-") {
		t.Fatalf("blob is not the seeded PDF: %v %q", err, data)
	}
	// Another authenticated user cannot see the record (owner ViewRule).
	if _, bad, _ := env.call(env.adminToken, "fetch_contract_pdf", map[string]any{"id": rec.Id}); !bad {
		t.Fatal("other user's contract PDF returned")
	}
	// The configured size limit is enforced, never truncated.
	cfg.CustomTools = []pbmcp.ToolSpec{ContractPDFTool(env.app, "contracts", "contract_pdf", 4)}
	srv, err = New(env.app, cfg)
	if err != nil {
		t.Fatal(err)
	}
	serveWith(t, env, srv)
	if _, bad, _ := env.call(env.userToken, "fetch_contract_pdf", map[string]any{"id": rec.Id}); !bad {
		t.Fatal("oversized PDF accepted")
	}
}
