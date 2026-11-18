package pocketbase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"

	"github.com/compdani/pocketbase-mcp/chat/provider/catalog"
	"github.com/compdani/pocketbase-mcp/chat/provider/codex"
)

func newProviderTestApp(t *testing.T) core.App {
	t.Helper()
	t.Setenv("PB_TEST_ENCRYPTION_KEY", strings.Repeat("k", 32))
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:       t.TempDir(),
		DefaultEncryptionEnv: "PB_TEST_ENCRYPTION_KEY",
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { app.ResetBootstrapState() })
	return app
}

func providerFixture(id string) catalog.Provider {
	return catalog.Provider{
		ID:           id,
		Label:        "Label " + id,
		Kind:         catalog.KindOpenAI,
		Enabled:      true,
		BaseURL:      "https://example.invalid/v1",
		Models:       []catalog.Model{{ID: "m1", Label: "Model 1"}},
		DefaultModel: "m1",
		Headers:      map[string]string{"X-Test": "one"},
		APIKey:       "sk-secret-" + id,
	}
}

func TestProviderStoreRoundTripAndEncryption(t *testing.T) {
	app := newProviderTestApp(t)
	scope := catalog.Scope{Namespace: "ns", Mount: "/mcp"}
	store, err := NewProviderStore(app, ProviderStoreOptions{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProvider(context.Background(), scope, providerFixture("openai")); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetProvider(context.Background(), scope, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-secret-openai" || got.Models[0].ID != "m1" || got.Headers["X-Test"] != "one" {
		t.Fatalf("provider = %+v", got)
	}
	rec, err := app.FindFirstRecordByData(store.providers, "slug", "openai")
	if err != nil {
		t.Fatal(err)
	}
	cipher := rec.GetString("apiKeyCipher")
	if cipher == "" || strings.Contains(cipher, "sk-secret") {
		t.Fatalf("api key was not encrypted: %q", cipher)
	}
	raw, err := security.Decrypt(cipher, strings.Repeat("k", 32))
	if err != nil || string(raw) != "sk-secret-openai" {
		t.Fatalf("decrypt = %q err = %v", raw, err)
	}
	headersCipher := rec.GetString("headersCipher")
	if headersCipher == "" || strings.Contains(headersCipher, "X-Test") {
		t.Fatalf("headers were not encrypted: %q", headersCipher)
	}
	rawHeaders, err := security.Decrypt(headersCipher, strings.Repeat("k", 32))
	if err != nil || !strings.Contains(string(rawHeaders), "X-Test") {
		t.Fatalf("decrypted headers = %q err = %v", rawHeaders, err)
	}

	cred := &codex.Credentials{
		ClientID:     "oaiapp_test",
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := store.SaveCredentials(context.Background(), scope, "openai", cred); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCredentials(context.Background(), scope, "openai")
	if err != nil || loaded.RefreshToken != "refresh" {
		t.Fatalf("credentials = %+v err = %v", loaded, err)
	}

	if err := store.SaveSetting(context.Background(), scope, "defaultChoice", map[string]string{"provider": "openai", "model": "m1"}); err != nil {
		t.Fatal(err)
	}
	var def struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := store.LoadSetting(context.Background(), scope, "defaultChoice", &def); err != nil {
		t.Fatal(err)
	}
	if def.Provider != "openai" || def.Model != "m1" {
		t.Fatalf("default = %+v", def)
	}
	hostID, err := store.HostID(context.Background(), scope)
	if err != nil || !validHostID(hostID) {
		t.Fatalf("host id = %q err = %v", hostID, err)
	}
	if again, _ := store.HostID(context.Background(), scope); again != hostID {
		t.Fatalf("host id changed from %q to %q", hostID, again)
	}

	if err := store.DeleteProvider(context.Background(), scope, "openai"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetProvider(context.Background(), scope, "openai"); !errors.Is(err, catalog.ErrProviderNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
	if _, err := store.LoadCredentials(context.Background(), scope, "openai"); !errors.Is(err, codex.ErrNotSignedIn) {
		t.Fatalf("credentials after delete = %v", err)
	}
}

func TestProviderStoreKeepsUndecryptableProviderVisible(t *testing.T) {
	app := newProviderTestApp(t)
	scope := catalog.Scope{Namespace: "ns", Mount: "/mcp"}
	store, err := NewProviderStore(app, ProviderStoreOptions{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProvider(context.Background(), scope, providerFixture("openai")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PB_TEST_ENCRYPTION_KEY", strings.Repeat("x", 32))
	got, err := store.GetProvider(context.Background(), scope, "openai")
	if err != nil {
		t.Fatalf("a rotated key should not hide the provider: %v", err)
	}
	if got.SecretError == "" {
		t.Fatal("undecryptable secret was not reported")
	}
	if got.APIKey != "" || len(got.Headers) != 0 {
		t.Fatalf("undecryptable secrets leaked: %+v", got)
	}
}

func TestProviderStoreScopeIsolation(t *testing.T) {
	app := newProviderTestApp(t)
	a := catalog.Scope{Namespace: "a", Mount: "/mcp"}
	b := catalog.Scope{Namespace: "b", Mount: "/mcp"}
	storeA, err := NewProviderStore(app, ProviderStoreOptions{Scope: a})
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := NewProviderStore(app, ProviderStoreOptions{Scope: b})
	if err != nil {
		t.Fatal(err)
	}
	if err := storeA.SaveProvider(context.Background(), a, providerFixture("openai")); err != nil {
		t.Fatal(err)
	}
	list, err := storeB.ListProviders(context.Background(), b)
	if err != nil || len(list) != 0 {
		t.Fatalf("scope B saw %+v err = %v", list, err)
	}
	if err := storeB.SaveProvider(context.Background(), a, providerFixture("nope")); err == nil {
		t.Fatal("cross-scope write accepted")
	}
}

func TestProviderStoreRequiresEncryptionKey(t *testing.T) {
	t.Setenv("PB_TEST_ENCRYPTION_KEY", "")
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:       t.TempDir(),
		DefaultEncryptionEnv: "PB_TEST_ENCRYPTION_KEY",
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.ResetBootstrapState() })
	scope := catalog.Scope{Namespace: "ns", Mount: "/mcp"}
	store, err := NewProviderStore(app, ProviderStoreOptions{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProvider(context.Background(), scope, providerFixture("openai")); !errors.Is(err, catalog.ErrNoEncryptionKey) {
		t.Fatalf("save without key = %v", err)
	}
}
