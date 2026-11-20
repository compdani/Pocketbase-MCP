package catalog

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// hostTokenStore is a host-owned catalog: List Pocket's settings, with its own
// ChatGPT sign-in.
type hostTokenStore struct {
	*fakeStore
	mu       sync.Mutex
	signedIn bool
	token    string
	asked    []bool
}

func (s *hostTokenStore) CodexToken(_ context.Context, _ Scope, _ string, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, force)
	if force {
		s.token = "fresh"
	}
	return s.token, nil
}

// HostID fails: a host-owned store needn't keep one.
func (s *hostTokenStore) HostID(context.Context, Scope) (string, error) {
	return "", errors.New("no host id here")
}

func (s *hostTokenStore) CodexSignedIn(context.Context, Scope, string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signedIn
}

func TestCodexTokensFromTheHost(t *testing.T) {
	var auths []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer srv.Close()

	store := &hostTokenStore{fakeStore: newFakeStore(), token: "stale"}
	store.providers["chatgpt"] = Provider{ID: "chatgpt", Label: "ChatGPT", Kind: KindCodex, Enabled: true, BaseURL: srv.URL, Models: []Model{{ID: "gpt-5"}}}
	ctx := context.Background()
	r, err := NewRouter(ctx, RouterOptions{Scope: testScope(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if choices, _ := r.Choices(ctx); len(choices) != 0 {
		t.Fatalf("a signed-out host offered %+v", choices)
	}
	if r.Manager("chatgpt") != nil {
		t.Fatal("the router started its own sign-in for a host-owned provider")
	}

	store.mu.Lock()
	store.signedIn = true
	store.mu.Unlock()
	if err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.Resolve(ctx, "chatgpt", "gpt-5")
	if err != nil {
		t.Fatalf("Resolve after sign-in: %v", err)
	}
	ch, err := resolved.Provider.Stream(ctx, provider.Request{Model: "gpt-5", Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var last provider.Chunk
	for c := range ch {
		last = c
	}
	if last.Kind != provider.ChunkDone {
		t.Fatalf("stream ended with %+v", last)
	}
	// One 401 with the host's stale token, then the host refreshed it.
	if len(auths) != 2 || auths[0] != "Bearer stale" || auths[1] != "Bearer fresh" {
		t.Fatalf("authorizations = %v", auths)
	}
	if len(store.asked) != 2 || store.asked[0] || !store.asked[1] {
		t.Fatalf("token requests = %v, want a normal one then a forced refresh", store.asked)
	}
}
