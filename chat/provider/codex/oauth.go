package codex

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAuthBase is the ChatGPT authorization server.
	DefaultAuthBase = "https://auth.openai.com"
	// Resource is the API audience the plan token is minted for.
	Resource = "https://api.openai.com/v1"
	// Scopes includes the plan-use grant; a token without it cannot serve
	// model requests.
	Scopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	// PlanScope is the grant that marks a plan credential.
	PlanScope = "chatgpt.tokens.use.direct"
	// DynamicClient is the first-time dynamic registration client id.
	DynamicClient = "dynamic_agent_client"
	// AgentName is shown on the authorization screen.
	AgentName = "PocketBase MCP"
	// CallbackPath is the loopback callback path.
	CallbackPath = "/mcp-chat/codex/callback"
	// DefaultPort is tried first for the loopback listener.
	DefaultPort = 1455
)

// OAuth talks to the ChatGPT authorization server. It is safe for concurrent
// use; discovery and JWKS responses are cached.
type OAuth struct {
	// AuthBase defaults to DefaultAuthBase.
	AuthBase string
	// HTTP defaults to a 30-second client.
	HTTP *http.Client
	// Now defaults to time.Now.
	Now func() time.Time

	mu   sync.Mutex
	disc *discovery
	keys map[string]*rsa.PublicKey
}

type discovery struct {
	Issuer             string `json:"issuer"`
	JWKSURI            string `json:"jwks_uri"`
	RevocationEndpoint string `json:"revocation_endpoint"`
}

func (o *OAuth) base() string {
	if o.AuthBase != "" {
		return strings.TrimRight(o.AuthBase, "/")
	}
	return DefaultAuthBase
}

func (o *OAuth) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (o *OAuth) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *OAuth) issuer() string       { return o.base() }
func (o *OAuth) authorizeURL() string { return o.base() + "/api/accounts/authorize" }
func (o *OAuth) tokenURL() string     { return o.base() + "/api/accounts/oauth/token" }

// Pending is one authorization attempt kept until its callback arrives.
type Pending struct {
	State       string `json:"state"`
	Nonce       string `json:"nonce"`
	Verifier    string `json:"verifier"`
	RedirectURI string `json:"redirect_uri"`
	ClientID    string `json:"client_id"`
	HostID      string `json:"host_id"`
	AuthURL     string `json:"auth_url"`
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("codex: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Begin builds the authorization URL. existing, when present, reuses the
// previously issued client id and id_token_hint for the same account.
func (o *OAuth) Begin(hostID, redirectURI string, existing *Credentials) *Pending {
	p := &Pending{
		State:       randomString(24),
		Nonce:       randomString(24),
		Verifier:    randomString(48),
		RedirectURI: redirectURI,
		HostID:      hostID,
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	query := url.Values{
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {Scopes},
		"resource":              {Resource},
		"state":                 {p.State},
		"nonce":                 {p.Nonce},
		"code_challenge_method": {"S256"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"ext_agent_host_id":     {hostID},
	}
	if existing != nil && existing.ClientID != "" {
		p.ClientID = existing.ClientID
		query.Set("client_id", existing.ClientID)
		if existing.IDToken != "" {
			query.Set("id_token_hint", existing.IDToken)
		}
	} else {
		query.Set("client_id", DynamicClient)
		query.Set("agent_name_hint", AgentName)
	}
	p.AuthURL = o.authorizeURL() + "?" + query.Encode()
	return p
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// Complete validates the callback, exchanges the code and verifies the ID
// token. It returns ErrPlanNotGranted (with the credentials) when the plan
// scope was not granted.
func (o *OAuth) Complete(ctx context.Context, p *Pending, cb url.Values) (*Credentials, error) {
	if p == nil {
		return nil, errors.New("no sign-in is in progress")
	}
	if cb.Get("state") != p.State {
		return nil, errors.New("state mismatch; start sign-in again")
	}
	if e := cb.Get("error"); e != "" {
		if e == "access_denied" {
			return nil, errors.New("sign-in was declined")
		}
		return nil, fmt.Errorf("authorization failed: %s %s", e, cb.Get("error_description"))
	}
	clientID := p.ClientID
	if issued := cb.Get("client_id"); issued != "" {
		if clientID != "" && issued != clientID {
			return nil, errors.New("callback returned a different client ID than requested")
		}
		clientID = issued
	}
	if clientID == "" || clientID == DynamicClient {
		return nil, errors.New("registration incomplete: no issued client ID in the callback")
	}
	code := cb.Get("code")
	if code == "" {
		return nil, errors.New("callback has no authorization code")
	}
	tr, err := o.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {p.Verifier},
		"redirect_uri":  {p.RedirectURI},
		"resource":      {Resource},
	})
	if err != nil {
		return nil, err
	}
	claims, err := o.validateIDToken(ctx, tr.IDToken, clientID, p.Nonce)
	if err != nil {
		return nil, err
	}
	cred := &Credentials{
		ClientID:     clientID,
		Sub:          claims.Subject,
		Email:        claims.Email,
		IDToken:      tr.IDToken,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Scopes:       strings.Fields(tr.Scope),
		ExpiresAt:    o.now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		HostID:       p.HostID,
	}
	if !cred.HasPlan() {
		return cred, ErrPlanNotGranted
	}
	return cred, nil
}

// Refresh renews the access token. ChatGPT refresh tokens rotate; the returned
// credentials carry the replacement and must be saved together.
func (o *OAuth) Refresh(ctx context.Context, c *Credentials) (*Credentials, error) {
	if c == nil || c.RefreshToken == "" {
		return nil, ErrNotSignedIn
	}
	tr, err := o.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.ClientID},
		"refresh_token": {c.RefreshToken},
		"resource":      {Resource},
	})
	if err != nil {
		return nil, err
	}
	next := *c
	next.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		next.RefreshToken = tr.RefreshToken
	}
	if tr.IDToken != "" {
		next.IDToken = tr.IDToken
	}
	if tr.Scope != "" {
		next.Scopes = strings.Fields(tr.Scope)
	}
	if tr.ExpiresIn > 0 {
		next.ExpiresAt = o.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return &next, nil
}

func (o *OAuth) token(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr tokenResponse
	_ = json.Unmarshal(raw, &tr)
	if resp.StatusCode != http.StatusOK || tr.Error != "" {
		if tr.Error != "" {
			return nil, fmt.Errorf("token endpoint: %s %s", tr.Error, tr.ErrorDesc)
		}
		return nil, fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access token")
	}
	return &tr, nil
}

// Revoke signs out remotely through the advertised revocation endpoint.
func (o *OAuth) Revoke(ctx context.Context, c *Credentials) error {
	if c == nil || c.RefreshToken == "" {
		return nil
	}
	d, err := o.discover(ctx)
	if err != nil {
		return err
	}
	if d.RevocationEndpoint == "" {
		return errors.New("no revocation endpoint advertised")
	}
	form := url.Values{"token": {c.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {c.ClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.RevocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revocation returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// --- discovery and ID token validation ---

func (o *OAuth) discover(ctx context.Context) (*discovery, error) {
	o.mu.Lock()
	if o.disc != nil {
		d := o.disc
		o.mu.Unlock()
		return d, nil
	}
	o.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base()+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var d discovery
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil || d.JWKSURI == "" {
		return nil, fmt.Errorf("openid configuration unavailable: %v", err)
	}
	o.mu.Lock()
	o.disc = &d
	o.mu.Unlock()
	return &d, nil
}

func (o *OAuth) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	o.mu.Lock()
	if o.keys != nil {
		if key := o.keys[kid]; key != nil {
			o.mu.Unlock()
			return key, nil
		}
	}
	o.mu.Unlock()

	d, err := o.discover(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	o.mu.Lock()
	o.keys = keys
	o.mu.Unlock()
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("signing key %q not found", kid)
}

// validateIDToken verifies the signature against the published JWKS and checks
// issuer, audience, expiry, nonce and subject.
func (o *OAuth) validateIDToken(ctx context.Context, raw, clientID, nonce string) (*idClaims, error) {
	if raw == "" {
		return nil, errors.New("no ID token returned")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("ID token is not a JWT")
	}
	headerRaw, err := decodeJWTPart(parts[0])
	if err != nil {
		return nil, fmt.Errorf("ID token header: %w", err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, fmt.Errorf("ID token header: %w", err)
	}
	key, err := o.key(ctx, header.Kid)
	if err != nil {
		return nil, err
	}
	claims, err := parseAndVerifyJWT(raw, key)
	if err != nil {
		return nil, fmt.Errorf("invalid ID token: %w", err)
	}
	issuer := o.issuer()
	if d, err := o.discover(ctx); err == nil && strings.TrimSpace(d.Issuer) != "" {
		// Prefer the issuer the authorization server publishes. A deployment
		// may front the same server on a different base URL.
		issuer = strings.TrimRight(d.Issuer, "/")
	}
	if err := validateClaims(claims, issuer, clientID, nonce, o.now()); err != nil {
		return nil, fmt.Errorf("invalid ID token: %w", err)
	}
	return claims, nil
}
