package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

type fakeAdminAuth struct{}

func (fakeAdminAuth) Authenticate(r *http.Request) (Caller, error) {
	if r.Header.Get("Authorization") != "Bearer root" {
		return Caller{}, ErrForbidden
	}
	return Caller{Owner: ownerFor("root")}, nil
}

type fakeProviderAdmin struct {
	saved ProviderInput
}

func (f *fakeProviderAdmin) ListProviders(context.Context) ([]ProviderView, string, error) {
	return []ProviderView{{ID: "openai", Kind: "openai", Models: []ModelOption{{ID: "m1"}}}}, ChoiceID("openai", "m1"), nil
}

func (f *fakeProviderAdmin) GetProvider(context.Context, string) (ProviderView, error) {
	return ProviderView{ID: "openai", Kind: "openai"}, nil
}

func (f *fakeProviderAdmin) SaveProvider(_ context.Context, id string, in ProviderInput) (ProviderView, error) {
	f.saved = in
	if id == "" && in.ID == "" {
		return ProviderView{}, &AdminError{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "An id is required."}
	}
	return ProviderView{ID: id, Kind: "openai"}, nil
}

func (f *fakeProviderAdmin) DeleteProvider(context.Context, string) error { return nil }
func (f *fakeProviderAdmin) TestProvider(context.Context, string) (ProviderTest, error) {
	return ProviderTest{OK: true}, nil
}
func (f *fakeProviderAdmin) TestProviderConfig(context.Context, ProviderInput) (ProviderTest, error) {
	return ProviderTest{OK: true}, nil
}
func (f *fakeProviderAdmin) RefreshProviderModels(context.Context, string, bool) ([]ModelOption, error) {
	return []ModelOption{{ID: "m1"}}, nil
}
func (f *fakeProviderAdmin) SetDefaultChoice(context.Context, string, string) error { return nil }
func (f *fakeProviderAdmin) BeginSignIn(context.Context, string, string) (SignInStart, error) {
	return SignInStart{AuthURL: "https://auth.example"}, nil
}
func (f *fakeProviderAdmin) CompleteSignIn(context.Context, string, string, string) (ProviderView, error) {
	return ProviderView{ID: "codex"}, nil
}
func (f *fakeProviderAdmin) CancelSignIn(context.Context, string, string) error { return nil }
func (f *fakeProviderAdmin) SignOut(context.Context, string) error              { return nil }
func (f *fakeProviderAdmin) ImportCredentials(context.Context, string, json.RawMessage) (ProviderView, error) {
	return ProviderView{ID: "codex"}, nil
}

func TestProviderAdminRoutesUseSeparateAuthenticator(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: scripted.New()})
	admin := &fakeProviderAdmin{}
	h, err := NewHandler(HandlerConfig{
		Prefix:        testPrefix,
		Runner:        runner,
		Store:         store,
		Auth:          newFakeAuth(),
		AdminAuth:     fakeAdminAuth{},
		ProviderAdmin: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(auth, path, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := newRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result()
	}

	if res := call("Bearer user-a", testPrefix+"/providers", ""); res.StatusCode != http.StatusForbidden {
		t.Fatalf("user provider list = %d", res.StatusCode)
	}
	if res := call("Bearer root", testPrefix+"/providers", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("root provider list = %d", res.StatusCode)
	}
	if res := call("Bearer root", testPrefix+"/status", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("root chat status = %d", res.StatusCode)
	}
	if res := call("Bearer user-a", testPrefix+"/status", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("user chat status = %d", res.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, testPrefix+"/providers", strings.NewReader(`{"id":"openai","kind":"openai","label":"OpenAI","models":[{"id":"m1"}],"defaultModel":"m1"}`))
	req.Header.Set("Authorization", "Bearer root")
	rec := newRecorder()
	h.ServeHTTP(rec, req)
	if rec.code != http.StatusCreated {
		t.Fatalf("create provider = %d %s", rec.code, rec.body.String())
	}
	if admin.saved.ID != "openai" || admin.saved.Kind == nil || *admin.saved.Kind != "openai" {
		t.Fatalf("saved input = %+v", admin.saved)
	}
}

// newRecorder is a tiny ResponseWriter so this test does not depend on the
// package's other HTTP helpers.
type recorder struct {
	header http.Header
	code   int
	body   strings.Builder
}

func newRecorder() *recorder { return &recorder{header: http.Header{}, code: http.StatusOK} }

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.code = code }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *recorder) Result() *http.Response {
	return &http.Response{StatusCode: r.code, Header: r.header, Body: http.NoBody}
}
func (r *recorder) String() string { return r.body.String() }

func TestProviderAdminWrongMethodIs405(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: scripted.New()})
	h, err := NewHandler(HandlerConfig{
		Prefix:        testPrefix,
		Runner:        runner,
		Store:         store,
		Auth:          newFakeAuth(),
		AdminAuth:     fakeAdminAuth{},
		ProviderAdmin: &fakeProviderAdmin{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method, path string
		code         int
		allow        string
	}{
		{http.MethodDelete, "/providers", http.StatusMethodNotAllowed, "GET, HEAD, POST"},
		{http.MethodGet, "/providers/actions/test", http.StatusMethodNotAllowed, "POST"},
		{http.MethodPut, "/providers/openai", http.StatusMethodNotAllowed, "GET, HEAD, PATCH, DELETE"},
		{http.MethodGet, "/providers/openai/signin", http.StatusMethodNotAllowed, "POST"},
		{http.MethodGet, "/providers/openai/nope", http.StatusNotFound, ""},
		{http.MethodGet, "/providers/openai", http.StatusOK, ""},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, testPrefix+tc.path, nil)
		req.Header.Set("Authorization", "Bearer root")
		rec := newRecorder()
		h.ServeHTTP(rec, req)
		if rec.code != tc.code {
			t.Fatalf("%s %s = %d %s, want %d", tc.method, tc.path, rec.code, rec.String(), tc.code)
		}
		if got := rec.header.Get("Allow"); got != tc.allow {
			t.Fatalf("%s %s Allow = %q, want %q", tc.method, tc.path, got, tc.allow)
		}
	}
}

// titleProviderAdmin adds the optional TitleAdmin extension.
type titleProviderAdmin struct {
	fakeProviderAdmin
	choice string
	mode   string
}

func (f *titleProviderAdmin) TitleChoice(context.Context) (string, error) { return f.choice, nil }

func (f *titleProviderAdmin) SetTitleChoice(_ context.Context, mode, providerID, model string) error {
	f.mode = mode
	switch {
	case mode != "":
		f.choice = mode
	case providerID != "":
		f.choice = ChoiceID(providerID, model)
	default:
		f.choice = TitleChoiceAuto
	}
	return nil
}

func TestProviderAdminTitleChoice(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: scripted.New()})
	admin := &titleProviderAdmin{choice: TitleChoiceAuto}
	h, err := NewHandler(HandlerConfig{
		Prefix: testPrefix, Runner: runner, Store: store, Auth: newFakeAuth(),
		AdminAuth: fakeAdminAuth{}, ProviderAdmin: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *recorder {
		req, _ := http.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer root")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := newRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(http.MethodGet, testPrefix+"/providers", ""); !strings.Contains(rec.String(), `"titleChoice":"auto"`) {
		t.Fatalf("list = %d %s", rec.code, rec.String())
	}
	rec := do(http.MethodPost, testPrefix+"/providers/actions/title", `{"provider":"openai","model":"m1"}`)
	if rec.code != http.StatusOK || !strings.Contains(rec.String(), `"titleChoice":"openai::m1"`) {
		t.Fatalf("set = %d %s", rec.code, rec.String())
	}
	rec = do(http.MethodPost, testPrefix+"/providers/actions/title", `{"mode":"OFF"}`)
	if rec.code != http.StatusOK || admin.mode != TitleChoiceOff {
		t.Fatalf("off = %d %s mode=%q", rec.code, rec.String(), admin.mode)
	}
	if rec := do(http.MethodGet, testPrefix+"/providers/actions/title", ""); rec.code != http.StatusMethodNotAllowed {
		t.Fatalf("GET title action = %d", rec.code)
	}

	// An admin without the extension has no title route and no titleChoice.
	plain, err := NewHandler(HandlerConfig{
		Prefix: testPrefix, Runner: runner, Store: store, Auth: newFakeAuth(),
		AdminAuth: fakeAdminAuth{}, ProviderAdmin: &fakeProviderAdmin{},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, testPrefix+"/providers", nil)
	req.Header.Set("Authorization", "Bearer root")
	list := newRecorder()
	plain.ServeHTTP(list, req)
	if strings.Contains(list.String(), "titleChoice") {
		t.Fatalf("plain admin list = %s", list.String())
	}
}
