package pocketbase

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/pocketbase/core"

	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/chat"
)

// TestResolveCallerRunsHostHandler is the pattern a host with its own
// permission checks uses: a custom tool resolves the caller and runs one of
// the host's route handlers in-process as that user.
func TestResolveCallerRunsHostHandler(t *testing.T) {
	env := newTestEnv(t)
	cfg := testConfig("per_collection")
	// A stand-in for a host route handler that reads the signed-in user.
	hostHandler := func(e *core.RequestEvent) error {
		if e.Auth == nil {
			return e.UnauthorizedError("", nil)
		}
		if e.Request.Header.Get("Authorization") == "" {
			return errors.New("token not forwarded")
		}
		return e.JSON(http.StatusOK, map[string]any{"user": e.Auth.GetString("email"), "page": e.Request.URL.Query().Get("page")})
	}
	cfg.CustomTools = []pbmcp.ToolSpec{{
		Name: "whoami", Policy: "authenticated", InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, call pbmcp.CallContext, _ map[string]any) (any, error) {
			caller, err := ResolveCaller(env.app, call)
			if err != nil {
				return nil, err
			}
			e, rec := caller.RequestEvent(ctx, http.MethodGet, "/api/whoami?page=2", nil)
			if err := hostHandler(e); err != nil {
				return nil, err
			}
			return map[string]any{"status": rec.Code, "body": rec.Body.String()}, nil
		},
	}}
	srv, err := New(env.app, cfg)
	if err != nil {
		t.Fatal(err)
	}
	serveWith(t, env, srv)

	sc, bad, msg := env.call(env.userToken, "whoami", nil)
	if bad {
		t.Fatalf("whoami failed: %s", msg)
	}
	body, _ := sc["body"].(string)
	if sc["status"] != float64(http.StatusOK) || !strings.Contains(body, `"user":"user@example.com"`) || !strings.Contains(body, `"page":"2"`) {
		t.Fatalf("unexpected result: %v", sc)
	}
}

func TestResolveCallerRefusesStaleOrForeignTokens(t *testing.T) {
	env := newTestEnv(t)
	rec, err := env.app.FindAuthRecordByToken(env.userToken, core.TokenTypeAuth)
	if err != nil {
		t.Fatal(err)
	}
	principal := pbmcp.Principal{ID: rec.Id, CollectionID: rec.Collection().Id, CollectionName: rec.Collection().Name, Token: env.userToken}

	if c, err := ResolveCaller(env.app, pbmcp.CallContext{Principal: principal}); err != nil || c.Record.Id != rec.Id {
		t.Fatalf("valid caller refused: %v", err)
	}
	// The same check from a chat Setup hook.
	if c, err := ResolveTurnCaller(context.Background(), env.app, chat.TurnInput{Identity: principal}); err != nil || c.Record.Id != rec.Id {
		t.Fatalf("turn caller refused: %v", err)
	}
	if _, err := ResolveTurnCaller(context.Background(), env.app, chat.TurnInput{}); err == nil {
		t.Fatal("a turn without an identity resolved")
	}
	cases := map[string]pbmcp.Principal{
		"no token":      {ID: rec.Id, CollectionID: rec.Collection().Id},
		"other user":    {ID: rec.Id, CollectionID: rec.Collection().Id, Token: env.otherToken},
		"garbage token": {ID: rec.Id, CollectionID: rec.Collection().Id, Token: "nope"},
	}
	for name, p := range cases {
		if _, err := ResolveCaller(env.app, pbmcp.CallContext{Principal: p}); err == nil {
			t.Fatalf("%s: expected refusal", name)
		} else if msg, ok := pbmcp.UserMessage(err); !ok || msg == "" {
			t.Fatalf("%s: refusal is not user-safe: %v", name, err)
		}
	}

	// Changing the password rotates the token key, so the old token stops
	// resolving even though the principal still names the user.
	rec.SetPassword("another-password-123")
	if err := env.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveCaller(env.app, pbmcp.CallContext{Principal: principal}); err == nil {
		t.Fatal("token from before a password change still resolves")
	}
}
