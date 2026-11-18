package codex

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type authServer struct {
	*httptest.Server
	key          *rsa.PrivateKey
	nonce        string
	tokenCalls   int
	revokeCalls  int
	clientIDSeen string
}

func newAuthServer(t *testing.T) *authServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	as := &authServer{key: key}
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":              base,
			"jwks_uri":            base + "/jwks",
			"revocation_endpoint": base + "/revoke",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		e := big.NewInt(int64(key.PublicKey.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "test-key",
			"kty": "RSA",
			"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(e),
		}}})
	})
	mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		as.tokenCalls++
		_ = r.ParseForm()
		clientID := r.Form.Get("client_id")
		as.clientIDSeen = clientID
		grant := r.Form.Get("grant_type")
		nonce := ""
		if grant == "authorization_code" {
			nonce = as.nonce
		}
		claims := map[string]any{
			"iss":   base,
			"sub":   "user-1",
			"email": "user@example.com",
			"aud":   []string{clientID},
			"exp":   time.Now().Add(time.Hour).Unix(),
			"nonce": nonce,
		}
		idToken := signJWT(t, key, map[string]any{"alg": "RS256", "kid": "test-key"}, claims)
		refresh := "refresh-1"
		if grant == "refresh_token" {
			refresh = "refresh-2"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-" + grant,
			"refresh_token": refresh,
			"id_token":      idToken,
			"expires_in":    3600,
			"scope":         Scopes,
		})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		as.revokeCalls++
		w.WriteHeader(http.StatusOK)
	})
	as.Server = httptest.NewServer(mux)
	base = as.Server.URL
	return as
}

func TestOAuthCompleteRefreshAndRevoke(t *testing.T) {
	as := newAuthServer(t)
	defer as.Close()
	o := &OAuth{AuthBase: as.URL, HTTP: as.Client()}
	p := o.Begin("urn:uuid:1234", "http://127.0.0.1:1455/cb", nil)
	as.nonce = p.Nonce
	cred, err := o.Complete(context.Background(), p, url.Values{
		"state":     {p.State},
		"code":      {"code-1"},
		"client_id": {"oaiapp_test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cred.HasPlan() || cred.Email != "user@example.com" || cred.AccessToken != "access-authorization_code" {
		t.Fatalf("credentials = %+v", cred)
	}
	if cred.RefreshToken != "refresh-1" {
		t.Fatalf("refresh token = %q", cred.RefreshToken)
	}

	next, err := o.Refresh(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if next.AccessToken != "access-refresh_token" || next.RefreshToken != "refresh-2" {
		t.Fatalf("refreshed = %+v", next)
	}
	if err := o.Revoke(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if as.revokeCalls != 1 {
		t.Fatalf("revoke calls = %d", as.revokeCalls)
	}
}

func TestOAuthCompleteRejectsState(t *testing.T) {
	as := newAuthServer(t)
	defer as.Close()
	o := &OAuth{AuthBase: as.URL, HTTP: as.Client()}
	p := o.Begin("urn:uuid:1234", "http://127.0.0.1:1455/cb", nil)
	if _, err := o.Complete(context.Background(), p, url.Values{"state": {"wrong"}, "code": {"c"}}); err == nil {
		t.Fatal("state mismatch accepted")
	}
}

type memCredStore struct {
	mu   sync.Mutex
	cred *Credentials
}

func (m *memCredStore) Load(context.Context) (*Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cred == nil {
		return nil, ErrNotSignedIn
	}
	copy := *m.cred
	return &copy, nil
}

func (m *memCredStore) Save(_ context.Context, c *Credentials) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copy := *c
	m.cred = &copy
	return nil
}

func (m *memCredStore) Clear(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cred = nil
	return nil
}

func TestManagerTokenRefreshAndStatus(t *testing.T) {
	as := newAuthServer(t)
	defer as.Close()
	store := &memCredStore{cred: &Credentials{
		ClientID:     "oaiapp_test",
		Email:        "user@example.com",
		AccessToken:  "old",
		RefreshToken: "refresh-1",
		Scopes:       strings.Fields(Scopes),
		ExpiresAt:    time.Now().Add(5 * time.Minute),
	}}
	manager := NewManager(store, &OAuth{AuthBase: as.URL, HTTP: as.Client()}, "urn:uuid:1234", "codex")
	token, err := manager.Token(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-refresh_token" {
		t.Fatalf("token = %q", token)
	}
	saved, _ := store.Load(context.Background())
	if saved.RefreshToken != "refresh-2" {
		t.Fatalf("rotated refresh token not saved: %+v", saved)
	}
	status := manager.Status(context.Background())
	if !status.SignedIn || !status.Plan || status.Email != "user@example.com" {
		t.Fatalf("status = %+v", status)
	}
}

func TestManagerTokenUsesValidCredential(t *testing.T) {
	as := newAuthServer(t)
	defer as.Close()
	store := &memCredStore{cred: &Credentials{
		ClientID:     "oaiapp_test",
		AccessToken:  "still-valid",
		RefreshToken: "refresh-1",
		Scopes:       strings.Fields(Scopes),
		ExpiresAt:    time.Now().Add(time.Hour),
	}}
	manager := NewManager(store, &OAuth{AuthBase: as.URL, HTTP: as.Client()}, "urn:uuid:1234", "codex")
	token, err := manager.Token(context.Background(), false)
	if err != nil || token != "still-valid" {
		t.Fatalf("token = %q err = %v", token, err)
	}
	if as.tokenCalls != 0 {
		t.Fatalf("unexpected refresh calls = %d", as.tokenCalls)
	}
	if _, err := manager.Token(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if as.tokenCalls != 1 {
		t.Fatalf("force refresh calls = %d", as.tokenCalls)
	}
}

func TestManagerSignInAndCancel(t *testing.T) {
	as := newAuthServer(t)
	defer as.Close()
	store := &memCredStore{}
	manager := NewManager(store, &OAuth{AuthBase: as.URL, HTTP: as.Client()}, "urn:uuid:1234", "codex")
	defer manager.Close()
	start, err := manager.BeginSignIn(context.Background(), "admin-1")
	if err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(start.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	as.nonce = authURL.Query().Get("nonce")
	if err := manager.CompleteSignIn(context.Background(), "admin-1", "http://127.0.0.1/cb?state="+url.QueryEscape(state)+"&code=code-1&client_id=oaiapp_test"); err != nil {
		t.Fatal(err)
	}
	status := manager.Status(context.Background())
	if !status.SignedIn || !status.Plan {
		t.Fatalf("status = %+v", status)
	}
	if err := manager.SignOut(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.Status(context.Background()).SignedIn {
		t.Fatal("sign-out kept the account")
	}
	start, err = manager.BeginSignIn(context.Background(), "admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CancelSignIn(context.Background(), "admin-2"); err == nil {
		t.Fatal("another admin cancelled the attempt")
	}
	if err := manager.CancelSignIn(context.Background(), "admin-1"); err != nil {
		t.Fatal(err)
	}
}

func TestListModelsSkipsHidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-a","display_name":"GPT A","visibility":"list"},{"slug":"hidden","visibility":"hide"},{"id":"gpt-b"}]}`))
	}))
	defer srv.Close()
	models, err := ListModels(context.Background(), nil, srv.URL, "tok")
	if err != nil || len(models) != 2 || models[0].Slug != "gpt-a" || models[1].DisplayName != "gpt-b" {
		t.Fatalf("models = %+v err = %v", models, err)
	}
}

// A loopback goroutine from an abandoned attempt must not fail a newer one
// when its listener is closed.
func TestManagerStaleLoopbackDoesNotFailNewAttempt(t *testing.T) {
	as := newAuthServer(t)
	defer as.Close()
	manager := NewManager(&memCredStore{}, &OAuth{AuthBase: as.URL, HTTP: as.Client()}, "urn:uuid:1234", "codex")
	defer manager.Close()
	if _, err := manager.BeginSignIn(context.Background(), "admin-1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.CancelSignIn(context.Background(), "admin-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginSignIn(context.Background(), "admin-1"); err != nil {
		t.Fatal(err)
	}
	// Give the first attempt's goroutine time to observe its closed listener.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !manager.Status(context.Background()).Pending {
			t.Fatal("the cancelled attempt's goroutine failed the new attempt")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status := manager.Status(context.Background()); status.Error != "" {
		t.Fatalf("status error = %q", status.Error)
	}
}
