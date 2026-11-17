package pocketbasemcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeHost struct{ cfg *CollectionConfig }

func (f *fakeHost) Authenticate(token string) (Principal, error) {
	if token == "good" {
		return Principal{ID: "u1", CollectionName: "users"}, nil
	}
	if token == "other" {
		return Principal{ID: "u2", CollectionName: "customers"}, nil
	}
	if token == "super" {
		return Principal{ID: "s1", CollectionName: "_superusers", IsSuperuser: true}, nil
	}
	return Principal{}, ToolError("bad token")
}
func (f *fakeHost) LookupCollection(n string) (*CollectionConfig, bool) {
	if n == f.cfg.Name || n == f.cfg.Alias {
		return f.cfg, true
	}
	return nil, false
}
func (f *fakeHost) VisibleCollections(Principal) []*CollectionConfig {
	return []*CollectionConfig{f.cfg}
}
func (f *fakeHost) AllCollections() []*CollectionConfig { return []*CollectionConfig{f.cfg} }
func (f *fakeHost) Search(_ context.Context, _ Principal, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	return map[string]any{"collection": cfg.Name, "results": []any{}, "page": 1, "perPage": 20, "totalItems": 0, "totalPages": 0, "hasMore": false}, nil
}
func (f *fakeHost) Fetch(_ context.Context, _ Principal, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	return map[string]any{"id": "x", "collection": cfg.Name, "title": "t", "record": map[string]any{}}, nil
}
func (f *fakeHost) Write(_ context.Context, _ Principal, _ string, cfg *CollectionConfig, _ map[string]any, _ string) (any, error) {
	return map[string]any{"id": "x", "collection": cfg.Name}, nil
}

func newTestServer() *Server {
	cfg := &CollectionConfig{Name: "deals", Alias: "deals", FieldMap: map[string]FieldInfo{}, SortFields: []string{"id"}}
	return New(Config{Auth: AuthConfig{Collections: []string{"users"}}}, &fakeHost{cfg: cfg})
}

func rpc(t *testing.T, srv *Server, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestProtocolBasics(t *testing.T) {
	srv := newTestServer()
	code, out := rpc(t, srv, "good", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if code != 200 || out["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize: %d %v", code, out)
	}
	code, _ = rpc(t, srv, "good", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	code, _ = rpc(t, srv, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauth: %d", code)
	}
	code, _ = rpc(t, srv, "other", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("other collection should be rejected: %d", code)
	}
	code, _ = rpc(t, srv, "super", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("superuser should be rejected by default: %d", code)
	}
	code, _ = rpc(t, srv, "good", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if code != http.StatusBadRequest {
		t.Fatalf("batch: %d", code)
	}
}
