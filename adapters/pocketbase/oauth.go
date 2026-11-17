package pocketbase

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/auth/stockoauth"
)

var oauthMountGuard sync.Map // key: app pointer + mount -> bool

// OAuthPlan returns the immutable registration plan computed at startup.
func (a *Adapter) OAuthPlan() pbmcp.OAuthPlan { return a.plan }

// Provider returns the module OAuth provider in module mode, else nil.
func (a *Adapter) Provider() *stockoauth.Provider { return a.provider }

func (a *Adapter) initOAuth() error {
	cfg := a.cfg
	plan, err := pbmcp.BuildOAuthPlan(cfg, "stock", "")
	if err != nil {
		return err
	}
	a.plan = plan
	if !plan.Enabled {
		return nil
	}
	mount := cfg.Path
	if mount == "" {
		mount = "/mcp"
	}
	col, err := a.app.FindCollectionByNameOrId(plan.Collection)
	if err != nil || col == nil {
		return fmt.Errorf("oauth: auth collection %q not found", plan.Collection)
	}
	if col.Name == core.CollectionNameSuperusers {
		return fmt.Errorf("oauth: _superusers cannot be the OAuth auth collection")
	}
	if !col.IsAuth() {
		return fmt.Errorf("oauth: collection %q is not an auth collection", col.Name)
	}
	if !col.PasswordAuth.Enabled {
		return fmt.Errorf("oauth: collection %q does not support password login (passwordAuth is disabled)", col.Name)
	}
	plan.Collection = col.Name
	plan.CollectionID = col.Id
	plan.Mount = mount
	a.plan = plan
	if plan.EffectiveMode == pbmcp.OAuthModeModule {
		prov, err := stockoauth.New(a.app, stockoauth.Config{
			AuthCollection:            col.Id,
			PublicBaseURL:             cfg.PublicBase(),
			MCPPath:                   mount,
			AliasPaths:                cfg.AliasPaths,
			LoginPath:                 cfg.OAuth.LoginPath,
			TokenPath:                 cfg.OAuth.TokenPath,
			UserinfoPath:              cfg.OAuth.UserinfoPath,
			RegisterPath:              cfg.OAuth.RegisterPath,
			TokenDuration:             cfg.OAuth.TokenDuration,
			DisableRegistration:       cfg.OAuth.DisableRegistration,
			RegistrationRedirectHosts: cfg.OAuth.RegistrationRedirectHosts,
		})
		if err != nil {
			return err
		}
		a.provider = prov
	}
	a.app.Logger().Info("MCP OAuth startup report: "+a.plan.Report(), "acceptedCollections", a.cfg.EffectiveAuthCollections(), "manifestRevision", a.cfg.EffectiveManifest().Revision())
	return nil
}

func (a *Adapter) registerOAuthRoutes(r *router.Router[*core.RequestEvent]) error {
	if !a.plan.Enabled {
		return nil
	}
	if a.provider != nil {
		return a.provider.RegisterRoutes(r)
	}
	// External mode: only MCP-facing protected-resource discovery is added.
	handler := func(e *core.RequestEvent) error {
		base := a.cfg.PublicBaseFor(e.Request)
		issuer := a.plan.ExternalIssuer
		if issuer == "" {
			issuer = base
		}
		return e.JSON(http.StatusOK, pbmcp.ProtectedResourceMetadata(base, a.plan.Mount, []string{issuer}))
	}
	r.GET("/.well-known/oauth-protected-resource", handler)
	r.GET(pbmcp.ProtectedResourcePath(a.plan.Mount), handler)
	return nil
}
