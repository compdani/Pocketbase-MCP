package pocketbase

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	pbmcp "github.com/compdani/pocketbase-mcp"
)

type oauthEnv struct {
	t       *testing.T
	app     core.App
	mux     http.Handler
	srv     *Server
	userID  string
	otherID string
}

func newOAuthEnv(t *testing.T) *oauthEnv {
	t.Helper()
	return newOAuthEnvWith(t, nil)
}

func newOAuthEnvWith(t *testing.T, mutate func(*pbmcp.Config)) *oauthEnv {
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
	empty := types.Pointer("")
	items := core.NewBaseCollection("items")
	items.ListRule = empty
	items.ViewRule = empty
	items.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "status"})
	autodates(items)
	must(app.Save(items))
	item := core.NewRecord(items)
	item.Set("title", "Widget")
	item.Set("status", "open")
	must(app.Save(item))

	users, err := app.FindCollectionByNameOrId("users")
	must(err)
	u := core.NewRecord(users)
	u.SetEmail("user@example.com")
	u.SetPassword("password123")
	must(app.Save(u))

	customers := core.NewAuthCollection("customers")
	must(app.Save(customers))
	cu := core.NewRecord(customers)
	cu.SetEmail("other@example.com")
	cu.SetPassword("password123")
	must(app.Save(cu))

	su, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	must(err)
	s := core.NewRecord(su)
	s.SetEmail("root@example.com")
	s.SetPassword("password123")
	must(app.Save(s))

	manifest := &pbmcp.Manifest{SchemaVersion: "1", ToolMode: "per_collection", Collections: []pbmcp.ManifestCollection{
		{Collection: "items", Operations: []string{"describe", "search", "fetch"},
			Presentation: pbmcp.Presentation{TitleFields: []string{"title"}, SummaryFields: []string{"title", "status"}},
			Fields:       pbmcp.FieldAllowlists{Read: []string{"id", "title", "status", "created", "updated"}},
			Search:       pbmcp.SearchSpec{TextFields: []string{"title"}}},
	}}
	cfg := pbmcp.Config{
		Path: "/mcp", AliasPaths: []string{"/sse"},
		Auth:     pbmcp.AuthConfig{Collection: "users"},
		OAuth:    pbmcp.OAuthConfig{Enabled: true, Mode: "module"},
		Manifest: manifest,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(app, cfg)
	must(err)
	r, err := apis.NewRouter(app)
	must(err)
	srv.RegisterRoutes(r)
	mux, err := r.BuildMux()
	must(err)
	return &oauthEnv{t: t, app: app, mux: mux, srv: srv, userID: u.Id, otherID: cu.Id}
}

func (e *oauthEnv) do(method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *oauthEnv) toolsList(token string) (int, *httptest.ResponseRecorder) {
	rec := e.do(http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
	return rec.Code, rec
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

type authzResult struct {
	code string
	raw  string
}

// authorize runs GET (login page) + POST (credentials) and returns the code.
func (e *oauthEnv) authorize(clientID, redirectURI, identity, password, challenge string) (authzResult, *httptest.ResponseRecorder) {
	e.t.Helper()
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI}, "state": {"xyz"}}
	if challenge != "" {
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
	}
	rec := e.do(http.MethodGet, "/oauth/login?"+q.Encode(), "", nil)
	if rec.Code != http.StatusOK {
		return authzResult{}, rec
	}
	m := csrfRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		e.t.Fatalf("no csrf in login page: %s", rec.Body.String())
	}
	cookie := rec.Header().Get("Set-Cookie")
	form := url.Values{"identity": {identity}, "password": {password}, "csrf_token": {m[1]}}
	rec2 := e.do(http.MethodPost, "/oauth/login", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": cookie})
	if rec2.Code != http.StatusFound {
		return authzResult{}, rec2
	}
	loc := rec2.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		e.t.Fatal(err)
	}
	return authzResult{code: u.Query().Get("code"), raw: loc}, rec2
}

func (e *oauthEnv) exchange(clientID, secret, code, redirectURI, verifier string) (int, map[string]any) {
	e.t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "client_id": {clientID}}
	if secret != "" {
		form.Set("client_secret", secret)
	}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	rec := e.do(http.MethodPost, "/oauth/token", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

const testRedirect = "https://example.com/callback"

func (e *oauthEnv) handClient(t *testing.T) (string, string) {
	t.Helper()
	id, secret, err := e.srv.Adapter.Provider().CreateHandClient("ChatGPT", []string{testRedirect})
	if err != nil {
		t.Fatal(err)
	}
	return id, secret
}

func TestOAuthChatGPTBaselineNoPKCE(t *testing.T) {
	env := newOAuthEnv(t)
	clientID, secret := env.handClient(t)
	// Root discovery as ChatGPT reads it.
	rec := env.do(http.MethodGet, "/.well-known/oauth-authorization-server", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"/oauth/token"`) && !strings.Contains(rec.Body.String(), "/oauth/token") {
		t.Fatalf("discovery: %d %s", rec.Code, rec.Body.String())
	}
	if rec2 := env.do(http.MethodGet, "/sse/.well-known/oauth-authorization-server", "", nil); rec2.Code != 200 {
		t.Fatalf("alias discovery: %d", rec2.Code)
	}
	// Unauthenticated /mcp carries the resource_metadata challenge.
	if rec3 := env.do(http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil); rec3.Code != 401 || !strings.Contains(rec3.Header().Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("401 challenge: %d %q", rec3.Code, rec3.Header().Get("WWW-Authenticate"))
	}
	res, postRec := env.authorize(clientID, testRedirect, "user@example.com", "password123", "")
	if res.code == "" {
		t.Fatalf("authorize failed: %d %s loc=%s", postRec.Code, postRec.Body.String(), postRec.Header().Get("Location"))
	}
	if strings.Contains(res.raw, "access_token") {
		t.Fatal("token must not appear in redirect")
	}
	code, out := env.exchange(clientID, secret, res.code, testRedirect, "")
	if code != 200 || out["access_token"] == nil {
		t.Fatalf("exchange: %d %v", code, out)
	}
	lcode, lrec := env.toolsList(out["access_token"].(string))
	if lcode != 200 || !strings.Contains(lrec.Body.String(), "search_items") {
		t.Fatalf("tools/list: %d %s", lcode, lrec.Body.String())
	}
}

func TestOAuthChatGPTBaselineWithPKCE(t *testing.T) {
	env := newOAuthEnv(t)
	clientID, secret := env.handClient(t)
	verifier := strings.Repeat("v", 50)
	res, postRec := env.authorize(clientID, testRedirect, "user@example.com", "password123", pkceChallenge(verifier))
	if res.code == "" {
		t.Fatalf("authorize: %d %s", postRec.Code, postRec.Body.String())
	}
	code, out := env.exchange(clientID, secret, res.code, testRedirect, verifier)
	if code != 200 || out["access_token"] == nil {
		t.Fatalf("exchange: %d %v", code, out)
	}
	if c, _ := env.toolsList(out["access_token"].(string)); c != 200 {
		t.Fatalf("tools/list: %d", c)
	}
}

func TestOAuthSelfRegistrationFlow(t *testing.T) {
	env := newOAuthEnv(t)
	// 1. unauthenticated /mcp -> WWW-Authenticate
	rec := env.do(http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	hdr := rec.Header().Get("WWW-Authenticate")
	m := regexp.MustCompile(`resource_metadata="([^"]+)"`).FindStringSubmatch(hdr)
	if rec.Code != 401 || m == nil {
		t.Fatalf("step1: %d %q", rec.Code, hdr)
	}
	// 2. protected-resource metadata (path taken from the challenge URL)
	u, _ := url.Parse(m[1])
	pr := env.do(http.MethodGet, u.Path, "", nil)
	var prDoc map[string]any
	if err := json.Unmarshal(pr.Body.Bytes(), &prDoc); err != nil || pr.Code != 200 {
		t.Fatalf("step2: %d %s", pr.Code, pr.Body.String())
	}
	if !strings.Contains(fmt.Sprint(prDoc["authorization_servers"]), "http://example.com") && len(prDoc["authorization_servers"].([]any)) == 0 {
		t.Fatalf("step2 doc: %v", prDoc)
	}
	// also the bare form
	if r := env.do(http.MethodGet, "/.well-known/oauth-protected-resource", "", nil); r.Code != 200 {
		t.Fatalf("bare protected-resource: %d", r.Code)
	}
	// 3. AS metadata
	as := env.do(http.MethodGet, "/.well-known/oauth-authorization-server", "", nil)
	var asDoc map[string]any
	_ = json.Unmarshal(as.Body.Bytes(), &asDoc)
	if asDoc["registration_endpoint"] == nil || fmt.Sprint(asDoc["code_challenge_methods_supported"]) != "[S256]" {
		t.Fatalf("step3: %v", asDoc)
	}
	if fmt.Sprint(asDoc["token_endpoint_auth_methods_supported"]) == "" || !strings.Contains(fmt.Sprint(asDoc["token_endpoint_auth_methods_supported"]), "none") {
		t.Fatalf("step3 auth methods: %v", asDoc)
	}
	// 4. DCR (public client)
	dcr := env.do(http.MethodPost, "/oauth/register", `{"client_name":"Test MCP Client","redirect_uris":["https://client.example.com/callback"],"token_endpoint_auth_method":"none"}`, map[string]string{"Content-Type": "application/json"})
	if dcr.Code != http.StatusCreated {
		t.Fatalf("step4 DCR: %d %s", dcr.Code, dcr.Body.String())
	}
	var dcrDoc map[string]any
	_ = json.Unmarshal(dcr.Body.Bytes(), &dcrDoc)
	clientID := dcrDoc["client_id"].(string)
	// 5. PKCE authorization (no secret)
	verifier := strings.Repeat("w", 50)
	res, postRec := env.authorize(clientID, "https://client.example.com/callback", "user@example.com", "password123", pkceChallenge(verifier))
	if res.code == "" {
		t.Fatalf("step5: %d %s", postRec.Code, postRec.Body.String())
	}
	code, out := env.exchange(clientID, "", res.code, "https://client.example.com/callback", verifier)
	if code != 200 || out["access_token"] == nil {
		t.Fatalf("step5 exchange: %d %v", code, out)
	}
	// 6. authorized tools/list
	if c, _ := env.toolsList(out["access_token"].(string)); c != 200 {
		t.Fatalf("step6: %d", c)
	}
}

func TestOAuthRejections(t *testing.T) {
	t.Run("redirect mismatch", func(t *testing.T) {
		env := newOAuthEnv(t)
		clientID, _ := env.handClient(t)
		q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://example.com/evil"}, "state": {"s"}}
		if rec := env.do(http.MethodGet, "/oauth/login?"+q.Encode(), "", nil); rec.Code != 400 {
			t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("legacy domain-only redirect", func(t *testing.T) {
		env := newOAuthEnv(t)
		id, _, err := env.srv.Adapter.Provider().CreateHandClient("Legacy", []string{"https://example.com"})
		if err != nil {
			t.Fatal(err)
		}
		q := url.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {"https://example.com/callback"}, "state": {"s"}}
		rec := env.do(http.MethodGet, "/oauth/login?"+q.Encode(), "", nil)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "legacy domain-only") || !strings.Contains(rec.Body.String(), "full callback URL") {
			t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("replayed and expired code", func(t *testing.T) {
		env := newOAuthEnv(t)
		clientID, secret := env.handClient(t)
		res, _ := env.authorize(clientID, testRedirect, "user@example.com", "password123", "")
		if code, out := env.exchange(clientID, secret, res.code, testRedirect, ""); code != 200 {
			t.Fatalf("first exchange: %d %v", code, out)
		}
		if code, _ := env.exchange(clientID, secret, res.code, testRedirect, ""); code == 200 {
			t.Fatal("replayed code accepted")
		}
		// expired
		res2, _ := env.authorize(clientID, testRedirect, "user@example.com", "password123", "")
		sum := sha256.Sum256([]byte(res2.code))
		rec, err := env.app.FindFirstRecordByData("mcp_oauth_codes", "code_hash", hex.EncodeToString(sum[:]))
		if err != nil {
			t.Fatal(err)
		}
		rec.Set("expires_at", "2000-01-01 00:00:00.000Z")
		if err := env.app.Save(rec); err != nil {
			t.Fatal(err)
		}
		if code, _ := env.exchange(clientID, secret, res2.code, testRedirect, ""); code == 200 {
			t.Fatal("expired code accepted")
		}
	})
	t.Run("wrong PKCE verifier and missing PKCE for public client", func(t *testing.T) {
		env := newOAuthEnv(t)
		clientID, secret := env.handClient(t)
		verifier := strings.Repeat("v", 50)
		res, _ := env.authorize(clientID, testRedirect, "user@example.com", "password123", pkceChallenge(verifier))
		if code, _ := env.exchange(clientID, secret, res.code, testRedirect, strings.Repeat("x", 50)); code == 200 {
			t.Fatal("wrong verifier accepted")
		}
		// public client without PKCE at authorize time
		pubID, _, err := env.srv.Adapter.Provider().CreateClient("Pub", []string{"https://example.com/callback"}, true)
		if err != nil {
			t.Fatal(err)
		}
		q := url.Values{"response_type": {"code"}, "client_id": {pubID}, "redirect_uri": {testRedirect}, "state": {"s"}}
		if rec := env.do(http.MethodGet, "/oauth/login?"+q.Encode(), "", nil); rec.Code != 400 {
			t.Fatalf("missing PKCE: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("revoked token key between login and exchange", func(t *testing.T) {
		env := newOAuthEnv(t)
		clientID, secret := env.handClient(t)
		res, _ := env.authorize(clientID, testRedirect, "user@example.com", "password123", "")
		user, err := env.app.FindRecordById("users", env.userID)
		if err != nil {
			t.Fatal(err)
		}
		user.RefreshTokenKey()
		if err := env.app.Save(user); err != nil {
			t.Fatal(err)
		}
		if code, _ := env.exchange(clientID, secret, res.code, testRedirect, ""); code == 200 {
			t.Fatal("exchange after token-key rotation accepted")
		}
	})
	t.Run("account from another collection and superuser cannot login", func(t *testing.T) {
		env := newOAuthEnv(t)
		clientID, _ := env.handClient(t)
		if res, rec := env.authorize(clientID, testRedirect, "other@example.com", "password123", ""); res.code != "" || rec.Code == http.StatusFound {
			t.Fatalf("other-collection login issued a code: %+v %d", res, rec.Code)
		}
		if res, rec := env.authorize(clientID, testRedirect, "root@example.com", "password123", ""); res.code != "" || rec.Code == http.StatusFound {
			t.Fatalf("superuser login issued a code: %+v %d", res, rec.Code)
		}
	})
	t.Run("wrong client secret", func(t *testing.T) {
		env := newOAuthEnv(t)
		clientID, _ := env.handClient(t)
		res, _ := env.authorize(clientID, testRedirect, "user@example.com", "password123", "")
		if code, _ := env.exchange(clientID, "wrong-secret-value", res.code, testRedirect, ""); code == 200 {
			t.Fatal("wrong secret accepted")
		}
	})
	t.Run("DCR with non-https redirect", func(t *testing.T) {
		env := newOAuthEnv(t)
		rec := env.do(http.MethodPost, "/oauth/register", `{"redirect_uris":["http://evil.example.com/cb"],"token_endpoint_auth_method":"none"}`, map[string]string{"Content-Type": "application/json"})
		if rec.Code != 400 {
			t.Fatalf("code %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("CSRF protection", func(t *testing.T) {
		env := newOAuthEnv(t)
		clientID, _ := env.handClient(t)
		q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "state": {"s"}}
		rec := env.do(http.MethodGet, "/oauth/login?"+q.Encode(), "", nil)
		form := url.Values{"identity": {"user@example.com"}, "password": {"password123"}, "csrf_token": {"bogus"}}
		rec2 := env.do(http.MethodPost, "/oauth/login", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": rec.Header().Get("Set-Cookie")})
		if rec2.Code != http.StatusForbidden {
			t.Fatalf("code %d", rec2.Code)
		}
	})
}

func TestOAuthMFARequiredAccountRejected(t *testing.T) {
	env := newOAuthEnv(t)
	users, err := env.app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	users.MFA.Enabled = true
	users.MFA.Rule = ""
	users.OTP.Enabled = true // MFA requires at least 2 auth methods
	if err := env.app.Save(users); err != nil {
		t.Fatal(err)
	}
	clientID, _ := env.handClient(t)
	res, rec := env.authorize(clientID, testRedirect, "user@example.com", "password123", "")
	if res.code != "" || rec.Code == http.StatusFound || !strings.Contains(rec.Body.String(), "multi-factor") {
		t.Fatalf("MFA account: code=%q status=%d body=%s", res.code, rec.Code, rec.Body.String())
	}
}

func TestOAuthModeResolutionStock(t *testing.T) {
	mk := func(t *testing.T, mode string) (core.App, pbmcp.Config) {
		app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: t.TempDir()})
		if err := app.Bootstrap(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { app.ResetBootstrapState() })
		cfg := pbmcp.Config{Auth: pbmcp.AuthConfig{Collection: "users"}, OAuth: pbmcp.OAuthConfig{Enabled: true, Mode: mode}, Manifest: &pbmcp.Manifest{SchemaVersion: "1"}}
		return app, cfg
	}
	t.Run("native is a startup error on stock", func(t *testing.T) {
		app, cfg := mk(t, "native")
		if _, err := New(app, cfg); err == nil || !strings.Contains(err.Error(), "native") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("duplicate registration guard", func(t *testing.T) {
		app, cfg := mk(t, "module")
		if _, err := New(app, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := New(app, cfg); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("superusers collection rejected", func(t *testing.T) {
		app, cfg := mk(t, "module")
		cfg.Auth.Collection = "_superusers"
		cfg.OAuth.Collection = "_superusers"
		if _, err := New(app, cfg); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestOAuthMigrationAndStoragePrivacy(t *testing.T) {
	env := newOAuthEnv(t)
	p := env.srv.Adapter.Provider()
	if err := p.ImportHandClient("old-chatgpt-id", "existing-secret", "Migrated", []string{testRedirect}); err != nil {
		t.Fatal(err)
	}
	result, _ := env.authorize("old-chatgpt-id", testRedirect, "user@example.com", "password123", "")
	if status, _ := env.exchange("old-chatgpt-id", "existing-secret", result.code, testRedirect, ""); status != 200 {
		t.Fatalf("migrated client exchange %d", status)
	}
	if err := p.ImportHandClient("legacy", "secret", "Legacy", []string{"https://example.com"}); err == nil {
		t.Fatal("legacy migration accepted")
	}
	col, _ := env.app.FindCollectionByNameOrId("mcp_oauth_clients")
	cfg := pbmcp.Config{Auth: pbmcp.AuthConfig{Collections: []string{"users"}}, Path: "/private-test", Manifest: &pbmcp.Manifest{SchemaVersion: "1", Collections: []pbmcp.ManifestCollection{{Collection: col.Id, Operations: []string{"search"}}}}}
	if _, err := New(env.app, cfg); err == nil {
		t.Fatal("private OAuth storage exposed")
	}
}

func TestOAuthPasswordHookPreserved(t *testing.T) {
	env := newOAuthEnv(t)
	env.app.OnRecordAuthWithPasswordRequest().BindFunc(func(e *core.RecordAuthWithPasswordRequestEvent) error {
		return e.ForbiddenError("Blocked by host policy", nil)
	})
	id, _ := env.handClient(t)
	if result, _ := env.authorize(id, testRedirect, "user@example.com", "password123", ""); result.code != "" {
		t.Fatal("host auth hook bypassed")
	}
}

// A self-registered client chooses its own name, so the login page must flag
// it as unverified and show where the code goes; the page must not be framable.
func TestOAuthLoginPageFlagsSelfRegisteredClients(t *testing.T) {
	env := newOAuthEnv(t)
	dcr := env.do(http.MethodPost, "/oauth/register", `{"client_name":"ChatGPT","redirect_uris":["https://attacker.example/cb"],"token_endpoint_auth_method":"none"}`, map[string]string{"Content-Type": "application/json"})
	if dcr.Code != http.StatusCreated {
		t.Fatalf("DCR: %d %s", dcr.Code, dcr.Body.String())
	}
	var doc map[string]any
	_ = json.Unmarshal(dcr.Body.Bytes(), &doc)
	q := url.Values{"response_type": {"code"}, "client_id": {doc["client_id"].(string)}, "redirect_uri": {"https://attacker.example/cb"},
		"code_challenge": {pkceChallenge(strings.Repeat("v", 50))}, "code_challenge_method": {"S256"}}
	page := env.do(http.MethodGet, "/oauth/login?"+q.Encode(), "", nil)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "unverified app") || !strings.Contains(body, "attacker.example") {
		t.Fatalf("self-registered login page must warn and show the redirect host: %d %s", page.Code, body)
	}
	if page.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("login page must not be framable: %v", page.Header())
	}

	clientID, _ := env.handClient(t)
	hq := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {testRedirect}}
	hand := env.do(http.MethodGet, "/oauth/login?"+hq.Encode(), "", nil)
	if hand.Code != http.StatusOK {
		t.Fatalf("hand client page: %d %s", hand.Code, hand.Body.String())
	}
	if strings.Contains(hand.Body.String(), "unverified app") {
		t.Fatal("hand-created client must not be flagged unverified")
	}
}

func TestOAuthRegistrationControls(t *testing.T) {
	env := newOAuthEnvWith(t, func(c *pbmcp.Config) {
		c.OAuth.RegistrationRedirectHosts = []string{"client.example.com"}
	})
	reg := func(uri string) int {
		return env.do(http.MethodPost, "/oauth/register", `{"redirect_uris":["`+uri+`"],"token_endpoint_auth_method":"none"}`, map[string]string{"Content-Type": "application/json"}).Code
	}
	if c := reg("https://attacker.example/cb"); c != http.StatusBadRequest {
		t.Fatalf("host outside allowlist: want 400, got %d", c)
	}
	if c := reg("https://CLIENT.example.com/cb"); c != http.StatusCreated {
		t.Fatalf("allowlisted host: want 201, got %d", c)
	}
	if c := reg("http://127.0.0.1:33418/cb"); c != http.StatusCreated {
		t.Fatalf("loopback always allowed: want 201, got %d", c)
	}

	off := newOAuthEnvWith(t, func(c *pbmcp.Config) { c.OAuth.DisableRegistration = true })
	rec := off.do(http.MethodPost, "/oauth/register", `{"redirect_uris":["https://client.example.com/cb"],"token_endpoint_auth_method":"none"}`, map[string]string{"Content-Type": "application/json"})
	if rec.Code == http.StatusCreated {
		t.Fatal("registration must be unavailable when disabled")
	}
	disc := off.do(http.MethodGet, "/.well-known/oauth-authorization-server", "", nil)
	if strings.Contains(disc.Body.String(), "registration_endpoint") {
		t.Fatalf("disabled registration must not be advertised: %s", disc.Body.String())
	}
}
