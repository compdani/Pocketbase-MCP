package pbpostgress

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	pbmcp "github.com/compdani/pocketbase-mcp"
)

var oauthMountGuard sync.Map // key: app pointer + mount -> bool

// OAuthPlan returns the immutable registration plan computed at startup.
func (a *Adapter) OAuthPlan() pbmcp.OAuthPlan { return a.plan }

func (a *Adapter) initOAuth() error {
	cfg := a.cfg
	plan, err := pbmcp.BuildOAuthPlan(cfg, "fork", "")
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
	plan.Collection = col.Name
	plan.CollectionID = col.Id
	plan.Mount = mount
	if plan.EffectiveMode == pbmcp.OAuthModeNative {
		if !col.OAuth2Server.Enabled {
			return fmt.Errorf("oauth: native mode requires the OAuth2Server config of auth collection %q to be enabled, but it is disabled", col.Name)
		}
	}
	a.plan = plan
	a.app.Logger().Info("MCP OAuth startup report: "+a.plan.Report(), "acceptedCollections", a.cfg.EffectiveAuthCollections(), "manifestRevision", a.cfg.EffectiveManifest().Revision())
	return nil
}

// registerOAuthRoutes in native/external mode adds ONLY MCP-facing
// protected-resource discovery. It adds zero authorize/token/userinfo/
// login/logout handlers, code collections, cleanup jobs or OAuth hooks;
// the fork owns those (milestone 4 adds fork-side root discovery).
func (a *Adapter) registerOAuthRoutes(r *router.Router[*core.RequestEvent]) {
	if !a.plan.Enabled {
		return
	}
	handler := func(e *core.RequestEvent) error {
		base := a.cfg.PublicBaseFor(e.Request)
		issuer := a.plan.ExternalIssuer
		if issuer == "" {
			if a.plan.EffectiveMode == pbmcp.OAuthModeNative {
				// sign in on the configured app when several apps share the database
				nativeBase := strings.TrimRight(a.cfg.OAuth.IssuerBaseURL, "/")
				if nativeBase == "" {
					if col, err := a.app.FindCachedCollectionByNameOrId(a.plan.Collection); err == nil {
						nativeBase = col.OAuth2Server.IssuerBase()
					}
				}
				if nativeBase == "" {
					nativeBase = strings.TrimRight(a.app.Settings().Meta.AppURL, "/")
				}
				if nativeBase == "" {
					nativeBase = strings.TrimRight(base, "/")
				}
				issuer = nativeBase + "/api/collections/" + a.plan.Collection + "/oauth2-server"
			} else {
				issuer = base
			}
		}
		return e.JSON(http.StatusOK, pbmcp.ProtectedResourceMetadata(base, a.plan.Mount, []string{issuer}))
	}
	r.GET("/.well-known/oauth-protected-resource", handler)
	r.GET(pbmcp.ProtectedResourcePath(a.plan.Mount), handler)
}
