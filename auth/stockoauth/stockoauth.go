// Package stockoauth is the module-owned OAuth2 authorization server for
// stock PocketBase (ported from customjs authentication/oauth.go, hardened
// per docs/mcp-server.md, "Stock OAuth"). It lives in its own nested module
// because the root module is stdlib-only and this provider needs PocketBase,
// while the fork adapter must never compile it (duplicate OAuth ownership).
package stockoauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/security"

	pbmcp "github.com/compdani/pocketbase-mcp"
)

// UpstreamAuthenticator is an optional hook for hosts migrating from
// customjs's upstream auth_server_url login. It is never used to POST
// credentials by this package itself; when nil, local PB auth is used.
type UpstreamAuthenticator interface {
	Authenticate(ctx context.Context, app core.App, collection *core.Collection, identity string, password string) (*core.Record, error)
}

// Config configures a Provider.
type Config struct {
	// AuthCollection is the configured auth collection name or ID (required).
	AuthCollection string
	// PublicBaseURL is the externally visible base URL (issuer). Derived
	// from the request when empty.
	PublicBaseURL string
	// MCPPath is the MCP mount used in protected-resource metadata.
	MCPPath string
	// AliasPaths are MCP alias mounts (e.g. /sse) that also get discovery copies.
	AliasPaths   []string
	LoginPath    string // default /oauth/login (also the authorization endpoint)
	TokenPath    string // default /oauth/token
	UserinfoPath string // default /oauth/userinfo
	RegisterPath string // default /oauth/register (RFC 7591 DCR)
	// TokenDuration, when >0, issues static non-refreshable tokens.
	TokenDuration time.Duration
	// CodeDuration defaults to 5 minutes; TransactionDuration to 10 minutes.
	CodeDuration        time.Duration
	TransactionDuration time.Duration
	// Namespace prefixes the private collections. Default "mcp_oauth_".
	Namespace string
	// LoginTemplate overrides the default login page (html/template with
	// fields ClientName, CSRF, Error, Identity, LoginPath, RedirectHost,
	// Unverified). Custom templates must show RedirectHost and the
	// Unverified warning for self-registered clients.
	LoginTemplate string
	// Upstream is an optional migration hook; nil means local auth.
	Upstream UpstreamAuthenticator
	// RegistrationPerHourPerIP caps DCR per IP. Default 10.
	RegistrationPerHourPerIP int
	// DisableRegistration turns off DCR (hand-created clients only).
	DisableRegistration bool
	// RegistrationRedirectHosts, when non-empty, limits DCR redirect URIs to
	// these hosts (loopback is always allowed).
	RegistrationRedirectHosts []string
}

func (c Config) withDefaults() Config {
	if c.MCPPath == "" {
		c.MCPPath = "/mcp"
	}
	if c.LoginPath == "" {
		c.LoginPath = "/oauth/login"
	}
	if c.TokenPath == "" {
		c.TokenPath = "/oauth/token"
	}
	if c.UserinfoPath == "" {
		c.UserinfoPath = "/oauth/userinfo"
	}
	if c.RegisterPath == "" {
		c.RegisterPath = "/oauth/register"
	}
	if c.CodeDuration <= 0 {
		c.CodeDuration = 5 * time.Minute
	}
	if c.TransactionDuration <= 0 {
		c.TransactionDuration = 10 * time.Minute
	}
	if c.Namespace == "" {
		c.Namespace = "mcp_oauth_"
	}
	if c.RegistrationPerHourPerIP <= 0 {
		c.RegistrationPerHourPerIP = 10
	}
	return c
}

// Collection names (namespaced, private, never MCP-exposed).
func (c Config) ClientsCollection() string      { return c.Namespace + "clients" }
func (c Config) TransactionsCollection() string { return c.Namespace + "transactions" }
func (c Config) CodesCollection() string        { return c.Namespace + "codes" }

// Provider is a per-app stock OAuth provider. No package-level app state
// beyond the duplicate-registration guard keyed by app+collection.
type Provider struct {
	app          core.App
	cfg          Config
	collectionID string
	collection   string // canonical auth collection name

	mu         sync.Mutex
	registered bool
	regHits    map[string][]time.Time
}

var guardMu sync.Mutex
var guardKeys = map[string]bool{}

// New validates the auth collection and provisions the private collections.
// It does not register routes; call RegisterRoutes on the serve router.
func New(app core.App, cfg Config) (*Provider, error) {
	cfg = cfg.withDefaults()
	if !strings.HasPrefix(cfg.Namespace, "mcp_oauth_") {
		return nil, fmt.Errorf("stockoauth: Namespace must start with reserved mcp_oauth_ prefix")
	}
	paths := []string{cfg.LoginPath, cfg.TokenPath, cfg.UserinfoPath, cfg.RegisterPath, cfg.MCPPath}
	seen := map[string]bool{}
	for _, path := range paths {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#{} ") || strings.HasPrefix(path, "/.well-known/") || seen[path] {
			return nil, fmt.Errorf("stockoauth: invalid or conflicting endpoint path %q", path)
		}
		seen[path] = true
	}
	for _, alias := range cfg.AliasPaths {
		if seen[alias] {
			return nil, fmt.Errorf("stockoauth: conflicting alias path %q", alias)
		}
	}

	if strings.TrimSpace(cfg.AuthCollection) == "" {
		return nil, fmt.Errorf("stockoauth: AuthCollection is required")
	}
	col, err := app.FindCollectionByNameOrId(cfg.AuthCollection)
	if err != nil || col == nil {
		return nil, fmt.Errorf("stockoauth: auth collection %q not found", cfg.AuthCollection)
	}
	if col.Name == core.CollectionNameSuperusers {
		return nil, fmt.Errorf("stockoauth: _superusers cannot be the OAuth auth collection")
	}
	if !col.IsAuth() {
		return nil, fmt.Errorf("stockoauth: collection %q is not an auth collection", col.Name)
	}
	if !col.PasswordAuth.Enabled {
		return nil, fmt.Errorf("stockoauth: collection %q does not support password login (passwordAuth is disabled)", col.Name)
	}
	key := fmt.Sprintf("%p|%s", app, col.Id)
	guardMu.Lock()
	if guardKeys[key] {
		guardMu.Unlock()
		return nil, fmt.Errorf("stockoauth: duplicate OAuth registration for app and auth collection %q", col.Name)
	}
	guardKeys[key] = true
	guardMu.Unlock()
	p := &Provider{app: app, cfg: cfg, collectionID: col.Id, collection: col.Name, regHits: map[string][]time.Time{}}
	if err := app.RunInTransaction(func(tx core.App) error {
		original := p.app
		p.app = tx
		defer func() { p.app = original }()
		return p.provision()
	}); err != nil {
		guardMu.Lock()
		delete(guardKeys, key)
		guardMu.Unlock()
		return nil, err
	}
	app.Logger().Info("MCP stock OAuth provider ready", "collection", col.Name, "collectionId", col.Id, "login", cfg.LoginPath, "token", cfg.TokenPath)
	return p, nil
}

// CollectionID returns the resolved stable auth collection ID.
func (p *Provider) CollectionID() string { return p.collectionID }

// CollectionName returns the canonical auth collection name.
func (p *Provider) CollectionName() string { return p.collection }

// Config returns the resolved config.
func (p *Provider) Config() Config { return p.cfg }

func expectedFields() map[string]map[string]string {
	return map[string]map[string]string{
		"clients": {
			"client_id": "text", "name": "text", "secret_hash": "text", "redirect_uris": "json",
			"client_type": "select", "auth_method": "text", "self_registered": "bool", "revoked": "bool", "auth_collection_id": "text",
		},
		"transactions": {
			"tx_hash": "text", "client_id": "text", "redirect_uri": "text", "state": "text", "scope": "text",
			"code_challenge": "text", "code_challenge_method": "text", "auth_collection_id": "text",
			"csrf_hash": "text", "expires_at": "date", "user_id": "text", "user_collection_id": "text",
		},
		"codes": {
			"code_hash": "text", "client_id": "text", "redirect_uri": "text", "code_challenge": "text",
			"auth_collection_id": "text", "user_id": "text", "user_collection_id": "text",
			"token_key_hash": "text", "scope": "text", "expires_at": "date", "consumed": "bool",
		},
	}
}

func (p *Provider) provision() error {
	names := map[string]string{
		"clients": p.cfg.ClientsCollection(), "transactions": p.cfg.TransactionsCollection(), "codes": p.cfg.CodesCollection(),
	}
	for kind, name := range names {
		want := expectedFields()[kind]
		existing, err := p.app.FindCollectionByNameOrId(name)
		if err == nil && existing != nil {
			if !existing.IsBase() || existing.ListRule != nil || existing.ViewRule != nil || existing.CreateRule != nil || existing.UpdateRule != nil || existing.DeleteRule != nil {
				return fmt.Errorf("stockoauth: conflicting existing schema for %q: storage must be private base collections", name)
			}
			for fname, ftype := range want {
				f := existing.Fields.GetByName(fname)
				if f == nil {
					return fmt.Errorf("stockoauth: conflicting existing schema for collection %q: missing field %q (remove the collection or migrate it to the expected schema)", name, fname)
				}
				if (fname == "secret_hash" || fname == "code_hash" || fname == "tx_hash" || fname == "csrf_hash" || fname == "token_key_hash") && !f.GetHidden() {
					return fmt.Errorf("stockoauth: conflicting existing schema for %q: field %q must be hidden", name, fname)
				}
				if f.Type() != ftype {
					return fmt.Errorf("stockoauth: conflicting existing schema for collection %q: field %q has type %q, expected %q", name, fname, f.Type(), ftype)
				}
			}
			continue
		}
		col := core.NewBaseCollection(name)
		// Private: nil API rules mean only superusers can access via the API;
		// the collections are never added to any MCP manifest.
		for fname := range want {
			switch want[fname] {
			case "text":
				hidden := fname == "secret_hash" || fname == "code_hash" || fname == "tx_hash" || fname == "csrf_hash" || fname == "token_key_hash"
				col.Fields.Add(&core.TextField{Name: fname, Hidden: hidden})
			case "json":
				col.Fields.Add(&core.JSONField{Name: fname})
			case "select":
				col.Fields.Add(&core.SelectField{Name: fname, Values: []string{"confidential", "public"}, MaxSelect: 1})
			case "bool":
				col.Fields.Add(&core.BoolField{Name: fname})
			case "date":
				col.Fields.Add(&core.DateField{Name: fname})
			}
		}
		if err := p.app.Save(col); err != nil {
			return fmt.Errorf("stockoauth: provision collection %q: %w", name, err)
		}
	}
	return nil
}

// RegisterRoutes mounts all OAuth + discovery routes. It fails on a second
// call for the same provider (duplicate-registration guard).
func (p *Provider) RegisterRoutes(r *router.Router[*core.RequestEvent]) error {
	// Preflight the complete route plan before mutating the host router.
	routes := map[string][]string{"GET": {p.cfg.LoginPath, p.cfg.UserinfoPath, "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration", "/.well-known/oauth-protected-resource", pbmcp.ProtectedResourcePath(p.cfg.MCPPath)}, "POST": {p.cfg.LoginPath, p.cfg.TokenPath}}
	if !p.cfg.DisableRegistration {
		routes["POST"] = append(routes["POST"], p.cfg.RegisterPath)
	}
	for _, alias := range p.cfg.AliasPaths {
		alias = strings.TrimRight(alias, "/")
		if alias == "" {
			continue
		}
		routes["GET"] = append(routes["GET"], alias+"/.well-known/oauth-authorization-server", alias+"/.well-known/openid-configuration", alias+"/.well-known/oauth-protected-resource", alias+pbmcp.ProtectedResourcePath(p.cfg.MCPPath), alias+"/.well-known/oauth-protected-resource"+alias)
	}
	for method, paths := range routes {
		seen := map[string]bool{}
		for _, path := range paths {
			if seen[path] || r.HasRoute(method, path) {
				return fmt.Errorf("stockoauth: route conflict: %s %s", method, path)
			}
			seen[path] = true
		}
	}
	p.mu.Lock()
	if p.registered {
		p.mu.Unlock()
		return fmt.Errorf("stockoauth: routes already registered for auth collection %q", p.collection)
	}
	p.registered = true
	p.mu.Unlock()

	r.GET(p.cfg.LoginPath, p.handleAuthorizeGet)
	r.POST(p.cfg.LoginPath, p.handleAuthorizePost)
	r.POST(p.cfg.TokenPath, p.handleToken)
	r.GET(p.cfg.UserinfoPath, p.handleUserinfo).Bind(apis.RequireAuth())
	if !p.cfg.DisableRegistration {
		r.POST(p.cfg.RegisterPath, p.handleRegister)
	}
	discovery := func(e *core.RequestEvent) error { return p.handleDiscovery(e, false) }
	oidc := func(e *core.RequestEvent) error { return p.handleDiscovery(e, true) }
	r.GET("/.well-known/oauth-authorization-server", discovery)
	r.GET("/.well-known/openid-configuration", oidc)
	r.GET("/.well-known/oauth-protected-resource", p.handleProtectedResource)
	r.GET(pbmcp.ProtectedResourcePath(p.cfg.MCPPath), p.handleProtectedResource)
	for _, alias := range p.cfg.AliasPaths {
		alias = strings.TrimRight(alias, "/")
		if alias == "" {
			continue
		}
		r.GET(alias+"/.well-known/oauth-authorization-server", discovery)
		r.GET(alias+"/.well-known/openid-configuration", oidc)
		r.GET(alias+"/.well-known/oauth-protected-resource", p.handleProtectedResource)
		r.GET(alias+pbmcp.ProtectedResourcePath(p.cfg.MCPPath), p.handleProtectedResource)
		// Compatibility copies under the alias mount itself (customjs /sse/...).
		r.GET(alias+"/.well-known/oauth-protected-resource"+alias, p.handleProtectedResource)
	}
	return nil
}

func (p *Provider) publicBase(e *core.RequestEvent) string {
	if p.cfg.PublicBaseURL != "" {
		return strings.TrimRight(p.cfg.PublicBaseURL, "/")
	}
	scheme := "http"
	if proto := strings.TrimSpace(e.Request.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	} else if e.Request.TLS != nil {
		scheme = "https"
	}
	host := strings.TrimSpace(e.Request.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = e.Request.Host
	}
	return scheme + "://" + host
}

func (p *Provider) handleDiscovery(e *core.RequestEvent, _ bool) error {
	issuer := p.publicBase(e)
	reg := p.cfg.RegisterPath
	if p.cfg.DisableRegistration {
		reg = ""
	}
	return e.JSON(http.StatusOK, pbmcp.AuthorizationServerMetadata(issuer, p.cfg.LoginPath, p.cfg.TokenPath, p.cfg.UserinfoPath, reg))
}

func (p *Provider) handleProtectedResource(e *core.RequestEvent) error {
	base := p.publicBase(e)
	return e.JSON(http.StatusOK, pbmcp.ProtectedResourceMetadata(base, p.cfg.MCPPath, []string{base}))
}

// --- client helpers ---

func hashValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func hashSecret(secret string) string {
	salt := security.RandomString(16)
	sum := sha256.Sum256([]byte(salt + "|" + secret))
	return "s1$" + salt + "$" + hex.EncodeToString(sum[:])
}

func verifySecret(stored string, secret string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 3 || parts[0] != "s1" {
		return false
	}
	sum := sha256.Sum256([]byte(parts[1] + "|" + secret))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(parts[2])) == 1
}

type clientRecord struct {
	Record         *core.Record
	ClientID       string
	Name           string
	RedirectURIs   []string
	ClientType     string // confidential|public
	AuthMethod     string
	SelfRegistered bool
	Revoked        bool
}

func (p *Provider) findClient(clientID string) (*clientRecord, error) {
	if clientID == "" {
		return nil, fmt.Errorf("missing client_id")
	}
	rec, err := p.app.FindFirstRecordByData(p.cfg.ClientsCollection(), "client_id", clientID)
	if err != nil || rec == nil {
		return nil, fmt.Errorf("unknown client")
	}
	if rec.GetString("auth_collection_id") != p.collectionID {
		return nil, fmt.Errorf("client is bound to a different auth collection")
	}
	if kind := rec.GetString("client_type"); kind != "public" && kind != "confidential" {
		return nil, fmt.Errorf("invalid client type")
	}
	return &clientRecord{
		Record: rec, ClientID: rec.GetString("client_id"), Name: rec.GetString("name"),
		RedirectURIs: decodeURIs(rec), ClientType: rec.GetString("client_type"),
		AuthMethod: rec.GetString("auth_method"), SelfRegistered: rec.GetBool("self_registered"), Revoked: rec.GetBool("revoked"),
	}, nil
}

func decodeURIs(rec *core.Record) []string {
	if v := rec.GetStringSlice("redirect_uris"); len(v) > 0 {
		return v
	}
	raw := strings.TrimSpace(rec.GetString("redirect_uris"))
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err == nil {
		return out
	}
	return []string{raw}
}

func (c *clientRecord) requiresPKCE() bool {
	return c.ClientType == "public" || c.SelfRegistered
}

// CreateClient creates a client bound to this provider's auth collection.
// For confidential clients the secret is returned once and only its hash is
// stored. Public clients get an empty secret.
func (p *Provider) CreateClient(name string, redirectURIs []string, public bool) (string, string, error) {
	if err := ValidateRedirectURIsForRegistration(redirectURIs, public); err != nil && !public {
		// Hand-created confidential clients may use any https URI (ChatGPT
		// baseline), but still require syntactically valid absolute URIs.
		if err2 := validateAbsoluteURIs(redirectURIs); err2 != nil {
			return "", "", err2
		}
	} else if err != nil {
		return "", "", err
	}
	col, err := p.app.FindCollectionByNameOrId(p.cfg.ClientsCollection())
	if err != nil {
		return "", "", err
	}
	clientID := "mcp_" + security.RandomString(24)
	secret := ""
	rec := core.NewRecord(col)
	rec.Set("client_id", clientID)
	rec.Set("name", name)
	rec.Set("redirect_uris", redirectURIs)
	rec.Set("auth_collection_id", p.collectionID)
	rec.Set("revoked", false)
	rec.Set("self_registered", false)
	if public {
		rec.Set("client_type", "public")
		rec.Set("auth_method", "none")
	} else {
		secret = security.RandomString(40)
		rec.Set("client_type", "confidential")
		rec.Set("auth_method", "client_secret_post")
		rec.Set("secret_hash", hashSecret(secret))
	}
	if err := p.app.Save(rec); err != nil {
		return "", "", err
	}
	return clientID, secret, nil
}

// CreateHandClient is CreateClient for a confidential (ChatGPT-style) client.
func (p *Provider) CreateHandClient(name string, redirectURIs []string) (string, string, error) {
	return p.CreateClient(name, redirectURIs, false)
}

// RevokeClient marks a client revoked (admin operation).
func (p *Provider) RevokeClient(clientID string) error {
	c, err := p.findClient(clientID)
	if err != nil {
		return err
	}
	c.Record.Set("revoked", true)
	return p.app.Save(c.Record)
}

// --- redirect validation ---

var pkceRe = regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`)

func isLoopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// IsLegacyDomainOnlyRedirect reports whether a registered URI is an old
// customjs domain-only value (scheme://host with no callback path).
func IsLegacyDomainOnlyRedirect(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}

// MigrationMessage explains the oauth_apps migration for a legacy value.
func MigrationMessage(registered string) string {
	return fmt.Sprintf("registered redirect_uri %q is a legacy domain-only value from customjs oauth_apps: exact redirect matching now requires the full callback URL (e.g. %s) to be stored in the client's redirect_uris; update the oauth_apps/client record to contain the complete redirect URI registered with the client (for ChatGPT, the full callback URL shown in the connector settings)", registered, strings.TrimRight(registered, "/")+"/callback")
}

func validateAbsoluteURIs(uris []string) error {
	if len(uris) == 0 {
		return fmt.Errorf("at least one redirect_uri is required")
	}
	for _, u := range uris {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Fragment != "" {
			return fmt.Errorf("invalid redirect_uri %q: must be an absolute URI without a fragment", u)
		}
	}
	return nil
}

// ValidateRedirectURIsForRegistration enforces DCR rules: https, or http
// only on loopback.
func ValidateRedirectURIsForRegistration(uris []string, _ bool) error {
	if err := validateAbsoluteURIs(uris); err != nil {
		return err
	}
	for _, u := range uris {
		parsed, _ := url.Parse(u)
		if parsed.Scheme == "https" {
			continue
		}
		if parsed.Scheme == "http" && isLoopbackHost(parsed.Host) {
			continue
		}
		return fmt.Errorf("invalid redirect_uri %q: only https redirect URIs are allowed (http is allowed only for loopback)", u)
	}
	return nil
}

// matchesRedirect checks exact match, with loopback port flexibility only
// for public/native clients.
func matchesRedirect(registered []string, provided string, client *clientRecord) (bool, bool) {
	// legacy reports whether a domain-only registered value was involved.
	legacy := false
	for _, reg := range registered {
		if reg == provided {
			return true, false
		}
		if IsLegacyDomainOnlyRedirect(reg) {
			legacy = true
		}
	}
	if client != nil && client.ClientType == "public" {
		pu, err1 := url.Parse(provided)
		if err1 == nil && isLoopbackHost(pu.Host) {
			for _, reg := range registered {
				ru, err := url.Parse(reg)
				if err != nil || !isLoopbackHost(ru.Host) {
					continue
				}
				rh, _, _ := net.SplitHostPort(ru.Host)
				if rh == "" {
					rh = ru.Host
				}
				ph, _, _ := net.SplitHostPort(pu.Host)
				if ph == "" {
					ph = pu.Host
				}
				if ru.Scheme == pu.Scheme && rh == ph && ru.Path == pu.Path && ru.RawQuery == pu.RawQuery {
					return true, false
				}
			}
		}
	}
	return false, legacy
}

// ImportHandClient migrates a customjs oauth_apps record without changing
// the client ID or secret entered in ChatGPT. It never stores the raw secret.
func (p *Provider) ImportHandClient(id, secret, name string, redirects []string) error {
	if id == "" || secret == "" {
		return fmt.Errorf("client ID and secret are required")
	}
	if err := validateAbsoluteURIs(redirects); err != nil {
		return err
	}
	for _, uri := range redirects {
		if IsLegacyDomainOnlyRedirect(uri) {
			return fmt.Errorf("%s", MigrationMessage(uri))
		}
	}
	if _, err := p.app.FindFirstRecordByData(p.cfg.ClientsCollection(), "client_id", id); err == nil {
		return fmt.Errorf("client ID already exists")
	}
	col, err := p.app.FindCollectionByNameOrId(p.cfg.ClientsCollection())
	if err != nil {
		return err
	}
	rec := core.NewRecord(col)
	rec.Set("client_id", id)
	rec.Set("secret_hash", hashSecret(secret))
	rec.Set("name", name)
	rec.Set("redirect_uris", redirects)
	rec.Set("client_type", "confidential")
	rec.Set("auth_method", "client_secret_post")
	rec.Set("auth_collection_id", p.collectionID)
	return p.app.Save(rec)
}
