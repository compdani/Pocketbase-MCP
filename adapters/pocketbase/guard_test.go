package pocketbase

import "testing"

func TestIsRecordEndpoint(t *testing.T) {
	ok := []string{"/api/collections/items/records", "/api/collections/items/records/abc123", "/api/collections/items/records/mcpabc"}
	bad := []string{"/mcp", "/api/collections/items/records/..", "/api/collections/items/records/a/b", "/api/collections/items/auth-with-password", "/api/collections/items/records/a%2Fb", "/api/collections/items/records?x=1", "/api/settings"}
	for _, p := range ok {
		if !isRecordEndpoint(p) {
			t.Errorf("expected allowed: %s", p)
		}
	}
	for _, p := range bad {
		if isRecordEndpoint(p) {
			t.Errorf("expected rejected: %s", p)
		}
	}
}
