package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider/codex"
)

func testScope() Scope { return Scope{Namespace: "ns", Mount: "/mcp"} }

func staticProviders() []Provider {
	return []Provider{
		{
			ID: "a", Label: "A", Kind: KindOpenAI, Enabled: true,
			Models: []Model{{ID: "m1", Label: "Model 1"}, {ID: "shared"}}, DefaultModel: "m1",
		},
		{
			ID: "b", Label: "B", Kind: KindOpenAI, Enabled: true,
			Models: []Model{{ID: "shared"}, {ID: "dup"}}, DefaultModel: "shared",
		},
		{
			ID: "c", Label: "C", Kind: KindOpenAI, Enabled: true,
			Models: []Model{{ID: "dup"}}, DefaultModel: "dup",
		},
	}
}

func TestRouterChoicesDefaultsAndResolution(t *testing.T) {
	r, err := NewRouter(context.Background(), RouterOptions{Scope: testScope(), Static: staticProviders()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	choices, err := r.Choices(context.Background())
	if err != nil || len(choices) != 5 {
		t.Fatalf("choices = %+v err = %v", choices, err)
	}
	if choices[0].ID != chat.ChoiceID("a", "m1") || !choices[0].Default {
		t.Fatalf("first choice = %+v", choices[0])
	}
	if choices[0].Group != "A" || choices[0].Label != "Model 1" {
		t.Fatalf("choice labels = %+v", choices[0])
	}
	if len(choices[0].AttachmentTypes) == 0 {
		t.Fatal("openai choice should advertise attachments")
	}

	def, err := r.Default(context.Background())
	if err != nil || def.ID != chat.ChoiceID("a", "m1") {
		t.Fatalf("default = %+v err = %v", def, err)
	}

	resolved, err := r.Resolve(context.Background(), "", "")
	if err != nil || resolved.Model != "m1" || resolved.Choice.Provider != "a" {
		t.Fatalf("default resolve = %+v err = %v", resolved, err)
	}
	resolved, err = r.Resolve(context.Background(), "b", "")
	if err != nil || resolved.Model != "shared" || resolved.Choice.Provider != "b" {
		t.Fatalf("provider default = %+v err = %v", resolved, err)
	}
	if _, err = r.Resolve(context.Background(), "", "dup"); !errors.Is(err, chat.ErrModelAmbiguous) {
		t.Fatalf("ambiguous err = %v", err)
	}
	if _, err := r.Resolve(context.Background(), "a", "shared"); err != nil {
		t.Fatalf("explicit shared: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "missing", ""); !errors.Is(err, chat.ErrModelUnknown) {
		t.Fatalf("missing err = %v", err)
	}
	if !r.SupportsAttachment("text/plain") || !r.SupportsAttachment("application/pdf") {
		t.Fatal("openai attachment support missing")
	}
}

type fakeStore struct {
	mu        sync.Mutex
	providers map[string]Provider
	creds     map[string]*codex.Credentials
	settings  map[string]json.RawMessage
}

func newFakeStore() *fakeStore {
	return &fakeStore{providers: map[string]Provider{}, creds: map[string]*codex.Credentials{}, settings: map[string]json.RawMessage{}}
}

func (s *fakeStore) ListProviders(context.Context, Scope) ([]Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Provider, 0, len(s.providers))
	for _, p := range s.providers {
		out = append(out, p)
	}
	return out, nil
}

func (s *fakeStore) GetProvider(_ context.Context, _ Scope, id string) (Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providers[id]
	if !ok {
		return Provider{}, ErrProviderNotFound
	}
	return p, nil
}

func (s *fakeStore) SaveProvider(_ context.Context, _ Scope, p Provider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providers[p.ID] = p
	return nil
}

func (s *fakeStore) DeleteProvider(_ context.Context, _ Scope, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.providers[id]; !ok {
		return ErrProviderNotFound
	}
	delete(s.providers, id)
	delete(s.creds, id)
	return nil
}

func (s *fakeStore) LoadCredentials(_ context.Context, _ Scope, id string) (*codex.Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.creds[id]; c != nil {
		copy := *c
		return &copy, nil
	}
	return nil, codex.ErrNotSignedIn
}

func (s *fakeStore) SaveCredentials(_ context.Context, _ Scope, id string, c *codex.Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *c
	s.creds[id] = &copy
	return nil
}

func (s *fakeStore) ClearCredentials(_ context.Context, _ Scope, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.creds, id)
	return nil
}

func (s *fakeStore) LoadSetting(_ context.Context, _ Scope, key string, dst any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.settings[key]
	if !ok {
		return ErrSettingNotFound
	}
	return json.Unmarshal(raw, dst)
}

func (s *fakeStore) SaveSetting(_ context.Context, _ Scope, key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.settings[key] = raw
	return nil
}

func (s *fakeStore) HostID(context.Context, Scope) (string, error) { return "urn:uuid:test", nil }

func TestAdminCRUDRedactionAndModels(t *testing.T) {
	modelsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"m1","name":"Model One"},{"id":"m2"}]}`)
	}))
	defer modelsSrv.Close()

	store := newFakeStore()
	admin, err := NewAdmin(RouterOptions{Scope: testScope(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Router().Close()

	enabled := true
	in := chat.ProviderInput{
		ID:           "openai",
		Label:        str("OpenAI"),
		Kind:         str("openai"),
		Enabled:      &enabled,
		BaseURL:      str(modelsSrv.URL),
		Models:       &[]chat.ModelOption{{ID: "m1", Label: "Model One"}},
		DefaultModel: str("m1"),
		Headers:      map[string]string{"X-Test": "one"},
		APIKey:       str("sk-secret"),
	}
	view, err := admin.SaveProvider(context.Background(), "", in)
	if err != nil {
		t.Fatal(err)
	}
	if !view.HasAPIKey || view.APIKeyHint != "…cret" {
		t.Fatalf("view redaction = %+v", view)
	}
	if len(view.HeaderNames) != 1 || view.HeaderNames[0] != "X-Test" {
		t.Fatalf("header names = %+v", view.HeaderNames)
	}
	if view.Models[0].Label != "Model One" {
		t.Fatalf("models = %+v", view.Models)
	}

	// An omitted key is unchanged; an explicitly empty key clears it.
	view, err = admin.SaveProvider(context.Background(), "openai", chat.ProviderInput{Label: str("Renamed")})
	if err != nil {
		t.Fatal(err)
	}
	if !view.HasAPIKey {
		t.Fatal("omitted key cleared the stored key")
	}
	view, err = admin.SaveProvider(context.Background(), "openai", chat.ProviderInput{APIKey: str("")})
	if err != nil {
		t.Fatal(err)
	}
	if view.HasAPIKey {
		t.Fatal("explicit empty key did not clear")
	}
	if _, err := admin.SaveProvider(context.Background(), "openai", chat.ProviderInput{APIKey: str("sk-secret")}); err != nil {
		t.Fatal(err)
	}

	test, err := admin.TestProvider(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !test.OK || len(test.Models) != 2 {
		t.Fatalf("test = %+v", test)
	}

	refreshed, err := admin.RefreshProviderModels(context.Background(), "openai", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 2 {
		t.Fatalf("refreshed = %+v", refreshed)
	}
	choices, _ := admin.Router().Choices(context.Background())
	if len(choices) != 2 {
		t.Fatalf("choices after refresh = %+v", choices)
	}

	if err := admin.SetDefaultChoice(context.Background(), "openai", "m2"); err != nil {
		t.Fatal(err)
	}
	def, err := admin.Router().Default(context.Background())
	if err != nil || def.Model != "m2" {
		t.Fatalf("default = %+v err = %v", def, err)
	}
	list, defaultID, err := admin.ListProviders(context.Background())
	if err != nil || len(list) != 1 || defaultID != chat.ChoiceID("openai", "m2") {
		t.Fatalf("list = %+v default = %q err = %v", list, defaultID, err)
	}

	if err := admin.DeleteProvider(context.Background(), "openai"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Router().Default(context.Background()); !errors.Is(err, chat.ErrNoModelConfigured) {
		t.Fatalf("default after delete = %v", err)
	}
}

func TestAdminCreateDiscoversModelsWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"m1","name":"Model One"}]}`)
	}))
	defer srv.Close()
	store := newFakeStore()
	admin, err := NewAdmin(RouterOptions{Scope: testScope(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Router().Close()
	enabled := true
	view, err := admin.SaveProvider(context.Background(), "", chat.ProviderInput{
		ID:      "openai",
		Kind:    str("openai"),
		Enabled: &enabled,
		BaseURL: str(srv.URL),
		APIKey:  str("sk"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Models) != 1 || view.Models[0].ID != "m1" || view.DefaultModel != "m1" {
		t.Fatalf("view = %+v", view)
	}
}

func TestRefreshModelsResetsMissingDefault(t *testing.T) {
	var body = `{"data":[{"id":"m2"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()
	store := newFakeStore()
	p := Provider{ID: "openai", Kind: KindOpenAI, Enabled: true, BaseURL: srv.URL, Models: []Model{{ID: "m1"}}, DefaultModel: "m1"}
	store.providers[p.ID] = p
	admin, err := NewAdminWithRouter(mustRouter(t, store, p), RouterOptions{Scope: testScope(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Router().Close()
	models, err := admin.RefreshProviderModels(context.Background(), p.ID, true)
	if err != nil || len(models) != 1 || models[0].ID != "m2" {
		t.Fatalf("models = %+v err = %v", models, err)
	}
	saved, _ := store.GetProvider(context.Background(), testScope(), p.ID)
	if saved.DefaultModel != "m2" {
		t.Fatalf("default = %q", saved.DefaultModel)
	}
	body = `{"data":[]}`
	if _, err := admin.RefreshProviderModels(context.Background(), p.ID, true); err == nil {
		t.Fatal("empty refresh saved an empty model list")
	}
	saved, _ = store.GetProvider(context.Background(), testScope(), p.ID)
	if len(saved.Models) != 1 || saved.Models[0].ID != "m2" {
		t.Fatalf("models after empty refresh = %+v", saved.Models)
	}
}

func mustRouter(t *testing.T, store Store, providers ...Provider) *Router {
	t.Helper()
	r, err := NewRouter(context.Background(), RouterOptions{Scope: testScope(), Store: store, Static: providers})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRouterRequiresCodexStore(t *testing.T) {
	_, err := NewRouter(context.Background(), RouterOptions{Scope: testScope(), Static: []Provider{{
		ID: "codex", Kind: KindCodex, Enabled: true, Models: []Model{{ID: "gpt-codex"}},
	}}})
	if err == nil {
		t.Fatal("codex without a store was accepted")
	}
}

func TestCodexUnsignedIsNotAChoice(t *testing.T) {
	store := newFakeStore()
	store.providers["codex"] = Provider{
		ID: "codex", Kind: KindCodex, Enabled: true, Models: []Model{{ID: "gpt-codex"}},
	}
	r, err := NewRouter(context.Background(), RouterOptions{
		Scope: testScope(), Store: store, Static: []Provider{store.providers["codex"]},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if choices, _ := r.Choices(context.Background()); len(choices) != 0 {
		t.Fatalf("unsigned codex leaked choices: %+v", choices)
	}
	if errText := r.ProviderError("codex"); !strings.Contains(errText, "Not signed in") {
		t.Fatalf("provider error = %q", errText)
	}

	store.creds["codex"] = &codex.Credentials{
		ClientID:     "oaiapp_test",
		AccessToken:  "access",
		RefreshToken: "refresh",
		Scopes:       strings.Fields(codex.Scopes),
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	choices, _ := r.Choices(context.Background())
	if len(choices) != 1 || choices[0].Provider != "codex" {
		t.Fatalf("signed-in choices = %+v", choices)
	}
}

func TestDisabledCodexWithoutStoreDoesNotFail(t *testing.T) {
	r, err := NewRouter(context.Background(), RouterOptions{Scope: testScope(), Static: []Provider{{
		ID: "codex", Kind: KindCodex, Enabled: false, Models: []Model{{ID: "gpt-codex"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if choices, _ := r.Choices(context.Background()); len(choices) != 0 {
		t.Fatalf("disabled codex leaked choices: %+v", choices)
	}
}

func TestCodexManagerExistsWhileDisabled(t *testing.T) {
	store := newFakeStore()
	store.providers["codex"] = Provider{
		ID: "codex", Kind: KindCodex, Enabled: false, Models: []Model{{ID: "gpt-codex"}},
	}
	r, err := NewRouter(context.Background(), RouterOptions{Scope: testScope(), Store: store, Static: []Provider{store.providers["codex"]}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Manager("codex") == nil {
		t.Fatal("disabled codex provider has no manager for sign-in")
	}
	if choices, _ := r.Choices(context.Background()); len(choices) != 0 {
		t.Fatalf("disabled provider leaked choices: %+v", choices)
	}
}

func TestProviderDefaults(t *testing.T) {
	p := Provider{ID: "x", Kind: KindCodex, Enabled: true, Models: []Model{{ID: "a"}, {ID: "b"}}}.Normalized()
	if p.Label != "x" || p.WireAPI != "responses" || p.DefaultModel != "a" || p.BaseURL == "" {
		t.Fatalf("normalized = %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := Provider{ID: "UPPER", Kind: KindOpenAI, Enabled: true, Models: []Model{{ID: "a"}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("invalid id accepted")
	}
	bad = Provider{ID: "x", Kind: KindOpenAI, Enabled: false, BaseURL: "ftp://example.com"}
	if err := bad.Validate(); err == nil {
		t.Fatal("invalid base URL accepted")
	}
	bad = Provider{ID: "x", Kind: KindOpenAI, BaseURL: "https://user:pass@example.com"}
	if err := bad.Validate(); err == nil {
		t.Fatal("credentialed URL accepted")
	}
	bad = Provider{ID: "x", Kind: KindCodex, Headers: map[string]string{"Authorization": "Bearer secret"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("codex Authorization header accepted")
	}
	bad = Provider{ID: "x", Kind: KindAnthropic, ExtraBody: map[string]any{"temperature": 0.2}}
	if err := bad.Validate(); err == nil {
		t.Fatal("Anthropic extra body accepted")
	}
}

func str(v string) *string { return &v }

func TestAdminCreateDoesNotReplaceExisting(t *testing.T) {
	store := newFakeStore()
	admin, err := NewAdmin(RouterOptions{Scope: testScope(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Router().Close()
	in := chat.ProviderInput{
		ID:     "gw",
		Kind:   str("openai"),
		Models: &[]chat.ModelOption{{ID: "m1"}},
		APIKey: str("sk-original"),
	}
	if _, err := admin.SaveProvider(context.Background(), "", in); err != nil {
		t.Fatal(err)
	}
	in.APIKey = str("sk-other")
	_, err = admin.SaveProvider(context.Background(), "", in)
	var ae *chat.AdminError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("second create err = %v, want 409", err)
	}
	if p, _ := store.GetProvider(context.Background(), testScope(), "gw"); p.APIKey != "sk-original" {
		t.Fatalf("create replaced the stored key: %q", p.APIKey)
	}
}

func TestAdminUpdateRequiresTheUnreadableSecret(t *testing.T) {
	store := newFakeStore()
	store.providers["gw"] = Provider{
		ID: "gw", Kind: KindOpenAI, Enabled: true,
		Models:           []Model{{ID: "m1"}},
		Headers:          map[string]string{"X-Team": "a"},
		SecretError:      "The API key could not be decrypted; check the encryption key or re-enter it.",
		APIKeyUnreadable: true,
	}
	admin, err := NewAdmin(RouterOptions{Scope: testScope(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Router().Close()
	// Re-entering only the headers must not save the unreadable key as empty.
	if _, err := admin.SaveProvider(context.Background(), "gw", chat.ProviderInput{Headers: map[string]string{"X-Team": "b"}}); err == nil {
		t.Fatal("saved without re-entering the unreadable API key")
	}
	if _, err := admin.SaveProvider(context.Background(), "gw", chat.ProviderInput{APIKey: str("sk-new")}); err != nil {
		t.Fatal(err)
	}
	p, _ := store.GetProvider(context.Background(), testScope(), "gw")
	if p.APIKey != "sk-new" || p.APIKeyUnreadable || p.SecretError != "" {
		t.Fatalf("provider = %+v", p)
	}
}

func TestRouterReplacesManagerWhenOAuthBaseChanges(t *testing.T) {
	store := newFakeStore()
	store.providers["cx"] = Provider{ID: "cx", Kind: KindCodex, Models: []Model{{ID: "gpt"}}}
	router := mustRouter(t, store)
	defer router.Close()
	first := router.Manager("cx")
	if first == nil {
		t.Fatal("no manager")
	}
	p := store.providers["cx"]
	p.OAuthBaseURL = "https://auth.example.test"
	store.providers["cx"] = p
	if err := router.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := router.Manager("cx")
	if second == first || second.OAuth.AuthBase != "https://auth.example.test" {
		t.Fatalf("manager was not rebuilt for the new OAuth base URL")
	}
}
