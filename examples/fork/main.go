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
	cfg := pbmcp.Config{
		Path:        "/mcp",
		AliasPaths:  []string{"/sse"},
		Auth:        pbmcp.AuthConfig{Collections: []string{"users"}, AllowSuperusers: false},
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
		srv.RegisterRoutes(se.Router)
		return se.Next()
	})
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
