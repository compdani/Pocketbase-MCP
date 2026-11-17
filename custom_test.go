package pocketbasemcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func okHandler(context.Context, CallContext, map[string]any) (any, error) {
	return map[string]any{"ok": true}, nil
}

func TestValidateCustomTools(t *testing.T) {
	manifest := &Manifest{SchemaVersion: "1", ToolMode: "per_collection", Collections: []ManifestCollection{
		{Collection: "deals", Operations: []string{"describe", "search", "fetch"}},
	}}
	base := func(tools ...ToolSpec) Config {
		return Config{Manifest: manifest, CustomTools: tools}
	}
	valid := ToolSpec{Name: "mine", InputSchema: map[string]any{"type": "object"}, Handler: okHandler}
	if err := base(valid).ValidateCustomTools(); err != nil {
		t.Fatalf("valid tool rejected: %v", err)
	}
	cases := []struct {
		name string
		tool ToolSpec
		want string
	}{
		{"empty name", ToolSpec{Handler: okHandler}, "name is required"},
		{"missing handler", ToolSpec{Name: "x"}, "missing handler"},
		{"unknown policy", ToolSpec{Name: "x", Policy: "nope", Handler: okHandler}, "unknown policy"},
		{"bad effect", ToolSpec{Name: "x", Effects: Effects{Kind: "admin"}, Handler: okHandler}, "invalid effect"},
		{"bad channels", ToolSpec{Name: "x", Channels: "sms", Handler: okHandler}, "invalid channels"},
		{"editor on mcp", ToolSpec{Name: "x", ExecutionLocation: "editor", Handler: okHandler}, "editor"},
		{"editor on both", ToolSpec{Name: "x", Channels: "both", ExecutionLocation: "editor", Handler: okHandler}, "editor"},
		{"chat only", ToolSpec{Name: "x", Channels: "chat", Handler: okHandler}, "chat channel isn't available yet"},
		{"chat-only editor", ToolSpec{Name: "x", Channels: "chat", ExecutionLocation: "editor", Handler: okHandler}, "chat channel isn't available yet"},
		{"bad location", ToolSpec{Name: "x", ExecutionLocation: "browser", Handler: okHandler}, "invalid execution location"},
		{"timeout too big", ToolSpec{Name: "x", Timeout: time.Hour, Handler: okHandler}, "exceeds server maximum"},
		{"unsupported schema", ToolSpec{Name: "x", InputSchema: map[string]any{"type": "object", "oneOf": []any{}}, Handler: okHandler}, "unsupported keyword"},
		{"collision with generated", ToolSpec{Name: "fetch_deals", Handler: okHandler}, "collides"},
		{"collision with built-in", ToolSpec{Name: "describe_access", Handler: okHandler}, "collides"},
	}
	for _, c := range cases {
		if err := base(c.tool).ValidateCustomTools(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want error containing %q", c.name, err, c.want)
		}
	}
	// "both" is accepted: it works over MCP now and chat will share it later.
	if err := base(ToolSpec{Name: "shared", Channels: "both", Handler: okHandler}).ValidateCustomTools(); err != nil {
		t.Fatalf("both-channel tool rejected: %v", err)
	}
	// Duplicate custom names are a startup error.
	if err := base(valid, valid).ValidateCustomTools(); err == nil {
		t.Fatal("duplicate custom tool names accepted")
	}
	// Nil input schema means "no arguments" and is valid.
	if err := base(ToolSpec{Name: "noargs", Handler: okHandler}).ValidateCustomTools(); err != nil {
		t.Fatalf("nil input schema rejected: %v", err)
	}
}

func TestCustomToolGateAndAudit(t *testing.T) {
	events := []AuditEvent{}
	cfg := &CollectionConfig{Name: "deals", Alias: "deals", FieldMap: map[string]FieldInfo{}, SortFields: []string{"id"}}
	srv := New(Config{
		Auth:                AuthConfig{Collections: []string{"users"}},
		AllowWrites:         false,
		AuditIncludePayload: true,
		AuditFunc:           func(e AuditEvent) { events = append(events, e) },
		CustomTools: []ToolSpec{
			{Name: "hidden_tool", Policy: "field:role:admin", InputSchema: map[string]any{"type": "object"}, Handler: okHandler},
			{Name: "echo", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}, "required": []string{"n"}, "additionalProperties": false}, Handler: okHandler},
			{Name: "sink", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"api_token": map[string]any{"type": "string"}}, "additionalProperties": false}, Handler: okHandler},
		},
	}, &fakeHost{cfg: cfg})

	_, out := rpc(t, srv, "good", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listed, _ := json.Marshal(out)
	for _, absent := range []string{"hidden_tool"} {
		if strings.Contains(string(listed), absent) {
			t.Fatalf("%s must not be listed: %s", absent, listed)
		}
	}
	if !strings.Contains(string(listed), "echo") {
		t.Fatalf("echo must be listed: %s", listed)
	}
	// Direct call to a policy-hidden tool is rejected through the same gate.
	_, out = rpc(t, srv, "good", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hidden_tool","arguments":{}}}`)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("hidden tool call must be a tool error: %v", out)
	}
	// Malformed arguments are a JSON-RPC invalid-params error with a path.
	_, out = rpc(t, srv, "good", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"n":"x"}}}`)
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32602 || !strings.Contains(errObj["data"].(string), "/n") {
		t.Fatalf("invalid args must be -32602 with pointer path: %v", out)
	}
	// A valid call succeeds; the audit payload is redacted, never raw.
	_, out = rpc(t, srv, "good", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"sink","arguments":{"api_token":"s3cr3t-value"}}}`)
	res, _ = out["result"].(map[string]any)
	if res == nil || res["isError"] == true {
		t.Fatalf("valid sink call failed: %v", out)
	}
	_, out = rpc(t, srv, "good", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"n":2}}}`)
	res, _ = out["result"].(map[string]any)
	if res == nil || res["isError"] == true {
		t.Fatalf("valid echo call failed: %v", out)
	}
	blob, _ := json.Marshal(events)
	if strings.Contains(string(blob), "s3cr3t-value") {
		t.Fatalf("secret leaked into audit events: %s", blob)
	}
	outcomes := map[string]bool{}
	for _, e := range events {
		outcomes[e.Outcome] = true
	}
	for _, want := range []string{"denied", "invalid", "ok"} {
		if !outcomes[want] {
			t.Fatalf("missing audit outcome %q in %v", want, outcomes)
		}
	}
}

func TestCustomToolTimeoutOutcome(t *testing.T) {
	events := []AuditEvent{}
	cfg := &CollectionConfig{Name: "deals", Alias: "deals", FieldMap: map[string]FieldInfo{}, SortFields: []string{"id"}}
	srv := New(Config{
		Auth:      AuthConfig{Collections: []string{"users"}},
		AuditFunc: func(e AuditEvent) { events = append(events, e) },
		CustomTools: []ToolSpec{
			{Name: "slow", Timeout: time.Millisecond, InputSchema: map[string]any{"type": "object"}, Handler: func(ctx context.Context, _ CallContext, _ map[string]any) (any, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}},
		},
	}, &fakeHost{cfg: cfg})
	_, out := rpc(t, srv, "good", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","arguments":{}}}`)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("timeout must be a tool error: %v", out)
	}
	found := false
	for _, e := range events {
		if e.Tool == "slow" && e.Outcome == "timeout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing timeout audit event: %v", events)
	}
}
