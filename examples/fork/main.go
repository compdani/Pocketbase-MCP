// Fork (pb-postgress) fixture: serves PocketBase with the MCP endpoint at /mcp (alias /sse).
package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	pbmcp "github.com/compdani/pocketbase-mcp"
	pb "github.com/compdani/pocketbase-mcp/adapters/pbpostgress"
)

func loadManifest() *pbmcp.Manifest {
	candidates := []string{}
	if v := os.Getenv("MCP_MANIFEST"); v != "" {
		candidates = append(candidates, v)
	}
	candidates = append(candidates, "manifest.json")
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "manifest.json"))
	}
	for _, c := range candidates {
		m, err := pbmcp.LoadManifestFile(c)
		if err == nil {
			mm := m
			return &mm
		}
		if _, statErr := os.Stat(c); statErr == nil {
			log.Fatalf("manifest %s: %v", c, err)
		}
	}
	return nil
}

func main() {
	app := pocketbase.New()
	publicURL := os.Getenv("MCP_PUBLIC_URL")
	cfg := pbmcp.Config{
		Path:        "/mcp",
		AliasPaths:  []string{"/sse"},
		PublicURL:   publicURL,
		Auth:        pbmcp.AuthConfig{Collection: "users", AllowSuperusers: false},
		OAuth:       pbmcp.OAuthConfig{Enabled: true, Mode: pbmcp.OAuthModeNative, PublicBaseURL: publicURL},
		Manifest:    loadManifest(),
		AllowWrites: os.Getenv("MCP_ALLOW_WRITES") == "true",
	}
	for _, name := range strings.Split(os.Getenv("MCP_COLLECTIONS"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cfg.Collections = append(cfg.Collections, pbmcp.CollectionPreset{
			Name: name, TitleFields: []string{"title", "name"},
			SummaryFields: []string{"title", "name", "status"},
		})
	}
	if len(cfg.Collections) == 0 {
		cfg.Collections = append(cfg.Collections, pbmcp.CollectionPreset{
			Name: "items", TitleFields: []string{"title"}, SummaryFields: []string{"title", "status"},
		})
	}
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		// Enable the fork's native OAuth2Server on the users collection
		// (fixture setup; the MCP module itself adds no OAuth storage).
		if os.Getenv("MCP_SEED_NATIVE_OAUTH") != "false" {
			users, err := se.App.FindCollectionByNameOrId("users")
			if err != nil {
				return err
			}
			if !users.OAuth2Server.Enabled {
				redirect := os.Getenv("MCP_OAUTH_REDIRECT_URI")
				if redirect == "" {
					redirect = "https://example.com/callback"
				}
				secret := os.Getenv("MCP_OAUTH_CLIENT_SECRET")
				if secret == "" {
					secret = "dev-oauth-secret-0123456789abcdef"
				}
				users.OAuth2Server = core.OAuth2ServerConfig{
					Enabled: true, CodeDuration: 600,
					Clients: []core.OAuth2ServerClient{{Id: "chatgpt-dev", Name: "ChatGPT", Secret: secret, RedirectURIs: []string{redirect}}},
				}
				if err := se.App.Save(users); err != nil {
					return err
				}
				log.Printf("enabled native OAuth2Server on users (client_id=chatgpt-dev redirect_uri=%s)", redirect)
			}
		}
		srv, err := pb.New(se.App, cfg)
		if err != nil {
			return err
		}
		srv.RegisterRoutes(se.Router)
		return se.Next()
	})
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
