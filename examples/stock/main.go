// Stock PocketBase fixture: serves PocketBase with the MCP endpoint at /mcp (alias /sse).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	pbmcp "github.com/compdani/pocketbase-mcp"
	pb "github.com/compdani/pocketbase-mcp/adapters/pocketbase"
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
		OAuth:       pbmcp.OAuthConfig{Enabled: true, Mode: pbmcp.OAuthModeModule, PublicBaseURL: publicURL},
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
		srv, err := pb.New(se.App, cfg)
		if err != nil {
			return err
		}
		if err := srv.RegisterRoutes(se.Router); err != nil {
			return err
		}
		// Seed a hand-created confidential client (ChatGPT baseline). The
		// secret is printed once, only when the client is first created.
		if os.Getenv("MCP_SEED_OAUTH_CLIENT") != "false" {
			if prov := srv.Adapter.Provider(); prov != nil {
				name := "ChatGPT"
				if _, err := se.App.FindFirstRecordByData(prov.Config().ClientsCollection(), "name", name); err != nil {
					redirect := os.Getenv("MCP_OAUTH_REDIRECT_URI")
					if redirect == "" {
						redirect = "https://example.com/callback"
					}
					id, secret, err := prov.CreateHandClient(name, []string{redirect})
					if err != nil {
						return err
					}
					credentials, _ := json.Marshal(map[string]string{"client_id": id, "client_secret": secret, "redirect_uri": redirect})
					path := filepath.Join(se.App.DataDir(), "mcp-client.json")
					if err := os.WriteFile(path, credentials, 0600); err != nil {
						return fmt.Errorf("write seed credentials: %w", err)
					}
					log.Printf("seeded OAuth client; credentials saved to %s (protect and remove after setup)", path)
				}
			}
		}
		return se.Next()
	})
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
