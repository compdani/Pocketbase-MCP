// Stock PocketBase fixture: serves PocketBase with the MCP endpoint at /mcp (alias /sse).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ganigeorgiev.com/pocketbase/pocketbase"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/filesystem"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"

	pbmcp "github.com/compdani/pocketbase-mcp"
	pb "github.com/compdani/pocketbase-mcp/adapters/pocketbase"
	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/anthropic"
	"github.com/compdani/pocketbase-mcp/chat/provider/catalog"
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
		OAuth:       pbmcp.OAuthConfig{Enabled: true, Mode: pbmcp.OAuthModeModule, PublicBaseURL: publicURL},
		Manifest:    loadManifest(),
		AllowWrites: allowWrites,
	}
	cfg.CustomTools = []pbmcp.ToolSpec{pb.ContractPDFTool(app, "contracts", "contract_pdf", 5<<20)}
	// The editor demo tool exists only when proposals are on: it is a chat-only
	// draft patch whose approval authorizes the browser to apply it locally.
	if os.Getenv("MCP_CHAT_PROPOSALS") == "true" {
		cfg.CustomTools = append(cfg.CustomTools, demoEditNoteTool())
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
		// The manifest names items and reports, so a fresh data directory has
		// to contain them before the adapter validates the manifest.
		if err := ensureManifestCollections(se.App); err != nil {
			return err
		}
		// The demo items rule is owner-scoped, and a chat-created item belongs to
		// the caller. The create rule only requires a signed-in user because the
		// record has no owner until this hook runs; every other operation still
		// checks ownership, and the hook always overwrites an explicit owner.
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

// enableChat turns the optional chat surface on. It does nothing unless
// MCP_CHAT_PROVIDER names a supported provider, so an MCP-only installation
// behaves exactly as before.
//
// Optional environment:
//
//	MCP_CHAT_PROVIDER       legacy single provider: openai | anthropic | demo;
//	                        unset leaves chat off (any other value is a startup
//	                        error rather than a silent no-op)
//	MCP_CHAT_API_KEY        legacy provider credential (never logged, not used by demo)
//	MCP_CHAT_BASE_URL       legacy provider API root (for gateways and local proxies)
//	MCP_CHAT_MODEL          legacy model name (required for openai and anthropic;
//	                        demo defaults to "demo-1"); not used in catalog mode
//	MCP_CHAT_PROVIDERS_FILE JSON catalog file for first-boot seeding; mutually
//	                        exclusive with MCP_CHAT_PROVIDER
//	MCP_CHAT_MANAGE         true starts managed mode with no seed; add providers
//	                        through the superuser settings API
//	MCP_CHAT_SYSTEM         system prompt
//	MCP_CHAT_PROPOSALS      true offers write tools on chat and turns every
//	                        model write into a proposal the user approves
//	                        before it runs; it requires MCP_ALLOW_WRITES=true.
//	                        It also registers the chat-only editor demo tool
//	                        edit_note, so the demo can show an editor proposal.
//	MCP_CHAT_AUTO_APPROVE   true lets tools declared Approval "auto" run
//	                        without review (requires proposals)
//	MCP_CHAT_ARG_EDITS      true lets a user edit a pending proposal's
//	                        arguments before approving it (requires proposals)
//	MCP_CHAT_UPLOADS        true turns chat attachments on (upload, download
//	                        and attachment turns)
//	MCP_CHAT_MAX_UPLOAD_BYTES  optional per-file byte limit for attachments
//	MCP_CHAT_TITLE_MODEL    titles new conversations from their first message:
//	                        auto (the turn's model), a model or a
//	                        "provider::model" choice, or off (the default); in
//	                        managed mode the admin setting
//	                        (POST /providers/actions/title) wins
//	MCP_CHAT_COMPACT_MODEL  summarizes a conversation's oldest messages when it
//	                        nears its model's context window, and lets users
//	                        compact a conversation at once: auto (the turn's
//	                        model), a model or "provider::model" choice, or off
//	                        (the default)
//	MCP_CHAT_CONTEXT_WINDOW the context window in tokens of a model the module
//	                        doesn't know (a gateway's, a local model's, or the
//	                        demo's); known Claude and OpenAI models need none.
//	                        A small value (4000) makes the demo show the
//	                        context indicator and compaction after a few messages
//
// With MCP_CHAT_PROVIDER=demo the canned replies also demonstrate v1.2: a
// message containing "create" asks for the first create tool the caller can
// see, a message containing "edit" asks edit_note (pass "rev:<n>" to name a
// base revision), and a message with attachments names them and counts the
// characters of its text files.
//
// It must run before RegisterRoutes, which mounts the chat handler when chat is
// enabled.
func enableChat(srv *pb.Server, allowWrites bool) error {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("MCP_CHAT_PROVIDER")))
	providersFile := strings.TrimSpace(os.Getenv("MCP_CHAT_PROVIDERS_FILE"))
	manage := strings.TrimSpace(os.Getenv("MCP_CHAT_MANAGE")) == "true"
	if name == "" && providersFile == "" && !manage {
		return nil
	}
	if name != "" && (providersFile != "" || manage) {
		return fmt.Errorf("set either MCP_CHAT_PROVIDER or MCP_CHAT_PROVIDERS_FILE/MCP_CHAT_MANAGE, not both")
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
	system := os.Getenv("MCP_CHAT_SYSTEM")
	contextWindow, err := chatContextWindow()
	if err != nil {
		return err
	}
	opts := pbmcp.ChatOptions{
		Namespace:          "example-stock",
		Prefix:             pbmcp.DefaultChatPrefix,
		Proposals:          proposals,
		AutoApprove:        autoApprove,
		AllowArgumentEdits: argEdits,
		Uploads:            uploads,
		MaxUploadBytes:     maxUploadBytes,
	}

	if providersFile != "" || manage {
		for _, key := range []string{"MCP_CHAT_MODEL", "MCP_CHAT_API_KEY", "MCP_CHAT_BASE_URL"} {
			if strings.TrimSpace(os.Getenv(key)) != "" {
				return fmt.Errorf("%s is only for the legacy MCP_CHAT_PROVIDER mode; put providers in MCP_CHAT_PROVIDERS_FILE instead", key)
			}
		}
		var providers []catalog.Provider
		if providersFile != "" {
			providers, err = loadChatProviders(providersFile)
			if err != nil {
				return err
			}
		}
		if err := srv.EnableChat(pb.ChatConfig{
			TitleModel:      os.Getenv("MCP_CHAT_TITLE_MODEL"),
			CompactionModel: os.Getenv("MCP_CHAT_COMPACT_MODEL"),
			RunnerConfig:    chat.RunnerConfig{ContextWindow: contextWindow},
			Options:         opts,
			ManageProviders: true,
			Providers:       providers,
			System:          system,
		}); err != nil {
			return fmt.Errorf("enable chat: %w", err)
		}
		log.Printf("chat enabled in managed provider mode at %s (settings: %s/providers)", pbmcp.DefaultChatPrefix, pbmcp.DefaultChatPrefix)
		if providersFile != "" {
			log.Printf("seeded %d provider(s) from %s on first boot", len(providers), providersFile)
		}
		return nil
	}

	model := strings.TrimSpace(os.Getenv("MCP_CHAT_MODEL"))
	apiKey := os.Getenv("MCP_CHAT_API_KEY")
	baseURL := strings.TrimSpace(os.Getenv("MCP_CHAT_BASE_URL"))

	var prov provider.Provider
	switch name {
	case "demo":
		// Offline, keyless and for local development only. It is reachable only
		// when this variable is set explicitly.
		if model == "" {
			model = "demo-1"
		}
		prov = &demoProvider{delay: 40 * time.Millisecond}
	case "openai":
		if model == "" {
			return fmt.Errorf("MCP_CHAT_MODEL is required when MCP_CHAT_PROVIDER=%q", name)
		}
		built, err := openai.New(openai.Config{BaseURL: baseURL, APIKey: apiKey})
		if err != nil {
			return fmt.Errorf("chat provider: %w", err)
		}
		prov = built
	case "anthropic":
		if model == "" {
			return fmt.Errorf("MCP_CHAT_MODEL is required when MCP_CHAT_PROVIDER=%q", name)
		}
		built, err := anthropic.New(anthropic.Config{BaseURL: baseURL, APIKey: apiKey})
		if err != nil {
			return fmt.Errorf("chat provider: %w", err)
		}
		prov = built
	default:
		return fmt.Errorf("MCP_CHAT_PROVIDER %q is not supported: use openai, anthropic or demo", name)
	}

	if err := srv.EnableChat(pb.ChatConfig{
		TitleModel:      os.Getenv("MCP_CHAT_TITLE_MODEL"),
		CompactionModel: os.Getenv("MCP_CHAT_COMPACT_MODEL"),
		RunnerConfig:    chat.RunnerConfig{ContextWindow: contextWindow},
		Options:         opts,
		Provider:        prov,
		Model:           model,
		System:          system,
	}); err != nil {
		return fmt.Errorf("enable chat: %w", err)
	}
	if proposals {
		log.Printf("chat proposals are enabled: write tools are offered for review and run only after approval")
	}
	if uploads {
		log.Printf("chat attachments are enabled: uploads are stored in the app's filesystem and served only to their owner")
	}
	if name == "demo" {
		log.Printf("chat enabled with the OFFLINE DEMO provider (no API key, no network; local development only) and model %s at %s", model, pbmcp.DefaultChatPrefix)
	} else {
		log.Printf("chat enabled with provider %s and model %s at %s", name, model, pbmcp.DefaultChatPrefix)
	}
	return nil
}

// loadChatProviders reads a non-secret (or 0600) provider file for first-boot
// seeding. The file is intentionally not an env-var list: it can be checked in
// without keys, and the admin API handles later changes.
func loadChatProviders(path string) ([]catalog.Provider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var file struct {
		Providers []struct {
			ID           string `json:"id"`
			Label        string `json:"label"`
			Kind         string `json:"kind"`
			Enabled      *bool  `json:"enabled"`
			BaseURL      string `json:"baseURL"`
			OAuthBaseURL string `json:"oauthBaseURL"`
			WireAPI      string `json:"wireAPI"`
			Models       []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"models"`
			DefaultModel string            `json:"defaultModel"`
			Headers      map[string]string `json:"headers"`
			ExtraBody    map[string]any    `json:"extraBody"`
			APIKey       string            `json:"apiKey"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make([]catalog.Provider, 0, len(file.Providers))
	hasKey := false
	for _, item := range file.Providers {
		enabled := true
		if item.Enabled != nil {
			enabled = *item.Enabled
		}
		models := make([]catalog.Model, 0, len(item.Models))
		for _, m := range item.Models {
			models = append(models, catalog.Model{ID: m.ID, Label: m.Label})
		}
		p := catalog.Provider{
			ID:           item.ID,
			Label:        item.Label,
			Kind:         catalog.Kind(item.Kind),
			Enabled:      enabled,
			BaseURL:      item.BaseURL,
			OAuthBaseURL: item.OAuthBaseURL,
			WireAPI:      item.WireAPI,
			Models:       models,
			DefaultModel: item.DefaultModel,
			Headers:      item.Headers,
			ExtraBody:    item.ExtraBody,
			APIKey:       item.APIKey,
		}.Normalized()
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if p.APIKey != "" {
			hasKey = true
		}
		out = append(out, p)
	}
	if hasKey {
		log.Printf("warning: %s contains API keys; keep it out of version control and chmod 600", path)
	}
	return out, nil
}

// chatMaxUploadBytes reads the optional MCP_CHAT_MAX_UPLOAD_BYTES limit. An
// unset or zero value leaves the chat default in place.
// chatContextWindow reads MCP_CHAT_CONTEXT_WINDOW.
func chatContextWindow() (int, error) {
	raw := strings.TrimSpace(os.Getenv("MCP_CHAT_CONTEXT_WINDOW"))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 2000 {
		return 0, fmt.Errorf("MCP_CHAT_CONTEXT_WINDOW must be a token count of at least 2000")
	}
	return n, nil
}

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

// demoProvider is a keyless, network-free provider for local development. It is
// used only when MCP_CHAT_PROVIDER=demo and is never a substitute for a real
// model: its replies are canned.
//
// Behaviour:
//   - a user message with attachments names them (and counts the characters of
//     its text files);
//   - a user message containing "edit" asks the chat-only editor tool
//     edit_note, naming the "rev:<n>" the user typed (or "0") and the remaining
//     text as the new note;
//   - a user message containing "create" asks the first create tool the caller
//     can see, with the text after the word as the title;
//   - a user message containing "search" asks for a read tool whose name starts
//     with "search_" (preferring one whose name matches a word in the message),
//     with {"limit": 5} when that tool accepts a limit;
//   - the step after a tool result summarises how many results it carried, or
//     says that a change is waiting for approval;
//   - a user message containing "long" gets a long reply, so a small
//     MCP_CHAT_CONTEXT_WINDOW fills up after a few messages;
//   - a summary request (chat.CompactionSystemPrompt) gets a summary listing
//     the user's earlier messages;
//   - any other message streams a short reply in a few text chunks.
type demoProvider struct {
	delay time.Duration
}

var _ provider.Provider = (*demoProvider)(nil)
var _ provider.AttachmentSupport = (*demoProvider)(nil)

// SupportsAttachment implements provider.AttachmentSupport for every MIME type
// the chat surface accepts, so a demo deployment advertises and accepts the
// same attachments a real provider can be given.
func (p *demoProvider) SupportsAttachment(mime string) bool {
	return provider.SupportsAttachment(mime)
}

// Name implements provider.Provider.
func (p *demoProvider) Name() string { return "demo" }

// Stream implements provider.Provider.
func (p *demoProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	chunks := demoChunks(req)
	delay := p.delay
	ch := make(chan provider.Chunk)
	go func() {
		defer close(ch)
		for _, chunk := range chunks {
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
			select {
			case ch <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// demoChunks is the whole scripted behaviour, kept pure so it is easy to read.
func demoChunks(req provider.Request) []provider.Chunk {
	var last provider.Message
	if n := len(req.Messages); n > 0 {
		last = req.Messages[n-1]
	}
	if req.System == chat.TitleSystemPrompt {
		return demoTextChunks(demoConversationTitle(last.Text))
	}
	if req.System == chat.CompactionSystemPrompt {
		return demoTextChunks(demoSummary(last.Text))
	}
	lower := strings.ToLower(last.Text)
	switch {
	case last.Role == provider.RoleTool:
		return demoTextChunks(demoToolSummary(last))
	case last.Role == provider.RoleUser && len(last.Attachments) > 0:
		return demoTextChunks(demoAttachmentReply(last.Attachments))
	case last.Role == provider.RoleUser && strings.Contains(lower, "edit"):
		if call := demoEditCall(req, last.Text); call != nil {
			return []provider.Chunk{
				{Kind: provider.ChunkToolCall, ToolCall: call},
				{Kind: provider.ChunkDone},
			}
		}
		return demoTextChunks("The demo provider would edit the note here, but edit_note is not available. Turn on MCP_CHAT_PROPOSALS=true (with MCP_ALLOW_WRITES=true) to review that change.")
	case last.Role == provider.RoleUser && strings.Contains(lower, "create"):
		if call := demoCreateCall(req, last.Text); call != nil {
			return []provider.Chunk{
				{Kind: provider.ChunkToolCall, ToolCall: call},
				{Kind: provider.ChunkDone},
			}
		}
		return demoTextChunks("The demo provider would create a record here, but no create tool is available. Turn on MCP_CHAT_PROPOSALS=true (with MCP_ALLOW_WRITES=true) to review that change.")
	case last.Role == provider.RoleUser && strings.Contains(lower, "search"):
		if call := demoSearchCall(req, last.Text); call != nil {
			return []provider.Chunk{
				{Kind: provider.ChunkToolCall, ToolCall: call},
				{Kind: provider.ChunkDone},
			}
		}
		return demoTextChunks("The demo provider would search here, but no search tool is available for this account.")
	case last.Role == provider.RoleUser && strings.Contains(lower, "long"):
		return demoTextChunks(demoLongReply())
	default:
		return demoTextChunks("Hello from the offline demo provider. This reply is canned: MCP_CHAT_PROVIDER=demo is for local development only.")
	}
}

// demoLongReply is about 1,500 tokens of filler, for trying the context
// indicator with a small MCP_CHAT_CONTEXT_WINDOW.
func demoLongReply() string {
	var b strings.Builder
	b.WriteString("Here is a deliberately long demo reply, so the conversation's context fills up quickly.\n\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "%d. The offline demo provider repeats this sentence to take up room in the model's context window.\n", i)
	}
	return b.String()
}

// demoSummary answers a summary request the way a model would, in short: it
// lists what the user asked, from the "User:" lines of the request.
func demoSummary(input string) string {
	var asked []string
	for _, line := range strings.Split(input, "\n") {
		if text, ok := strings.CutPrefix(line, "User: "); ok {
			if len(text) > 60 {
				text = text[:60] + "…"
			}
			asked = append(asked, text)
		}
	}
	if len(asked) == 0 {
		return "The conversation so far had no user messages worth keeping."
	}
	return "Demo summary of the earlier conversation. The user asked: " + strings.Join(asked, "; ") + "."
}

// demoConversationTitle titles a conversation from the first words of its message, as a
// real model would summarise it.
func demoConversationTitle(text string) string {
	words := strings.Fields(text)
	if len(words) > 5 {
		words = words[:5]
	}
	if len(words) == 0 {
		return "Demo chat"
	}
	title := strings.Join(words, " ")
	return strings.ToUpper(title[:1]) + title[1:]
}

// demoAttachmentReply names every attachment and counts the characters of the
// text ones, which is the one thing a model can do with a file it cannot see.
func demoAttachmentReply(attachments []provider.Attachment) string {
	names := make([]string, 0, len(attachments))
	var textNames []string
	textChars := 0
	for _, a := range attachments {
		names = append(names, a.Name)
		if kind, ok := provider.AttachmentKind(a.MIME); ok && kind == provider.AttachmentText {
			textNames = append(textNames, a.Name)
			textChars += utf8.RuneCount(a.Data)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "I received %d attachment(s): %s.", len(attachments), strings.Join(names, ", "))
	if len(textNames) > 0 {
		fmt.Fprintf(&b, " The text attachment(s) %s contain %d character(s).", strings.Join(textNames, ", "), textChars)
	}
	return b.String()
}

// demoEditCall builds the edit_note call from the user's text. The tool is only
// listed when proposals are on, so its absence is the "not enabled" answer.
func demoEditCall(req provider.Request, text string) *provider.ToolCall {
	var chosen *provider.ToolDef
	for i := range req.Tools {
		if req.Tools[i].Name == "edit_note" {
			chosen = &req.Tools[i]
			break
		}
	}
	if chosen == nil {
		return nil
	}
	revision, body := demoRevisionAndText(text)
	raw, err := json.Marshal(map[string]any{
		"editorId":     "demo-note",
		"baseRevision": revision,
		"text":         body,
	})
	if err != nil {
		return nil
	}
	return &provider.ToolCall{ID: "demo-edit-1", Name: chosen.Name, Arguments: raw}
}

// demoRevisionAndText splits an "edit rev:<n> <text>" message into the base
// revision the user named and the note text, defaulting to "0" and the canned
// sentence. The revision is whatever the user typed because the provider cannot
// know the browser editor's real revision.
func demoRevisionAndText(text string) (revision, body string) {
	revision = "0"
	rest := text
	if idx := strings.Index(strings.ToLower(rest), "rev:"); idx >= 0 {
		after := rest[idx+len("rev:"):]
		end := strings.IndexAny(after, " \t\r\n")
		if end < 0 {
			end = len(after)
		}
		if v := strings.TrimSpace(after[:end]); v != "" {
			revision = v
		}
		rest = strings.TrimSpace(rest[:idx] + " " + after[end:])
	}
	if idx := strings.Index(strings.ToLower(rest), "edit"); idx >= 0 {
		rest = strings.TrimSpace(rest[:idx] + rest[idx+len("edit"):])
	}
	rest = strings.TrimLeft(rest, " \t:,-")
	if rest == "" {
		rest = "Edited by the demo assistant"
	}
	return revision, rest
}

// demoCreateCall asks the first create tool the caller can see for one record.
// A per-collection tool takes {"data":{...}}; the generic tool also names its
// collection, taken from the enum the caller's listing carries.
func demoCreateCall(req provider.Request, text string) *provider.ToolCall {
	title := demoTitle(text)
	for i := range req.Tools {
		tool := &req.Tools[i]
		switch {
		case tool.Name == "create":
			collection := demoFirstCollection(tool)
			if collection == "" {
				continue
			}
			raw, err := json.Marshal(map[string]any{
				"collection": collection,
				"data":       map[string]any{"title": title},
			})
			if err != nil {
				continue
			}
			return &provider.ToolCall{ID: "demo-create-1", Name: tool.Name, Arguments: raw}
		case strings.HasPrefix(tool.Name, "create_"):
			raw, err := json.Marshal(map[string]any{
				"data": map[string]any{"title": title},
			})
			if err != nil {
				continue
			}
			return &provider.ToolCall{ID: "demo-create-1", Name: tool.Name, Arguments: raw}
		}
	}
	return nil
}

// demoFirstCollection reads the first collection name from a generic create
// tool's enum. The same schema is compiled from a decoded map at runtime, so the
// enum may be []string or []any.
func demoFirstCollection(tool *provider.ToolDef) string {
	props, _ := tool.InputSchema["properties"].(map[string]any)
	collection, _ := props["collection"].(map[string]any)
	switch enum := collection["enum"].(type) {
	case []string:
		if len(enum) > 0 {
			return enum[0]
		}
	case []any:
		for _, v := range enum {
			if name, ok := v.(string); ok && name != "" {
				return name
			}
		}
	}
	return ""
}

// demoTitle is the text after the word "create", or a canned title.
func demoTitle(text string) string {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, "create")
	if idx < 0 {
		return "Demo item"
	}
	rest := strings.TrimLeft(strings.TrimSpace(text[idx+len("create"):]), " \t:,-")
	if rest == "" {
		return "Demo item"
	}
	return rest
}

// demoSearchCall picks the search tool for a message and builds its arguments.
func demoSearchCall(req provider.Request, text string) *provider.ToolCall {
	lower := strings.ToLower(text)
	var chosen *provider.ToolDef
	for i := range req.Tools {
		tool := &req.Tools[i]
		if !strings.HasPrefix(tool.Name, "search_") {
			continue
		}
		if chosen == nil {
			chosen = tool
		}
		suffix := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(tool.Name, "search_"), "_", " "))
		if suffix != "" && strings.Contains(lower, suffix) {
			chosen = tool
			break
		}
	}
	if chosen == nil {
		return nil
	}
	args := map[string]any{}
	if props, ok := chosen.InputSchema["properties"].(map[string]any); ok {
		if _, ok := props["limit"]; ok {
			args["limit"] = 5
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		raw = []byte("{}")
	}
	return &provider.ToolCall{ID: "demo-call-1", Name: chosen.Name, Arguments: raw}
}

// demoToolSummary reports what a tool result means: a proposed change is
// awaiting approval (never applied), a search carries results, and anything else
// is unsupported by the demo.
func demoToolSummary(message provider.Message) string {
	if message.ToolIsError {
		return "The demo tool call failed, so there is nothing to summarise. This reply is canned; MCP_CHAT_PROVIDER=demo is for local development only."
	}
	var pending struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(message.ToolResult, &pending); err == nil && pending.Status == "pending_review" {
		summary := pending.Summary
		if summary == "" {
			summary = "the change"
		}
		return fmt.Sprintf("The change is waiting for your approval: %s. Nothing has been applied yet. This reply is canned; MCP_CHAT_PROVIDER=demo is for local development only.", summary)
	}
	var payload struct {
		Results    []json.RawMessage `json:"results"`
		TotalItems int               `json:"totalItems"`
	}
	if err := json.Unmarshal(message.ToolResult, &payload); err != nil {
		return "The demo tool returned a result the demo cannot summarise. This reply is canned; MCP_CHAT_PROVIDER=demo is for local development only."
	}
	if payload.TotalItems > len(payload.Results) {
		return fmt.Sprintf("The demo search returned %d item(s) on this page out of %d in total. This reply is canned; MCP_CHAT_PROVIDER=demo is for local development only.", len(payload.Results), payload.TotalItems)
	}
	return fmt.Sprintf("The demo search returned %d item(s). This reply is canned; MCP_CHAT_PROVIDER=demo is for local development only.", len(payload.Results))
}

// demoTextChunks splits a reply into a few text chunks, then ends the step.
func demoTextChunks(text string) []provider.Chunk {
	const parts = 3
	words := strings.Fields(text)
	if len(words) == 0 {
		return []provider.Chunk{{Kind: provider.ChunkDone}}
	}
	per := (len(words) + parts - 1) / parts
	chunks := make([]provider.Chunk, 0, parts+1)
	for i := 0; i < len(words); i += per {
		end := i + per
		if end > len(words) {
			end = len(words)
		}
		piece := strings.Join(words[i:end], " ")
		if end < len(words) {
			piece += " "
		}
		chunks = append(chunks, provider.Chunk{Kind: provider.ChunkText, Text: piece})
	}
	return append(chunks, provider.Chunk{Kind: provider.ChunkDone})
}

// demoEditNoteTool is the chat-only editor demo tool. It has no handler: a
// model call becomes an editor proposal, and approval only authorizes the
// browser to apply the draft patch locally. Saving stays in the app's own
// protected API, so this tool can never claim one.
func demoEditNoteTool() pbmcp.ToolSpec {
	return pbmcp.ToolSpec{
		Name:              "edit_note",
		Title:             "Edit note",
		Description:       "Replaces the note text in the page's editor. Approval authorizes a local draft patch; this tool writes nothing on the server.",
		Channels:          "chat",
		ExecutionLocation: "editor",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"editorId":     map[string]any{"type": "string", "description": "The editor/document to edit."},
				"baseRevision": map[string]any{"type": "string", "description": "The editor revision the text was computed against."},
				"text":         map[string]any{"type": "string", "description": "The full replacement text for the note."},
			},
			"required":             []string{"editorId", "baseRevision", "text"},
			"additionalProperties": false,
		},
	}
}

// ensureManifestCollections creates the demo collections the bundled manifest
// names. Without them a fresh data directory fails manifest validation at
// startup; they are never touched again once they exist (idempotent), and an
// operator-supplied schema is adopted as-is.
func ensureManifestCollections(app core.App) error {
	if err := ensureItems(app); err != nil {
		return err
	}
	return ensureReports(app)
}

// ensureItems creates the owner-scoped items collection the manifest describes:
// title, status, price and an owner relation to users.
//
// The create rule only requires a signed-in caller because a new record has no
// owner yet: the OnRecordCreateRequest hook in main assigns it from the caller
// before the save. List, view, update and delete stay owner-scoped, and the hook
// always overwrites any client-supplied owner.
func ensureItems(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("items"); err == nil {
		return nil
	}
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}
	c := core.NewBaseCollection("items")
	ownerRule := types.Pointer(`@request.auth.id != "" && owner = @request.auth.id`)
	createRule := types.Pointer(`@request.auth.id != ""`)
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = ownerRule, ownerRule, createRule, ownerRule, ownerRule
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
