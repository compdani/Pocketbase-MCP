package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/anthropic"
	"github.com/compdani/pocketbase-mcp/chat/provider/codex"
	"github.com/compdani/pocketbase-mcp/chat/provider/openai"
)

// RouterOptions configures a Router.
type RouterOptions struct {
	Scope Scope
	// Store is the provider persistence. It is required in managed mode and
	// whenever a codex provider needs credential storage.
	Store Store
	// Static, when non-empty, is the provider source instead of Store. Store
	// may still be supplied for codex credentials and settings.
	Static []Provider
	// HTTPClient is shared by every built provider. Nil uses a client with a
	// bounded response-header wait and no total timeout, so streams are bounded
	// by the turn context instead.
	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time
	// TitleChoice is the title model when none is stored (the admin setting
	// wins): "" or chat.TitleChoiceOff (no titles, the default),
	// chat.TitleChoiceAuto (the turn's own model), a choice id
	// ("provider::model"), or a model id.
	TitleChoice string
	// CompactionChoice is the model that writes conversation summaries, in
	// the same forms as TitleChoice: "" or chat.CompactionChoiceOff (off,
	// the default), chat.CompactionChoiceAuto (the turn's own model), a
	// choice id or a model id.
	CompactionChoice string
}

// Router is an immutable snapshot of built providers, swapped atomically on
// reload. It implements chat.ModelRouter.
type Router struct {
	opts       RouterOptions
	client     *http.Client
	ownsClient bool

	mu       sync.RWMutex
	snap     *snapshot
	managers map[string]*codex.Manager

	// buildMu serializes reloads without blocking Resolve/Choices on a slow
	// provider store.
	buildMu sync.Mutex
}

type snapshot struct {
	choices   []chat.ModelChoice
	byID      map[string]*builtProvider
	errs      map[string]string
	defaultID string
	// title is the configured title choice: chat.TitleChoiceAuto,
	// chat.TitleChoiceOff, or a provider/model pair.
	title titleSetting
	// compaction is the summary model, in the same form.
	compaction titleSetting
}

// titleSetting is the stored "titleChoice" setting.
type titleSetting struct {
	Mode     string `json:"mode,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// parseTitleChoice reads the RouterOptions.TitleChoice string form.
func parseTitleChoice(raw string) titleSetting {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "", chat.TitleChoiceOff:
		return titleSetting{Mode: chat.TitleChoiceOff}
	case chat.TitleChoiceAuto:
		return titleSetting{Mode: chat.TitleChoiceAuto}
	}
	if providerID, model, ok := chat.SplitChoiceID(raw); ok {
		return titleSetting{Provider: providerID, Model: model}
	}
	return titleSetting{Model: raw}
}

// String is the public form: auto, off, or the choice id.
func (t titleSetting) String() string {
	switch {
	case t.Mode == chat.TitleChoiceOff:
		return chat.TitleChoiceOff
	case t.Model == "":
		return chat.TitleChoiceAuto
	case t.Provider == "":
		return t.Model
	}
	return chat.ChoiceID(t.Provider, t.Model)
}

type builtProvider struct {
	config  Provider
	impl    provider.Provider
	manager *codex.Manager
}

// NewRouter builds the first snapshot.
func NewRouter(ctx context.Context, opts RouterOptions) (*Router, error) {
	if err := opts.Scope.Validate(); err != nil {
		return nil, err
	}
	if opts.Store == nil && len(opts.Static) == 0 {
		return nil, errors.New("catalog: router requires a store or static providers")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	r := &Router{opts: opts, managers: map[string]*codex.Manager{}}
	if opts.HTTPClient != nil {
		r.client = opts.HTTPClient
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 60 * time.Second
		r.client = &http.Client{Transport: transport}
		r.ownsClient = true
	}
	if err := r.Reload(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload rebuilds the snapshot from the configured source.
func (r *Router) Reload(ctx context.Context) error {
	r.buildMu.Lock()
	defer r.buildMu.Unlock()

	providers, err := r.source(ctx)
	if err != nil {
		return err
	}
	var hostID string
	hasCodex, needsCodex := false, false
	for _, p := range providers {
		if p.Kind == KindCodex {
			hasCodex = true
			if p.Enabled {
				needsCodex = true
			}
		}
	}
	if needsCodex && r.opts.Store == nil {
		return errors.New("catalog: codex providers require a credential store")
	}
	_, hostOwnsCodex := r.opts.Store.(CodexTokenStore)
	if hasCodex && r.opts.Store != nil && !hostOwnsCodex {
		// Only the router's own sign-in uses the host id; a host-owned plan
		// sign-in never needs it.
		hostID, err = r.opts.Store.HostID(ctx, r.opts.Scope)
		if err != nil {
			return fmt.Errorf("catalog: host id: %w", err)
		}
	}

	defaultSetting := struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}{}
	if r.opts.Store != nil {
		_ = r.opts.Store.LoadSetting(ctx, r.opts.Scope, "defaultChoice", &defaultSetting)
	}
	title := parseTitleChoice(r.opts.TitleChoice)
	if r.opts.Store != nil {
		var stored titleSetting
		if err := r.opts.Store.LoadSetting(ctx, r.opts.Scope, titleSettingKey, &stored); err == nil {
			title = stored
			if title.Mode == "" && title.Model == "" {
				title.Mode = chat.TitleChoiceOff
			}
		}
	}

	byID := make(map[string]*builtProvider, len(providers))
	errs := make(map[string]string)
	var choices []chat.ModelChoice
	usedManagers := make(map[string]*codex.Manager)

	for _, p := range providers {
		p = p.Normalized()
		var manager *codex.Manager
		hostTokens, hostOwned := r.opts.Store.(CodexTokenStore)
		if p.Kind == KindCodex && r.opts.Store != nil && !hostOwned {
			// A codex manager exists even while the provider is disabled, so
			// an administrator can sign in before enabling it.
			manager = r.managerFor(p, hostID)
			usedManagers[p.ID] = manager
		}
		if !p.Enabled {
			continue
		}
		if p.SecretError != "" {
			errs[p.ID] = p.SecretError
			continue
		}
		if err := p.Validate(); err != nil {
			errs[p.ID] = err.Error()
			continue
		}
		if p.Kind == KindCodex && hostOwned {
			if !hostTokens.CodexSignedIn(ctx, r.opts.Scope, p.ID) {
				errs[p.ID] = "Not signed in with a ChatGPT plan."
				continue
			}
		} else if p.Kind == KindCodex {
			if manager == nil {
				errs[p.ID] = "Codex requires a credential store."
				continue
			}
			status := manager.Status(ctx)
			if !status.SignedIn || !status.Plan {
				errs[p.ID] = "Not signed in with a ChatGPT plan."
				continue
			}
		}
		var impl provider.Provider
		var err error
		if p.Kind == KindCodex && hostOwned {
			impl, err = r.buildCodex(p, hostCodexToken{store: hostTokens, scope: r.opts.Scope, id: p.ID})
		} else {
			impl, err = r.build(ctx, p, manager)
		}
		if err != nil {
			errs[p.ID] = err.Error()
			continue
		}
		byID[p.ID] = &builtProvider{config: p, impl: impl, manager: manager}
		attachments := attachmentTypesFor(impl)
		for _, m := range p.Models {
			choices = append(choices, chat.ModelChoice{
				ID:              chat.ChoiceID(p.ID, m.ID),
				Provider:        p.ID,
				Model:           m.ID,
				Label:           m.Label,
				Group:           p.Label,
				AttachmentTypes: attachments,
				Efforts:         m.Efforts,
				ContextWindow:   m.Window(),
			})
		}
	}

	defaultID := ""
	if len(choices) > 0 {
		wanted := chat.ChoiceID(defaultSetting.Provider, defaultSetting.Model)
		for _, choice := range choices {
			if choice.ID == wanted {
				defaultID = wanted
				break
			}
		}
		if defaultID == "" {
			defaultID = choices[0].ID
		}
	}
	for i := range choices {
		choices[i].Default = choices[i].ID == defaultID
	}

	snap := &snapshot{choices: choices, byID: byID, errs: errs, defaultID: defaultID, title: title,
		compaction: parseTitleChoice(r.opts.CompactionChoice)}

	r.mu.Lock()
	r.snap = snap
	for id, manager := range r.managers {
		if usedManagers[id] == nil {
			manager.Close()
			delete(r.managers, id)
		}
	}
	r.mu.Unlock()

	for id, err := range errs {
		r.opts.Logger.Warn("MCP chat: provider is unavailable", "provider", id, "error", err)
	}
	return nil
}

func (r *Router) source(ctx context.Context) ([]Provider, error) {
	if len(r.opts.Static) > 0 {
		out := make([]Provider, len(r.opts.Static))
		copy(out, r.opts.Static)
		return out, nil
	}
	return r.opts.Store.ListProviders(ctx, r.opts.Scope)
}

func (r *Router) managerFor(p Provider, hostID string) *codex.Manager {
	r.mu.Lock()
	defer r.mu.Unlock()
	if manager := r.managers[p.ID]; manager != nil {
		if manager.OAuth != nil && manager.OAuth.AuthBase == p.OAuthBaseURL {
			return manager
		}
		// The OAuth base URL changed. The old manager caches discovery and
		// keys for the previous issuer, so replace it; stored credentials are
		// unaffected.
		manager.Close()
		delete(r.managers, p.ID)
	}
	oauth := &codex.OAuth{AuthBase: p.OAuthBaseURL, HTTP: r.client, Now: r.opts.Now}
	manager := codex.NewManager(&credentialStore{store: r.opts.Store, scope: r.opts.Scope, id: p.ID}, oauth, hostID, p.ID)
	manager.Now = r.opts.Now
	r.managers[p.ID] = manager
	return manager
}

func (r *Router) build(_ context.Context, p Provider, manager *codex.Manager) (provider.Provider, error) {
	switch p.Kind {
	case KindOpenAI:
		return openai.New(openai.Config{
			BaseURL:        p.BaseURL,
			APIKey:         p.APIKey,
			HTTPClient:     r.client,
			Headers:        p.Headers,
			Name:           p.ID,
			WireAPI:        p.WireAPI,
			ExtraBody:      p.ExtraBody,
			MaxTokensField: p.MaxTokensField,
			TokenSource:    nil,
		})
	case KindAnthropic:
		return anthropic.New(anthropic.Config{
			BaseURL:    p.BaseURL,
			APIKey:     p.APIKey,
			HTTPClient: r.client,
			Headers:    p.Headers,
			Name:       p.ID,
		})
	case KindCodex:
		if manager == nil {
			return nil, errors.New("codex provider has no credential manager")
		}
		return r.buildCodex(p, manager)
	}
	return nil, fmt.Errorf("unsupported provider kind %q", p.Kind)
}

// buildCodex builds the plan route over a token source: the router's own
// sign-in manager, or the host's (CodexTokenStore).
func (r *Router) buildCodex(p Provider, tokens openai.TokenSource) (provider.Provider, error) {
	return openai.New(openai.Config{
		BaseURL:     p.BaseURL,
		HTTPClient:  r.client,
		Headers:     p.Headers,
		Name:        p.ID,
		WireAPI:     openai.WireResponses,
		ExtraBody:   p.ExtraBody,
		TokenSource: tokens,
	})
}

// BuildTest builds one provider for an admin test. It is not part of the live
// snapshot.
func (r *Router) BuildTest(ctx context.Context, p Provider) (provider.Provider, *codex.Manager, error) {
	p = p.Normalized()
	if err := p.Validate(); err != nil {
		return nil, nil, err
	}
	var manager *codex.Manager
	if p.Kind == KindCodex {
		if r.opts.Store == nil {
			return nil, nil, errors.New("codex providers require a credential store")
		}
		hostID, err := r.opts.Store.HostID(ctx, r.opts.Scope)
		if err != nil {
			return nil, nil, err
		}
		oauth := &codex.OAuth{AuthBase: p.OAuthBaseURL, HTTP: r.client, Now: r.opts.Now}
		manager = codex.NewManager(&credentialStore{store: r.opts.Store, scope: r.opts.Scope, id: p.ID}, oauth, hostID, p.ID)
		manager.Now = r.opts.Now
	}
	impl, err := r.build(ctx, p, manager)
	return impl, manager, err
}

// Choices implements chat.ModelRouter.
func (r *Router) Choices(_ context.Context) ([]chat.ModelChoice, error) {
	r.mu.RLock()
	snap := r.snap
	r.mu.RUnlock()
	if snap == nil {
		return nil, nil
	}
	out := make([]chat.ModelChoice, len(snap.choices))
	copy(out, snap.choices)
	return out, nil
}

// Default implements chat.ModelRouter.
func (r *Router) Default(_ context.Context) (chat.ModelChoice, error) {
	r.mu.RLock()
	snap := r.snap
	r.mu.RUnlock()
	if snap == nil || snap.defaultID == "" {
		return chat.ModelChoice{}, chat.ErrNoModelConfigured
	}
	for _, choice := range snap.choices {
		if choice.ID == snap.defaultID {
			return choice, nil
		}
	}
	return chat.ModelChoice{}, chat.ErrNoModelConfigured
}

// Resolve implements chat.ModelRouter.
func (r *Router) Resolve(_ context.Context, providerID, model string) (chat.ResolvedModel, error) {
	r.mu.RLock()
	snap := r.snap
	r.mu.RUnlock()
	if snap == nil || len(snap.choices) == 0 {
		return chat.ResolvedModel{}, chat.ErrNoModelConfigured
	}
	providerID = strings.TrimSpace(providerID)
	model = strings.TrimSpace(model)

	if providerID != "" {
		built := snap.byID[providerID]
		if built == nil {
			return chat.ResolvedModel{}, chat.ErrModelUnknown
		}
		if model == "" {
			model = built.config.DefaultModelID()
		}
		choice, ok := choiceFor(built, model)
		if !ok {
			return chat.ResolvedModel{}, chat.ErrModelUnknown
		}
		return chat.ResolvedModel{Choice: choice, Provider: built.impl, Model: model}, nil
	}

	defaultID := snap.defaultID
	if model == "" {
		return r.defaultResolved(snap)
	}
	if built := snap.byID[choiceProvider(defaultID)]; built != nil {
		if choice, ok := choiceFor(built, model); ok {
			return chat.ResolvedModel{Choice: choice, Provider: built.impl, Model: model}, nil
		}
	}
	var found []*builtProvider
	var foundChoice chat.ModelChoice
	for _, built := range snap.byID {
		if choice, ok := choiceFor(built, model); ok {
			found = append(found, built)
			foundChoice = choice
		}
	}
	switch len(found) {
	case 0:
		return chat.ResolvedModel{}, chat.ErrModelUnknown
	case 1:
		return chat.ResolvedModel{Choice: foundChoice, Provider: found[0].impl, Model: model}, nil
	default:
		return chat.ResolvedModel{}, chat.ErrModelAmbiguous
	}
}

func (r *Router) defaultResolved(snap *snapshot) (chat.ResolvedModel, error) {
	for _, choice := range snap.choices {
		if choice.ID != snap.defaultID {
			continue
		}
		built := snap.byID[choice.Provider]
		if built == nil {
			break
		}
		return chat.ResolvedModel{Choice: choice, Provider: built.impl, Model: choice.Model}, nil
	}
	return chat.ResolvedModel{}, chat.ErrNoModelConfigured
}

// titleSettingKey is the provider-store setting holding the title model.
const titleSettingKey = "titleChoice"

// TitleChoice reports the configured title model: chat.TitleChoiceAuto,
// chat.TitleChoiceOff, or a choice id.
func (r *Router) TitleChoice() string {
	r.mu.RLock()
	snap := r.snap
	r.mu.RUnlock()
	if snap == nil {
		return chat.TitleChoiceOff
	}
	return snap.title.String()
}

// TitleModel implements chat.TitleRouter. A configured model that is not
// available (disabled, signed out, removed) falls back to the turn's model,
// so titles keep working; "off" turns them off.
func (r *Router) TitleModel(ctx context.Context, turn chat.ResolvedModel) (chat.ResolvedModel, bool) {
	r.mu.RLock()
	snap := r.snap
	r.mu.RUnlock()
	if snap == nil {
		return chat.ResolvedModel{}, false
	}
	switch {
	case snap.title.Mode == chat.TitleChoiceOff:
		return chat.ResolvedModel{}, false
	case snap.title.Model == "":
		return turn, true
	}
	resolved, err := r.Resolve(ctx, snap.title.Provider, snap.title.Model)
	if err != nil {
		r.opts.Logger.Warn("MCP chat: the title model is not available; using the turn's model",
			"title", snap.title.String(), "error", err.Error())
		return turn, true
	}
	return resolved, true
}

// CompactionEnabled reports whether a summary model is configured.
func (r *Router) CompactionEnabled() bool {
	r.mu.RLock()
	snap := r.snap
	r.mu.RUnlock()
	return snap != nil && snap.compaction.Mode != chat.TitleChoiceOff
}

// CompactionModel implements chat.CompactionRouter, like TitleModel: a
// configured model that is not available falls back to the turn's model.
func (r *Router) CompactionModel(ctx context.Context, turn chat.ResolvedModel) (chat.ResolvedModel, bool) {
	r.mu.RLock()
	snap := r.snap
	r.mu.RUnlock()
	if snap == nil {
		return chat.ResolvedModel{}, false
	}
	switch {
	case snap.compaction.Mode == chat.TitleChoiceOff:
		return chat.ResolvedModel{}, false
	case snap.compaction.Model == "":
		return turn, true
	}
	resolved, err := r.Resolve(ctx, snap.compaction.Provider, snap.compaction.Model)
	if err != nil {
		r.opts.Logger.Warn("MCP chat: the summary model is not available; using the turn's model",
			"summary", snap.compaction.String(), "error", err.Error())
		return turn, true
	}
	return resolved, true
}

func choiceFor(built *builtProvider, model string) (chat.ModelChoice, bool) {
	m, ok := built.config.Model(model)
	if !ok {
		return chat.ModelChoice{}, false
	}
	return chat.ModelChoice{
		ID:              chat.ChoiceID(built.config.ID, m.ID),
		Provider:        built.config.ID,
		Model:           m.ID,
		Label:           m.Label,
		Group:           built.config.Label,
		AttachmentTypes: attachmentTypesFor(built.impl),
		Efforts:         m.Efforts,
		ContextWindow:   m.Window(),
	}, true
}

func choiceProvider(id string) string {
	providerID, _, ok := chat.SplitChoiceID(id)
	if !ok {
		return ""
	}
	return providerID
}

// SupportsAttachment implements chat.ModelRouter as the union over ready
// providers.
func (r *Router) SupportsAttachment(mimeType string) bool {
	r.mu.RLock()
	snap := r.snap
	r.mu.RUnlock()
	if snap == nil {
		return false
	}
	for _, built := range snap.byID {
		support, ok := built.impl.(provider.AttachmentSupport)
		if ok && support.SupportsAttachment(mimeType) {
			return true
		}
	}
	return false
}

// ProviderError returns the last build error for one provider, or "".
func (r *Router) ProviderError(id string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snap == nil {
		return ""
	}
	return r.snap.errs[id]
}

// Manager returns one codex manager, or nil for a non-codex provider.
func (r *Router) Manager(id string) *codex.Manager {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.managers[id]
}

// HTTPClient returns the shared client used by built providers.
func (r *Router) HTTPClient() *http.Client { return r.client }

// Close stops sign-in listeners and releases idle transport connections when
// this router owns the client.
func (r *Router) Close() {
	r.mu.Lock()
	for id, manager := range r.managers {
		manager.Close()
		delete(r.managers, id)
	}
	r.mu.Unlock()
	if r.ownsClient {
		if transport, ok := r.client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
}

func attachmentTypesFor(p provider.Provider) []string {
	support, ok := p.(provider.AttachmentSupport)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(provider.AttachmentMIMETypes()))
	for _, mimeType := range provider.AttachmentMIMETypes() {
		if support.SupportsAttachment(mimeType) {
			out = append(out, mimeType)
		}
	}
	return out
}

// credentialStore binds a codex.Store to one provider within a catalog scope.
type credentialStore struct {
	store Store
	scope Scope
	id    string
}

func (c *credentialStore) Load(ctx context.Context) (*codex.Credentials, error) {
	return c.store.LoadCredentials(ctx, c.scope, c.id)
}

func (c *credentialStore) Save(ctx context.Context, creds *codex.Credentials) error {
	return c.store.SaveCredentials(ctx, c.scope, c.id, creds)
}

func (c *credentialStore) Clear(ctx context.Context) error {
	return c.store.ClearCredentials(ctx, c.scope, c.id)
}
