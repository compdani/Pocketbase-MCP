package pocketbase

import (
	"context"
	"net/http"
	"testing"

	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider/catalog"
)

func TestChatManagedProvidersEndToEnd(t *testing.T) {
	env := newChatEnv(t, chatEnvOptions{
		enable: func(srv *Server) error {
			return srv.EnableChat(ChatConfig{
				Options:         pbmcp.ChatOptions{Namespace: "chat-tests", Prefix: chatTestPrefix},
				ManageProviders: true,
				Providers: []catalog.Provider{{
					ID:           "openai",
					Label:        "Seed provider",
					Kind:         catalog.KindOpenAI,
					Enabled:      true,
					BaseURL:      "https://example.invalid/v1",
					APIKey:       "sk-seed",
					Models:       []catalog.Model{{ID: "m1", Label: "Model One"}},
					DefaultModel: "m1",
				}},
				System: "You are a test model.",
			})
		},
	})

	var status struct {
		Choices       []chat.ModelChoice `json:"choices"`
		DefaultChoice string             `json:"defaultChoice"`
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/status", "", &status); code != http.StatusOK {
		t.Fatalf("status: HTTP %d", code)
	}
	if len(status.Choices) != 1 || status.Choices[0].Provider != "openai" || status.Choices[0].Model != "m1" {
		t.Fatalf("choices = %+v", status.Choices)
	}
	if status.DefaultChoice != chat.ChoiceID("openai", "m1") {
		t.Fatalf("default = %q", status.DefaultChoice)
	}

	var list struct {
		Items         []chat.ProviderView `json:"items"`
		DefaultChoice string              `json:"defaultChoice"`
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/providers", "", &list); code != http.StatusForbidden {
		t.Fatalf("user provider list: HTTP %d", code)
	}
	if code := env.json(env.superToken, http.MethodGet, chatTestPrefix+"/providers", "", &list); code != http.StatusOK {
		t.Fatalf("root provider list: HTTP %d", code)
	}
	if len(list.Items) != 1 || !list.Items[0].HasAPIKey || list.Items[0].APIKeyHint != "…seed" {
		t.Fatalf("provider view = %+v", list.Items)
	}

	var updated struct {
		Provider chat.ProviderView `json:"provider"`
	}
	if code := env.json(env.superToken, http.MethodPatch, chatTestPrefix+"/providers/openai", `{"label":"Renamed"}`, &updated); code != http.StatusOK {
		t.Fatalf("update provider: HTTP %d", code)
	}
	if updated.Provider.Label != "Renamed" || !updated.Provider.HasAPIKey {
		t.Fatalf("updated provider = %+v", updated.Provider)
	}

	if code := env.json(env.superToken, http.MethodPost, chatTestPrefix+"/providers/actions/default", `{"provider":"openai","model":"m1"}`, nil); code != http.StatusOK {
		t.Fatalf("set default: HTTP %d", code)
	}

	if code := env.json(env.superToken, http.MethodDelete, chatTestPrefix+"/providers/openai", "", nil); code != http.StatusOK {
		t.Fatalf("delete provider: HTTP %d", code)
	}
	status.Choices = nil
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/status", "", &status); code != http.StatusOK {
		t.Fatalf("status after delete: HTTP %d", code)
	}
	if len(status.Choices) != 0 {
		t.Fatalf("choices after delete = %+v", status.Choices)
	}
}

// A host that keeps its own provider settings passes its store without
// ManageProviders: the catalog loads from it, no admin routes exist, and the
// host reloads the catalog after its settings change.
func TestChatHostOwnedProviderStore(t *testing.T) {
	var store catalog.Store
	var srvRef *Server
	env := newChatEnv(t, chatEnvOptions{
		enable: func(srv *Server) error {
			srvRef = srv
			built, err := NewProviderStore(srv.Adapter.app, ProviderStoreOptions{Prefix: "_hostProviders", Scope: catalog.Scope{Namespace: "chat-tests", Mount: "/mcp"}})
			if err != nil {
				return err
			}
			if err := built.SaveProvider(context.Background(), catalog.Scope{Namespace: "chat-tests", Mount: "/mcp"}, catalog.Provider{
				ID: "api", Label: "API", Kind: catalog.KindOpenAI, Enabled: true, BaseURL: "https://example.invalid/v1",
				Models: []catalog.Model{{ID: "m1"}}, DefaultModel: "m1",
			}); err != nil {
				return err
			}
			store = built
			return srv.EnableChat(ChatConfig{
				Options:       pbmcp.ChatOptions{Namespace: "chat-tests", Prefix: chatTestPrefix},
				ProviderStore: built,
			})
		},
	})
	var status struct {
		Choices []chat.ModelChoice `json:"choices"`
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/status", "", &status); code != http.StatusOK || len(status.Choices) != 1 {
		t.Fatalf("status: HTTP %d %+v", code, status.Choices)
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/providers", "", nil); code != http.StatusNotFound {
		t.Fatalf("admin routes: HTTP %d, want 404", code)
	}
	// The host's settings page adds a model, then reloads the catalog.
	scope := catalog.Scope{Namespace: "chat-tests", Mount: "/mcp"}
	p, err := store.GetProvider(context.Background(), scope, "api")
	if err != nil {
		t.Fatal(err)
	}
	p.Models = append(p.Models, catalog.Model{ID: "m2"})
	if err := store.SaveProvider(context.Background(), scope, p); err != nil {
		t.Fatal(err)
	}
	router, ok := srvRef.ChatRouter()
	if !ok {
		t.Fatal("no router")
	}
	if err := router.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	status.Choices = nil
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/status", "", &status); code != http.StatusOK || len(status.Choices) != 2 {
		t.Fatalf("after reload: HTTP %d %+v", code, status.Choices)
	}
}
