package pocketbasemcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OAuthEnabled reports whether OAuth is enabled in config (mode != off).
func (c Config) OAuthEnabled() bool {
	if c.OAuth.Mode != "" && strings.EqualFold(c.OAuth.Mode, OAuthModeOff) {
		return false
	}
	return c.OAuth.Enabled || (c.OAuth.Mode != "" && !strings.EqualFold(c.OAuth.Mode, OAuthModeAuto))
}

// OAuthCollectionName returns the configured OAuth auth collection name/ID.
func (c Config) OAuthCollectionName() string {
	if c.OAuth.Collection != "" {
		return c.OAuth.Collection
	}
	return c.Auth.Collection
}

// EffectiveAuthCollections returns accepted collections, defaulting to the
// OAuth collection when OAuth is on and Collections is empty.
func (c Config) EffectiveAuthCollections() []string {
	if len(c.Auth.Collections) > 0 {
		return append([]string(nil), c.Auth.Collections...)
	}
	if c.OAuthEnabled() {
		if name := c.OAuthCollectionName(); name != "" {
			return []string{name}
		}
	}
	return nil
}

// PublicBase returns the configured public base URL without trailing slash.
func (c Config) PublicBase() string {
	for _, v := range []string{c.OAuth.PublicBaseURL, c.PublicURL, c.BaseURL} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimRight(strings.TrimSpace(v), "/")
		}
	}
	return ""
}

// PublicBaseFor returns the configured public base or derives it from r.
func (c Config) PublicBaseFor(r *http.Request) string {
	if b := c.PublicBase(); b != "" {
		return b
	}
	if r == nil {
		return ""
	}
	scheme := "http"
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// OAuthPlan is the immutable registration plan computed before routes/hooks.
type OAuthPlan struct {
	Enabled        bool
	RequestedMode  string // normalized requested mode (auto/native/module/external/off)
	EffectiveMode  string // module|native|external|off
	Owner          string // module|native|external|none
	HostKind       string // stock|fork
	Collection     string // configured name
	CollectionID   string // resolved stable ID (adapters fill in)
	Mount          string // MCP path
	PublicBase     string
	ExternalIssuer string
}

// ResolveOAuthMode maps (enabled, mode, hostKind) to an effective owner.
func ResolveOAuthMode(enabled bool, mode string, hostKind string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = OAuthModeAuto
	}
	switch m {
	case OAuthModeAuto, OAuthModeNative, OAuthModeModule, OAuthModeExternal, OAuthModeOff:
	default:
		return "", fmt.Errorf("oauth: unknown mode %q (valid: auto, native, module, external, off)", mode)
	}
	if m == OAuthModeOff {
		return OAuthModeOff, nil
	}
	if !enabled && mode == "" {
		return OAuthModeOff, nil
	}
	// An explicit non-off mode enables OAuth even if Enabled was left false,
	// so Mode:"module" alone is not silently bearer-only.
	_ = enabled
	switch m {
	case OAuthModeAuto:
		if hostKind == "fork" {
			return OAuthModeNative, nil
		}
		return OAuthModeModule, nil
	case OAuthModeNative:
		if hostKind == "fork" {
			return OAuthModeNative, nil
		}
		return "", fmt.Errorf("oauth: mode %q is not supported by the stock adapter (stock PocketBase has no native OAuth2 server); use mode %q or %q", OAuthModeNative, OAuthModeModule, OAuthModeAuto)
	case OAuthModeModule:
		if hostKind == "fork" {
			return "", fmt.Errorf("oauth: mode %q is rejected by the fork adapter to prevent duplicate OAuth ownership; use mode %q or %q", OAuthModeModule, OAuthModeNative, OAuthModeAuto)
		}
		return OAuthModeModule, nil
	case OAuthModeExternal:
		return OAuthModeExternal, nil
	}
	return OAuthModeOff, nil
}

// BuildOAuthPlan computes the plan. collectionID may be empty at root level.
func BuildOAuthPlan(cfg Config, hostKind string, collectionID string) (OAuthPlan, error) {
	enabled := cfg.OAuthEnabled()
	requested := strings.ToLower(strings.TrimSpace(cfg.OAuth.Mode))
	if requested == "" {
		if enabled {
			requested = OAuthModeAuto
		} else {
			requested = OAuthModeOff
		}
	}
	effective, err := ResolveOAuthMode(enabled, cfg.OAuth.Mode, hostKind)
	if err != nil {
		return OAuthPlan{}, err
	}
	if !enabled {
		effective = OAuthModeOff
	}
	plan := OAuthPlan{
		Enabled: effective != OAuthModeOff, RequestedMode: requested, EffectiveMode: effective,
		HostKind: hostKind, Collection: cfg.OAuthCollectionName(), CollectionID: collectionID,
		Mount: cfg.Path, PublicBase: cfg.PublicBase(), ExternalIssuer: cfg.OAuth.ExternalIssuer,
	}
	if plan.Mount == "" {
		plan.Mount = "/mcp"
	}
	switch effective {
	case OAuthModeModule:
		plan.Owner = "module"
	case OAuthModeNative:
		plan.Owner = "native"
	case OAuthModeExternal:
		plan.Owner = "external"
		if strings.TrimSpace(cfg.OAuth.ExternalIssuer) == "" {
			return OAuthPlan{}, fmt.Errorf("oauth: mode %q requires OAuth.ExternalIssuer", OAuthModeExternal)
		}
	default:
		plan.Owner = "none"
	}
	if plan.Enabled && plan.Collection == "" && effective != OAuthModeExternal {
		return OAuthPlan{}, fmt.Errorf("oauth: an auth collection is required (set Auth.Collection or OAuth.Collection)")
	}
	if plan.Collection == "_superusers" {
		return OAuthPlan{}, fmt.Errorf("oauth: _superusers cannot be the OAuth auth collection")
	}
	return plan, nil
}

// Report returns a credential-free one-line startup report.
func (p OAuthPlan) Report() string {
	return fmt.Sprintf("oauth owner=%s mode=%s requested=%s host=%s collection=%s collectionId=%s mount=%s publicBase=%s",
		p.Owner, p.EffectiveMode, p.RequestedMode, p.HostKind, p.Collection, p.CollectionID, p.Mount, p.PublicBase)
}

// ProtectedResourcePath returns the well-known path for an MCP mount.
func ProtectedResourcePath(mcpPath string) string {
	if mcpPath == "" || mcpPath == "/" {
		return "/.well-known/oauth-protected-resource"
	}
	return "/.well-known/oauth-protected-resource" + mcpPath
}

// ProtectedResourceMetadata builds the RFC 9728 document.
func ProtectedResourceMetadata(publicBase string, mcpPath string, authorizationServers []string) map[string]any {
	resource := strings.TrimRight(publicBase, "/") + mcpPath
	return map[string]any{
		"resource":                 resource,
		"authorization_servers":    authorizationServers,
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"openid", "profile", "email"},
	}
}

// AuthorizationServerMetadata builds the RFC 8414 document. ID tokens are
// not issued in v1, so no id_token signing algorithms are advertised.
func AuthorizationServerMetadata(issuer string, loginPath string, tokenPath string, userinfoPath string, registerPath string) map[string]any {
	issuer = strings.TrimRight(issuer, "/")
	md := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + loginPath,
		"token_endpoint":                        issuer + tokenPath,
		"userinfo_endpoint":                     issuer + userinfoPath,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic", "none"},
		"code_challenge_methods_supported":      []string{"S256"},
		"subject_types_supported":               []string{"public"},
	}
	if registerPath != "" {
		md["registration_endpoint"] = issuer + registerPath
	}
	return md
}

// WWWAuthenticateValue builds the 401 challenge for an MCP request.
func (s *Server) WWWAuthenticateValue(r *http.Request) string {
	if !s.cfg.OAuthEnabled() {
		return ""
	}
	base := s.cfg.PublicBaseFor(r)
	return fmt.Sprintf(`Bearer resource_metadata="%s%s"`, base, ProtectedResourcePath(s.cfg.Path))
}

func (s *Server) writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if v := s.WWWAuthenticateValue(r); v != "" {
		w.Header().Set("WWW-Authenticate", v)
	}
	writePlainError(w, http.StatusUnauthorized, "Unauthorized")
}

// ProtectedResourceHandler serves the protected-resource metadata (stdlib).
func (s *Server) ProtectedResourceHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := s.cfg.PublicBaseFor(r)
		issuer := base
		if s.cfg.OAuth.EffectiveModeExternal() {
			issuer = strings.TrimRight(s.cfg.OAuth.ExternalIssuer, "/")
		}
		writeJSON(w, http.StatusOK, ProtectedResourceMetadata(base, s.cfg.Path, []string{issuer}))
	})
}

// EffectiveModeExternal reports whether the configured mode is external.
func (o OAuthConfig) EffectiveModeExternal() bool {
	return strings.EqualFold(strings.TrimSpace(o.Mode), OAuthModeExternal)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
