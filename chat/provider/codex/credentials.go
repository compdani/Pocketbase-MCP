// Package codex implements the ChatGPT plan ("Sign in with ChatGPT") route used
// by Codex. It provides OAuth with PKCE, rotating refresh tokens, ID-token
// verification over the standard library, a model list and a token manager.
//
// It is authentication only; the wire protocol is the OpenAI Responses adapter
// in chat/provider/openai. A catalog builds that adapter with a Manager as its
// token source.
package codex

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

var (
	// ErrNotSignedIn reports that no account is stored for this provider.
	ErrNotSignedIn = errors.New("codex: no ChatGPT account is signed in")
	// ErrPlanNotGranted reports a sign-in that did not grant plan usage. The
	// account may be signed in, but model requests would be rejected.
	ErrPlanNotGranted = errors.New("codex: ChatGPT plan use was not granted; sign in again and allow plan usage")
)

// Credentials are one signed-in ChatGPT account for one provider entry.
type Credentials struct {
	ClientID     string    `json:"client_id"`
	Sub          string    `json:"sub"`
	Email        string    `json:"email,omitempty"`
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Scopes       []string  `json:"scopes"`
	ExpiresAt    time.Time `json:"expires_at"`
	HostID       string    `json:"host_id"`
}

// HasPlan reports whether the stored scopes grant plan usage.
func (c *Credentials) HasPlan() bool {
	return c != nil && slices.Contains(c.Scopes, PlanScope)
}

// ValidForImport checks the minimum fields an imported credential needs.
func (c *Credentials) ValidForImport() error {
	if c == nil || strings.TrimSpace(c.ClientID) == "" || strings.TrimSpace(c.ClientID) == DynamicClient {
		return errors.New("credentials need an issued client_id")
	}
	if strings.TrimSpace(c.RefreshToken) == "" {
		return errors.New("credentials need a refresh_token")
	}
	if !c.HasPlan() {
		return ErrPlanNotGranted
	}
	return nil
}

// SignInStart is the browser-facing start of one authorization attempt.
type SignInStart struct {
	AuthURL   string    `json:"authUrl"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Status summarizes one provider's sign-in for the admin UI. It never contains
// a token.
type Status struct {
	SignedIn  bool      `json:"signedIn"`
	Email     string    `json:"email,omitempty"`
	Plan      bool      `json:"plan"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	ClientID  string    `json:"clientId,omitempty"`
	HostID    string    `json:"hostId,omitempty"`
	Pending   bool      `json:"pending,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Store persists one provider's credentials. The adapter implements it over
// the encrypted private provider-auth collection.
type Store interface {
	Load(ctx context.Context) (*Credentials, error)
	Save(ctx context.Context, c *Credentials) error
	Clear(ctx context.Context) error
}
