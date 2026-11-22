package catalog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider/codex"
)

func TestCodexRefreshWithMaxEffortCanBeEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("unexpected model request: %s, authenticated=%v", r.URL.Path, r.Header.Get("Authorization") != "")
		}
		_, _ = fmt.Fprint(w, `{"models":[{"slug":"gpt-5.6-luna","supported_reasoning_levels":[{"effort":"max"},{"effort":"low"},{"effort":"high"}]}]}`)
	}))
	defer srv.Close()
	ctx := context.Background()
	store := newFakeStore()
	store.providers["chatgpt"] = Provider{ID: "chatgpt", Kind: KindCodex, BaseURL: srv.URL}
	store.creds["chatgpt"] = &codex.Credentials{AccessToken: "access", Scopes: []string{codex.PlanScope}, ExpiresAt: time.Now().Add(time.Hour)}
	admin, err := NewAdmin(RouterOptions{Scope: testScope(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Router().Close()
	if _, err := admin.RefreshProviderModels(ctx, "chatgpt", true); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GetProvider(ctx, testScope(), "chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	// Durable stores validate on save; the fake store does not.
	if err := saved.Validate(); err != nil {
		t.Fatalf("refreshed provider cannot be persisted: %v", err)
	}
	enabled := true
	if _, err := admin.SaveProvider(ctx, "chatgpt", chat.ProviderInput{Enabled: &enabled}); err != nil {
		t.Fatalf("enable refreshed provider: %v", err)
	}
	choices, err := admin.Router().Choices(ctx)
	if err != nil || len(choices) != 1 || !slices.Equal(choices[0].Efforts, []string{"low", "high", "max"}) {
		t.Fatalf("choices = %+v, err = %v", choices, err)
	}
}

func TestDefaultEffortsGuessesOpenAIReasoningModels(t *testing.T) {
	for id, want := range map[string]bool{
		"gpt-5": true, "gpt-5.4-mini": true, "o3": true, "o4-mini": true, "openai/gpt-5-codex": true,
		"gpt-5-chat-latest": false, "gpt-4.1": false, "gpt-4o-realtime-preview": false, "llama3.1": false,
	} {
		if got := len(DefaultEfforts(id)) > 0; got != want {
			t.Errorf("DefaultEfforts(%q) = %v, want efforts %v", id, DefaultEfforts(id), want)
		}
	}
}

func TestEffortsAreNormalizedAndValidated(t *testing.T) {
	p := Provider{ID: "o", Kind: KindOpenAI, Enabled: true, Models: []Model{{ID: "m", Efforts: []string{" HIGH", "low", "high", ""}}}}.Normalized()
	if got := p.Models[0].Efforts; !slices.Equal(got, []string{"low", "high"}) {
		t.Fatalf("normalized efforts = %v", got)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}

	bad := Provider{ID: "o", Kind: KindOpenAI, Enabled: true, Models: []Model{{ID: "m", Efforts: []string{"turbo"}}}}.Normalized()
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "turbo") {
		t.Fatalf("unknown effort accepted: %v", err)
	}
	claude := Provider{ID: "c", Kind: KindAnthropic, Enabled: true, Models: []Model{{ID: "m", Efforts: []string{"high"}}}}.Normalized()
	if err := claude.Validate(); err == nil {
		t.Fatal("Anthropic model with efforts accepted")
	}
}

func TestDiscoveryAndChoicesCarryEfforts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[
			{"id":"gpt-5"},
			{"id":"gpt-4.1"},
			{"id":"gateway-model","supported_reasoning_levels":[{"effort":"minimal"},{"effort":"high"}]}
		]}`)
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
		ID: "openai", Kind: str("openai"), Enabled: &enabled, BaseURL: str(srv.URL), APIKey: str("sk"),
	})
	if err != nil {
		t.Fatal(err)
	}
	efforts := map[string][]string{}
	for _, m := range view.Models {
		efforts[m.ID] = m.Efforts
	}
	if !slices.Equal(efforts["gpt-5"], []string{"low", "medium", "high"}) || efforts["gpt-4.1"] != nil ||
		!slices.Equal(efforts["gateway-model"], []string{"minimal", "high"}) {
		t.Fatalf("discovered efforts = %v", efforts)
	}

	choices, err := admin.Router().Choices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range choices {
		if !slices.Equal(c.Efforts, efforts[c.Model]) {
			t.Errorf("choice %s efforts = %v, want %v", c.ID, c.Efforts, efforts[c.Model])
		}
	}
	resolved, err := admin.Router().Resolve(context.Background(), "openai", "gpt-5")
	if err != nil || !slices.Equal(resolved.Choice.Efforts, []string{"low", "medium", "high"}) {
		t.Fatalf("resolved choice = %+v err = %v", resolved.Choice, err)
	}
}

// An administrator's efforts survive a refresh that cannot discover any.
func TestRefreshKeepsConfiguredEfforts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"local-reasoner"},{"id":"new-model"}]}`)
	}))
	defer srv.Close()
	store := newFakeStore()
	p := Provider{ID: "local", Kind: KindOpenAI, Enabled: true, BaseURL: srv.URL,
		Models: []Model{{ID: "local-reasoner", Efforts: []string{"low", "high"}}}, DefaultModel: "local-reasoner"}
	store.providers[p.ID] = p
	admin, err := NewAdminWithRouter(mustRouter(t, store, p), RouterOptions{Scope: testScope(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Router().Close()
	if _, err := admin.RefreshProviderModels(context.Background(), p.ID, true); err != nil {
		t.Fatal(err)
	}
	saved, _ := store.GetProvider(context.Background(), testScope(), p.ID)
	m, _ := saved.Model("local-reasoner")
	if !slices.Equal(m.Efforts, []string{"low", "high"}) {
		t.Fatalf("efforts after refresh = %v", m.Efforts)
	}
}
