package pocketbasemcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveOAuthMode(t *testing.T) {
	cases := []struct {
		enabled bool
		mode    string
		host    string
		want    string
		wantErr bool
	}{
		{true, "auto", "stock", "module", false},
		{true, "auto", "fork", "native", false},
		{true, "module", "stock", "module", false},
		{true, "module", "fork", "", true},
		{true, "native", "stock", "", true},
		{true, "native", "fork", "native", false},
		{true, "external", "stock", "external", false},
		{true, "off", "stock", "off", false},
		{false, "", "stock", "off", false},
		{true, "bogus", "stock", "", true},
	}
	for _, c := range cases {
		got, err := ResolveOAuthMode(c.enabled, c.mode, c.host)
		if c.wantErr && err == nil {
			t.Errorf("%+v: expected error", c)
		}
		if !c.wantErr && (err != nil || got != c.want) {
			t.Errorf("%+v: got %q err %v", c, got, err)
		}
	}
}

func TestEffectiveAuthCollectionsDefaultsToOAuthCollection(t *testing.T) {
	cfg := Config{Auth: AuthConfig{Collection: "users"}, OAuth: OAuthConfig{Enabled: true, Mode: "module"}}
	if got := cfg.EffectiveAuthCollections(); len(got) != 1 || got[0] != "users" {
		t.Fatalf("got %v", got)
	}
	cfg2 := Config{Auth: AuthConfig{Collections: []string{"staff"}}}
	if got := cfg2.EffectiveAuthCollections(); len(got) != 1 || got[0] != "staff" {
		t.Fatalf("got %v", got)
	}
	if got := (Config{}).EffectiveAuthCollections(); len(got) != 0 {
		t.Fatalf("expected fail-closed empty, got %v", got)
	}
}

func TestWWWAuthenticateWhenOAuthEnabled(t *testing.T) {
	cfg := &CollectionConfig{Name: "deals", Alias: "deals", FieldMap: map[string]FieldInfo{}, SortFields: []string{"id"}}
	srv := New(Config{
		Auth:  AuthConfig{Collection: "users"},
		OAuth: OAuthConfig{Enabled: true, Mode: "module", PublicBaseURL: "https://example.com"},
	}, &fakeHost{cfg: cfg})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code %d", rec.Code)
	}
	want := `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource/mcp"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
	}
	// OAuth off: no header.
	srv2 := newTestServer()
	rec2 := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)))
	if rec2.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("unexpected header %q", rec2.Header().Get("WWW-Authenticate"))
	}
}
