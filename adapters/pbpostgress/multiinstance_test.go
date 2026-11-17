package pbpostgress

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// Two fork instances (different apps) share one database, like
// pocket.dreia.org (main) and Event Pocket, and the MCP server runs on the
// secondary app. With the OAuth2 server's sign-in app URL set to main, MCP
// clients are sent to main, the secondary's sign-in page redirects there, and
// main's token works on the secondary's /mcp.
func TestMultiInstanceSharedDatabase(t *testing.T) {
	main := newForkOAuthApp(t)
	events := newForkOAuthApp(t) // separate data dir => separate instance ID

	mainID, _ := core.AsBaseApp(main).InstanceID()
	eventsID, _ := core.AsBaseApp(events).InstanceID()
	if mainID == eventsID {
		t.Fatal("expected two distinct instances")
	}

	// main uses the shared settings; events gets its own Application URL
	main.Settings().Meta.AppURL = "https://pocket.example"
	if err := main.Save(main.Settings()); err != nil {
		t.Fatal(err)
	}
	if err := core.AsBaseApp(events).EnableInstanceSettings(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.AsBaseApp(events).DisableInstanceSettings() })
	if err := events.ReloadSettings(); err != nil {
		t.Fatal(err)
	}
	events.Settings().Meta.AppURL = "https://events.example"
	if err := events.Save(events.Settings()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		main.Settings().Meta.AppURL = ""
		_ = main.Save(main.Settings())
	})

	// OAuth server on the shared users collection (configured from main)
	enableNative(t, main)
	users, _ := main.FindCollectionByNameOrId("users")
	users.OAuth2Server.Registration.Enabled = true
	users.OAuth2Server.IssuerURL = "https://pocket.example"
	users.MFA.Enabled = false
	if err := main.Save(users); err != nil {
		t.Fatal(err)
	}
	user := core.NewRecord(users)
	user.Set("email", "multi@example.com")
	user.SetPassword("multi-password-12345")
	if err := main.Save(user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteWithoutHooks(main, user) })
	if err := events.ReloadCachedCollections(); err != nil {
		t.Fatal(err)
	}

	mainMux := buildMux(t, main, false)
	eventsMux := buildMux(t, events, true) // MCP mounted on the secondary app

	parse := func(rec *httptest.ResponseRecorder) map[string]any {
		var data map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
			t.Fatalf("JSON %d %s", rec.Code, rec.Body.String())
		}
		return data
	}

	issuer := "https://pocket.example/api/collections/users/oauth2-server"

	// 1. The MCP server on the secondary app sends clients to the main app.
	md := parse(serve(eventsMux, "GET", "/.well-known/oauth-protected-resource/mcp", "", "", ""))
	if got := md["authorization_servers"].([]any)[0].(string); got != issuer {
		t.Fatalf("events /mcp advertises %s, want the main app %s", got, issuer)
	}

	// 2. Both apps describe one issuer: the main app.
	path := "/.well-known/oauth-authorization-server/api/collections/users/oauth2-server"
	for name, mux := range map[string]http.Handler{"main": mainMux, "events": eventsMux} {
		as := parse(serve(mux, "GET", path, "", "", ""))
		if as["issuer"] != issuer || as["authorization_endpoint"] != issuer+"/authorize" {
			t.Fatalf("%s AS metadata: issuer %v, authorize %v", name, as["issuer"], as["authorization_endpoint"])
		}
	}

	// 3. Register on main, then start sign-in on the secondary app: it redirects to main.
	rec := serve(mainMux, "POST", "/api/collections/users/oauth2-server/register", `{"client_name":"Multi","redirect_uris":["http://127.0.0.1:32124/callback"],"token_endpoint_auth_method":"none"}`, "application/json", "")
	if rec.Code != 201 {
		t.Fatalf("DCR on main %d %s", rec.Code, rec.Body.String())
	}
	client := parse(rec)["client_id"].(string)
	verifier := strings.Repeat("b", 64)
	sum := sha256.Sum256([]byte(verifier))
	params := url.Values{"client_id": {client}, "redirect_uri": {"http://127.0.0.1:32124/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	rec = serve(eventsMux, "GET", "/api/collections/users/oauth2-server/authorize?"+params.Encode(), "", "", "")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != issuer+"/authorize?"+params.Encode() {
		t.Fatalf("events sign-in should redirect to main: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec = serve(mainMux, "GET", "/api/collections/users/oauth2-server/authorize?"+params.Encode(), "", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("main must serve its sign-in page: %d", rec.Code)
	}

	// 4. Sign in on main; the token works on the secondary app's /mcp.
	params.Set("identity", "multi@example.com")
	params.Set("password", "multi-password-12345")
	rec = serve(mainMux, "POST", "/api/collections/users/oauth2-server/authorize", params.Encode(), "application/x-www-form-urlencoded", "")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc == nil || loc.Query().Get("code") == "" {
		t.Fatalf("sign-in on main %d %s", rec.Code, rec.Body.String())
	}
	form := url.Values{"client_id": {client}, "redirect_uri": {"http://127.0.0.1:32124/callback"}, "grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "code_verifier": {verifier}}
	tok := serve(mainMux, "POST", "/api/collections/users/oauth2-server/token", form.Encode(), "application/x-www-form-urlencoded", "")
	if tok.Code != 200 {
		t.Fatalf("token on main %d %s", tok.Code, tok.Body.String())
	}
	token := parse(tok)["access_token"].(string)
	call := serve(eventsMux, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "application/json", token)
	if call.Code != 200 || parse(call)["error"] != nil {
		t.Fatalf("main-issued token rejected by events /mcp: %d %s", call.Code, call.Body.String())
	}
}

func buildMux(t *testing.T, app core.App, withMCP bool) http.Handler {
	t.Helper()
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	if withMCP {
		srv, err := New(app, forkCfg("native"))
		if err != nil {
			t.Fatal(err)
		}
		srv.RegisterRoutes(r)
	}
	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	return mux
}

func serve(mux http.Handler, method, path, body, ct, token string) *httptest.ResponseRecorder {
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
