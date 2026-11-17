package stockoauth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
)

// handleRegister implements RFC 7591 Dynamic Client Registration.
// Only public clients (token_endpoint_auth_method "none", PKCE required)
// can self-register; registrations are stored as ordinary client records
// so they are admin-visible and revocable (see RevokeClient).
func (p *Provider) handleRegister(e *core.RequestEvent) error {
	e.Response.Header().Set("Cache-Control", "no-store")
	ip := e.RealIP()
	p.mu.Lock()
	now := time.Now()
	hits := p.regHits[ip]
	kept := hits[:0]
	for _, t := range hits {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	if len(kept) >= p.cfg.RegistrationPerHourPerIP {
		p.mu.Unlock()
		return oauthError(e, http.StatusTooManyRequests, "invalid_request", "Registration rate limit exceeded. Try again later.")
	}
	p.regHits[ip] = append(kept, now)
	p.mu.Unlock()

	body, err := io.ReadAll(io.LimitReader(e.Request.Body, 32<<10))
	if err != nil {
		return oauthError(e, http.StatusBadRequest, "invalid_request", "Invalid request body.")
	}
	var req struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		Scope                   string   `json:"scope"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return oauthError(e, http.StatusBadRequest, "invalid_request", "Invalid JSON.")
	}
	if req.TokenEndpointAuthMethod != "" && req.TokenEndpointAuthMethod != "none" {
		return oauthError(e, http.StatusBadRequest, "invalid_client_metadata", "Only token_endpoint_auth_method \"none\" (public clients) is supported for self-registration.")
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" {
			return oauthError(e, http.StatusBadRequest, "invalid_client_metadata", "Only grant_types [\"authorization_code\"] is supported.")
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			return oauthError(e, http.StatusBadRequest, "invalid_client_metadata", "Only response_types [\"code\"] is supported.")
		}
	}
	if err := ValidateRedirectURIsForRegistration(req.RedirectURIs, true); err != nil {
		return oauthError(e, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
	}
	if hosts := p.cfg.RegistrationRedirectHosts; len(hosts) > 0 {
		for _, raw := range req.RedirectURIs {
			u, _ := url.Parse(raw)
			if isLoopbackHost(u.Host) {
				continue
			}
			allowed := false
			for _, h := range hosts {
				if strings.EqualFold(strings.TrimSpace(h), u.Hostname()) {
					allowed = true
					break
				}
			}
			if !allowed {
				return oauthError(e, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri host "+u.Hostname()+" is not allowed for self-registered clients")
			}
		}
	}
	col, err := p.app.FindCollectionByNameOrId(p.cfg.ClientsCollection())
	if err != nil {
		return oauthError(e, http.StatusInternalServerError, "server_error", "Client storage unavailable.")
	}
	clientID := "mcp_" + security.RandomString(24)
	rec := core.NewRecord(col)
	rec.Set("client_id", clientID)
	rec.Set("name", strings.TrimSpace(req.ClientName))
	rec.Set("redirect_uris", req.RedirectURIs)
	rec.Set("client_type", "public")
	rec.Set("auth_method", "none")
	rec.Set("self_registered", true)
	rec.Set("revoked", false)
	rec.Set("auth_collection_id", p.collectionID)
	if err := p.app.Save(rec); err != nil {
		return oauthError(e, http.StatusInternalServerError, "server_error", "Failed to register client.")
	}
	return e.JSON(http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_name":                req.ClientName,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      req.Scope,
	})
}
