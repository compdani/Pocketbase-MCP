// Fork (pb-postgress) fixture: serves PocketBase with the MCP endpoint at /mcp (alias /sse).
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/types"

	pbmcp "github.com/compdani/pocketbase-mcp"
	pb "github.com/compdani/pocketbase-mcp/adapters/pbpostgress"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/anthropic"
	"github.com/compdani/pocketbase-mcp/chat/provider/openai"
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
	allowWrites := os.Getenv("MCP_ALLOW_WRITES") == "true"
	cfg := pbmcp.Config{
		Path:        "/mcp",
		AliasPaths:  []string{"/sse"},
		PublicURL:   publicURL,
		Auth:        pbmcp.AuthConfig{Collection: "users", AllowSuperusers: false},
		OAuth:       pbmcp.OAuthConfig{Enabled: true, Mode: pbmcp.OAuthModeNative, PublicBaseURL: publicURL},
		Manifest:    loadManifest(),
		AllowWrites: allowWrites,
	}
	cfg.CustomTools = []pbmcp.ToolSpec{pb.ContractPDFTool(app, "contracts", "contract_pdf", 5<<20)}
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
					Registration: core.OAuth2ServerRegistrationConfig{Enabled: true, RateLimit: 10},
					Clients:      []core.OAuth2ServerClient{{Id: "chatgpt-dev", Name: "ChatGPT", Secret: secret, RedirectURIs: []string{redirect}}},
				}
				if err := se.App.Save(users); err != nil {
					return err
				}
				log.Printf("enabled native OAuth2Server on users (client_id=chatgpt-dev redirect_uri=%s)", redirect)
			} else if !users.OAuth2Server.Registration.Enabled {
				users.OAuth2Server.Registration.Enabled = true
				users.OAuth2Server.Registration.RateLimit = 10
				if err := se.App.Save(users); err != nil {
					return err
				}
			}
		}
		if err := ensureManifestCollections(se.App); err != nil {
			return err
		}
		// Same owner handling as the stock example: create only needs a signed-in
		// user, and this hook makes the caller the owner (overwriting any value
		// sent), so a chat or MCP create can pass the owner-scoped rules.
		se.App.OnRecordCreateRequest("items").BindFunc(func(e *core.RecordRequestEvent) error {
			if e.Auth != nil && e.Auth.Collection().Name == "users" {
				e.Record.Set("owner", e.Auth.Id)
			}
			return e.Next()
		})
		if err := ensureContracts(se.App); err != nil {
			return err
		}
		if os.Getenv("MCP_SEED_DEMO") == "true" {
			if err := seedDemoContract(se.App); err != nil {
				return err
			}
		}
		srv, err := pb.New(se.App, cfg)
		if err != nil {
			return err
		}
		if err := enableChat(srv, allowWrites); err != nil {
			return err
		}
		if err := srv.RegisterRoutes(se.Router); err != nil {
			return err
		}
		return se.Next()
	})
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// enableChat turns the optional chat surface on. It does nothing unless
// MCP_CHAT_PROVIDER names a supported provider, so an MCP-only installation
// behaves exactly as before.
//
// Optional environment:
//
//	MCP_CHAT_PROVIDER      openai | anthropic; unset leaves chat off (any other
//	                       value is a startup error rather than a silent no-op)
//	MCP_CHAT_API_KEY       provider credential (never logged)
//	MCP_CHAT_BASE_URL      provider API root (for gateways and local proxies)
//	MCP_CHAT_MODEL         model name (required when chat is enabled)
//	MCP_CHAT_SYSTEM        system prompt
//	MCP_CHAT_PROPOSALS     true offers write tools on chat and turns every
//	                       model write into a proposal the user approves
//	                       before it runs; it requires MCP_ALLOW_WRITES=true
//	MCP_CHAT_AUTO_APPROVE  true lets tools declared Approval "auto" run
//	                       without review (requires proposals)
//	MCP_CHAT_ARG_EDITS     true lets a user edit a pending proposal's
//	                       arguments before approving it (requires proposals)
//	MCP_CHAT_UPLOADS       true turns chat attachments on (upload, download
//	                       and attachment turns)
//	MCP_CHAT_MAX_UPLOAD_BYTES  optional per-file byte limit for attachments
//
// It must run before RegisterRoutes, which mounts the chat handler when chat is
// enabled. The chat collections live in the shared database and are scoped to
// the "example-fork" namespace, so restart recovery only interrupts this
// example's own turns and reconciles only its own proposals.
func enableChat(srv *pb.Server, allowWrites bool) error {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("MCP_CHAT_PROVIDER")))
	if name == "" {
		return nil
	}
	proposals := os.Getenv("MCP_CHAT_PROPOSALS") == "true"
	autoApprove := os.Getenv("MCP_CHAT_AUTO_APPROVE") == "true"
	argEdits := os.Getenv("MCP_CHAT_ARG_EDITS") == "true"
	uploads := os.Getenv("MCP_CHAT_UPLOADS") == "true"
	maxUploadBytes, err := chatMaxUploadBytes()
	if err != nil {
		return err
	}
	if proposals && !allowWrites {
		return fmt.Errorf("MCP_CHAT_PROPOSALS=true requires MCP_ALLOW_WRITES=true: a chat write is a server write and must be reviewed")
	}
	if !proposals && (autoApprove || argEdits) {
		return fmt.Errorf("MCP_CHAT_AUTO_APPROVE and MCP_CHAT_ARG_EDITS require MCP_CHAT_PROPOSALS=true")
	}
	model := strings.TrimSpace(os.Getenv("MCP_CHAT_MODEL"))
	if model == "" {
		return fmt.Errorf("MCP_CHAT_MODEL is required when MCP_CHAT_PROVIDER=%q", name)
	}
	apiKey := os.Getenv("MCP_CHAT_API_KEY")
	baseURL := strings.TrimSpace(os.Getenv("MCP_CHAT_BASE_URL"))
	system := os.Getenv("MCP_CHAT_SYSTEM")

	var prov provider.Provider
	switch name {
	case "openai":
		built, err := openai.New(openai.Config{BaseURL: baseURL, APIKey: apiKey})
		if err != nil {
			return fmt.Errorf("chat provider: %w", err)
		}
		prov = built
	case "anthropic":
		built, err := anthropic.New(anthropic.Config{BaseURL: baseURL, APIKey: apiKey})
		if err != nil {
			return fmt.Errorf("chat provider: %w", err)
		}
		prov = built
	default:
		return fmt.Errorf("MCP_CHAT_PROVIDER %q is not supported: use openai or anthropic", name)
	}

	if err := srv.EnableChat(pb.ChatConfig{
		Options: pbmcp.ChatOptions{
			Namespace:          "example-fork",
			Prefix:             pbmcp.DefaultChatPrefix,
			Proposals:          proposals,
			AutoApprove:        autoApprove,
			AllowArgumentEdits: argEdits,
			Uploads:            uploads,
			MaxUploadBytes:     maxUploadBytes,
		},
		Provider: prov,
		Model:    model,
		System:   system,
	}); err != nil {
		return fmt.Errorf("enable chat: %w", err)
	}
	if proposals {
		log.Printf("chat proposals are enabled: write tools are offered for review and run only after approval")
	}
	if uploads {
		log.Printf("chat attachments are enabled: uploads are stored in the app's filesystem and served only to their owner")
	}
	log.Printf("chat enabled with provider %s and model %s at %s", name, model, pbmcp.DefaultChatPrefix)
	return nil
}

// chatMaxUploadBytes reads the optional MCP_CHAT_MAX_UPLOAD_BYTES limit. An
// unset or zero value leaves the chat default in place.
func chatMaxUploadBytes() (int64, error) {
	raw := strings.TrimSpace(os.Getenv("MCP_CHAT_MAX_UPLOAD_BYTES"))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("MCP_CHAT_MAX_UPLOAD_BYTES must be a non-negative byte count")
	}
	return n, nil
}

// ensureManifestCollections creates the demo collections the bundled manifest
// names. Without them a fresh database fails manifest validation at startup;
// they are never touched again once they exist (idempotent), and an
// operator-supplied schema is adopted as-is.
func ensureManifestCollections(app core.App) error {
	if err := ensureItems(app); err != nil {
		return err
	}
	return ensureReports(app)
}

// ensureItems creates the owner-scoped items collection the manifest describes:
// title, status, price and an owner relation to users.
func ensureItems(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("items"); err == nil {
		return nil
	}
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}
	c := core.NewBaseCollection("items")
	rule := types.Pointer(`@request.auth.id != "" && owner = @request.auth.id`)
	createRule := types.Pointer(`@request.auth.id != ""`)
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = rule, rule, createRule, rule, rule
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "status"},
		&core.NumberField{Name: "price"},
		&core.RelationField{Name: "owner", CollectionId: users.Id, MaxSelect: 1},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	return app.Save(c)
}

// ensureReports creates the reports collection the manifest describes. Its
// field:role:admin policy is enforced by the MCP module, so the record rules
// only have to require an authenticated caller.
func ensureReports(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("reports"); err == nil {
		return nil
	}
	c := core.NewBaseCollection("reports")
	rule := types.Pointer(`@request.auth.id != ""`)
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = rule, rule, rule, rule, rule
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	return app.Save(c)
}

// ensureContracts seeds the contracts collection used by the
// fetch_contract_pdf reference tool (idempotent).
func ensureContracts(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("contracts"); err == nil {
		return nil
	}
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}
	c := core.NewBaseCollection("contracts")
	rule := types.Pointer("owner = @request.auth.id")
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = rule, rule, rule, rule, rule
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.FileField{Name: "contract_pdf", MaxSelect: 1, Protected: true},
		&core.RelationField{Name: "owner", CollectionId: users.Id, MaxSelect: 1},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	return app.Save(c)
}

// seedDemoContract creates demo@example.com (password123) with one
// contract PDF when MCP_SEED_DEMO=true. Fixture/verification use only.
func seedDemoContract(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}
	user, err := app.FindAuthRecordByEmail("users", "demo@example.com")
	if err != nil {
		user = core.NewRecord(users)
		user.SetEmail("demo@example.com")
		user.SetPassword("password123")
		if err := app.Save(user); err != nil {
			return err
		}
	}
	existing, err := app.FindRecordsByFilter("contracts", "owner = '"+user.Id+"'", "", 1, 0)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	contracts, err := app.FindCollectionByNameOrId("contracts")
	if err != nil {
		return err
	}
	rec := core.NewRecord(contracts)
	rec.Set("title", "Demo contract")
	rec.Set("owner", user.Id)
	file, err := filesystem.NewFileFromBytes([]byte("%PDF-1.4\n% demo contract\n"), "contract.pdf")
	if err != nil {
		return err
	}
	rec.Set("contract_pdf", file)
	return app.Save(rec)
}
