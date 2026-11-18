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
