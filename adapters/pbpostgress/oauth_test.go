package pbpostgress

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	pbmcp "github.com/compdani/pocketbase-mcp"
)

func newForkOAuthApp(t *testing.T) core.App {
	t.Helper()
	dsn := os.Getenv("PB_DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("PB_TEST_DB_DSN")
	}
	if dsn == "" {
		t.Skip("fork OAuth tests require PB_DB_DSN (disposable Postgres)")
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
	return app
}

// deleteWithoutHooks removes a test record from the shared database with a
// plain SQL delete. app.Delete would start PocketBase's background storage
// cleanup, which recreates <dataDir>/storage after the call returns and races
// with t.TempDir's RemoveAll ("directory not empty").
func deleteWithoutHooks(app core.App, record *core.Record) {
	_, _ = app.DB().Delete(record.Collection().Name, dbx.HashExp{"id": record.Id}).Execute()
}

func forkCfg(mode string) pbmcp.Config {
	return pbmcp.Config{
		Path: "/mcp", AliasPaths: []string{"/sse"},
		Auth:     pbmcp.AuthConfig{Collection: "users"},
		OAuth:    pbmcp.OAuthConfig{Enabled: true, Mode: mode},
		Manifest: &pbmcp.Manifest{SchemaVersion: "1"},
	}
}

func disableNative(t *testing.T, app core.App) {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	if users.OAuth2Server.Enabled {
		users.OAuth2Server.Enabled = false
		if err := app.Save(users); err != nil {
			t.Fatal(err)
		}
	}
}

func enableNative(t *testing.T, app core.App) {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	users.OAuth2Server = core.OAuth2ServerConfig{
		Enabled: true, CodeDuration: 600,
		Clients: []core.OAuth2ServerClient{{Id: "chatgpt-dev", Name: "ChatGPT", Secret: "dev-oauth-secret-0123456789abcdef", RedirectURIs: []string{"https://example.com/callback"}}},
	}
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
}

func collectionNames(t *testing.T, app core.App) map[string]bool {
	t.Helper()
	cols, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, c := range cols {
		out[c.Name] = true
	}
	return out
}

func TestForkNativeMode(t *testing.T) {
	t.Run("disabled native config fails startup", func(t *testing.T) {
		app := newForkOAuthApp(t)
		disableNative(t, app)
		if _, err := New(app, forkCfg("native")); err == nil || !strings.Contains(err.Error(), "OAuth2Server") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("auto with disabled native config fails startup", func(t *testing.T) {
		app := newForkOAuthApp(t)
		disableNative(t, app)
		if _, err := New(app, forkCfg("auto")); err == nil {
			t.Fatal("expected startup error")
		}
	})
	t.Run("module mode rejected", func(t *testing.T) {
		app := newForkOAuthApp(t)
		enableNative(t, app)
		if _, err := New(app, forkCfg("module")); err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("native adds zero OAuth collections and zero module OAuth routes", func(t *testing.T) {
		app := newForkOAuthApp(t)
		enableNative(t, app)
		// A baseline server without OAuth on the same app is not possible
		// (duplicate guard), so compare collection sets before/after the
		// native server and probe the module's route paths.
		before := collectionNames(t, app)
		srv, err := New(app, forkCfg("native"))
		if err != nil {
			t.Fatal(err)
		}
		if srv.Adapter.OAuthPlan().EffectiveMode != "native" {
			t.Fatalf("plan %+v", srv.Adapter.OAuthPlan())
		}
		r, err := apis.NewRouter(app)
		if err != nil {
			t.Fatal(err)
		}
		srv.RegisterRoutes(r)
		mux, err := r.BuildMux()
		if err != nil {
			t.Fatal(err)
		}
		after := collectionNames(t, app)
		if len(before) != len(after) {
			t.Fatalf("collections changed by native mode: before %v after %v", before, after)
		}
		for name := range after {
			if strings.HasPrefix(name, "mcp_oauth_") {
				t.Fatalf("module OAuth collection %q created in native mode", name)
			}
		}
		do := func(method, path string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, path, strings.NewReader(""))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			return rec
		}
		// Module OAuth endpoints must NOT exist.
		for _, p := range []string{"/oauth/login", "/oauth/token", "/oauth/register"} {
			if rec := do(http.MethodGet, p); rec.Code != http.StatusNotFound {
				t.Fatalf("module route %s exists in native mode: %d", p, rec.Code)
			}
		}
		// Native mode serves MCP resource metadata owned by this adapter.
		rec := do(http.MethodGet, "/.well-known/oauth-protected-resource/mcp")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/api/collections/users/oauth2-server") {
			t.Fatalf("native resource metadata: %d %s", rec.Code, rec.Body.String())
		}
		// The fork's own native authorize endpoint exists (owned by the fork).
		if rec := do(http.MethodGet, "/api/collections/users/oauth2-server/authorize?response_type=code&client_id=chatgpt-dev&redirect_uri=https://example.com/callback&state=x"); rec.Code == http.StatusNotFound {
			t.Fatalf("fork native authorize missing: %d %s", rec.Code, rec.Body.String())
		}
		// Unauthenticated /mcp carries the challenge.
		if rec := do(http.MethodPost, "/mcp"); rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "oauth-protected-resource") {
			t.Fatalf("401 challenge: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	})
}

// Snapshot router declarations without invoking endpoints (including POST-only routes).
func routeList(v reflect.Value, prefix string) []string {
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		return routeList(v.Elem(), prefix)
	}
	if f := v.FieldByName("RouterGroup"); f.IsValid() {
		return routeList(f, prefix)
	}
	if f := v.FieldByName("Method"); f.IsValid() {
		return []string{f.String() + " " + prefix + v.FieldByName("Path").String()}
	}
	prefix += v.FieldByName("Prefix").String()
	var result []string
	children := v.FieldByName("children")
	for i := 0; i < children.Len(); i++ {
		result = append(result, routeList(children.Index(i), prefix)...)
	}
	sort.Strings(result)
	return result
}
func TestNativeOwnershipInventory(t *testing.T) {
	app := newForkOAuthApp(t)
	enableNative(t, app)
	before := collectionNames(t, app)
	jobs := app.Cron().Total()
	authHooks := app.OnRecordAuthWithPasswordRequest().Length()
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	baseline := routeList(reflect.ValueOf(r), "")
	srv, err := New(app, forkCfg("native"))
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterRoutes(r)
	after := routeList(reflect.ValueOf(r), "")
	for _, route := range after {
		if route == "GET /.well-known/oauth-protected-resource" || route == "GET /.well-known/oauth-protected-resource/mcp" || strings.Contains(route, " /mcp") || strings.Contains(route, " /sse") {
			continue
		}
		found := false
		for _, old := range baseline {
			if route == old {
				found = true
			}
		}
		if !found {
			t.Fatalf("native added route %s", route)
		}
	}
	if !reflect.DeepEqual(before, collectionNames(t, app)) || jobs != app.Cron().Total() || authHooks != app.OnRecordAuthWithPasswordRequest().Length() {
		t.Fatal("native mode changed OAuth collections/jobs/auth hooks")
	}
}

func TestNativeDiscoveryRegistrationPKCE(t *testing.T) {
	app := newForkOAuthApp(t)
	enableNative(t, app)
	users, _ := app.FindCollectionByNameOrId("users")
	users.OAuth2Server.Registration.Enabled = true
	users.MFA.Enabled = false
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
	user := core.NewRecord(users)
	user.Set("email", "m4@example.com")
	user.SetPassword("m4-password-12345")
	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteWithoutHooks(app, user) })
	srv, err := New(app, forkCfg("native"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterRoutes(r)
	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body, ct, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	parse := func(rec *httptest.ResponseRecorder) map[string]any {
		var data map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
			t.Fatalf("JSON %d %s", rec.Code, rec.Body.String())
		}
		return data
	}
	rec := do("POST", "/mcp", "", "", "")
	if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatal(rec.Code, rec.Header())
	}
	md := parse(do("GET", "/.well-known/oauth-protected-resource/mcp", "", "", ""))
	issuer := md["authorization_servers"].([]any)[0].(string)
	u, _ := url.Parse(issuer)
	as := parse(do("GET", "/.well-known/oauth-authorization-server"+u.Path, "", "", ""))
	register, _ := url.Parse(as["registration_endpoint"].(string))
	rec = do("POST", register.Path, `{"client_name":"M4 client","redirect_uris":["http://127.0.0.1:32123/callback"],"token_endpoint_auth_method":"none"}`, "application/json", "")
	if rec.Code != 201 {
		t.Fatalf("DCR %d %s", rec.Code, rec.Body.String())
	}
	client := parse(rec)["client_id"].(string)
	verifier := strings.Repeat("a", 64)
	sum := sha256.Sum256([]byte(verifier))
	params := url.Values{"client_id": {client}, "redirect_uri": {"http://127.0.0.1:32123/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "scope": {"openid"}}
	rec = do("GET", u.Path+"/authorize?"+params.Encode(), "", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "unverified app") || !strings.Contains(rec.Body.String(), "127.0.0.1:32123") {
		t.Fatalf("hosted page %d %s", rec.Code, rec.Body.String())
	}
	params.Set("identity", "m4@example.com")
	params.Set("password", "m4-password-12345")
	rec = do("POST", u.Path+"/authorize", params.Encode(), "application/x-www-form-urlencoded", "")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("login %d %s", rec.Code, rec.Body.String())
	}
	form := url.Values{"client_id": {client}, "redirect_uri": {"http://127.0.0.1:32123/callback"}, "grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}}
	rec = do("POST", u.Path+"/token", form.Encode(), "application/x-www-form-urlencoded", "")
	if rec.Code != 200 {
		t.Fatalf("token %d %s", rec.Code, rec.Body.String())
	}
	token := parse(rec)["access_token"].(string)
	rec = do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "application/json", token)
	if rec.Code != 200 || parse(rec)["error"] != nil {
		t.Fatalf("tools %d %s", rec.Code, rec.Body.String())
	}
}
