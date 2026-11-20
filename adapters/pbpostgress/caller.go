package pbpostgress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"

	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/pocketbase/pocketbase/core"
)

// Caller is the signed-in PocketBase user behind one custom tool call.
//
// It is for hosts whose permission checks live in their own route handlers
// rather than in collection rules: a custom tool resolves the caller, then
// runs the host's existing handler as that user instead of re-implementing
// its checks.
type Caller struct {
	App    core.App
	Record *core.Record
	// Token is the caller's PocketBase auth token, as the MCP or chat request
	// sent it. Never log it.
	Token string
}

// ResolveCaller returns the user behind call.
//
// It validates the call's token again rather than trusting the principal, so
// a token that was revoked (password change, deleted user) after the turn or
// MCP request began is refused, and the record it returns is read now, with
// the user's current roles and fields. A refusal is pbmcp.Denied, which the
// tool pipeline reports to the model as-is.
func ResolveCaller(app core.App, call pbmcp.CallContext) (*Caller, error) {
	p := call.Principal
	if app == nil || p.Token == "" || p.ID == "" {
		return nil, pbmcp.Denied("Sign in again to use this tool.")
	}
	rec, err := app.FindAuthRecordByToken(p.Token, core.TokenTypeAuth)
	if err != nil || rec == nil || rec.Id != p.ID || rec.Collection().Id != p.CollectionID {
		return nil, pbmcp.Denied("Sign in again to use this tool.")
	}
	return &Caller{App: app, Record: rec, Token: p.Token}, nil
}

// RequestEvent builds a request event signed in as the caller, for running
// one of the host's own route handlers in-process. The handler's response is
// recorded in the returned recorder, not sent anywhere.
//
// The request carries the caller's token in its Authorization header and
// ctx as its context, so the tool's timeout and cancellation reach the
// handler. target is a path with an optional query, such as
// "/api/things?page=2". A non-nil body is sent as JSON. Path values
// ({id} and the like) are not set by this function: set them with
// e.Request.SetPathValue before calling a handler that reads them.
func (c *Caller) RequestEvent(ctx context.Context, method, target string, body io.Reader) (*core.RequestEvent, *httptest.ResponseRecorder) {
	if body == nil {
		body = http.NoBody
	}
	req := httptest.NewRequestWithContext(ctx, method, target, body)
	req.Header.Set("Authorization", c.Token)
	if body != http.NoBody {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e := &core.RequestEvent{App: c.App, Auth: c.Record}
	e.Request = req
	e.Response = rec
	return e, rec
}

// ResolveTurnCaller returns the user behind a chat turn, for a
// chat.RunnerConfig.Setup hook that reads what the turn's context names as the
// caller (through the host's own permission-checked handlers). It validates
// the token again, exactly like ResolveCaller. A turn without a chat
// principal is refused with pbmcp.Denied.
func ResolveTurnCaller(ctx context.Context, app core.App, in chat.TurnInput) (*Caller, error) {
	p, ok := pbmcp.TurnPrincipal(in)
	if !ok {
		return nil, pbmcp.Denied("Sign in again to use this tool.")
	}
	return ResolveCaller(app, pbmcp.CallContext{Principal: p, Context: ctx})
}
