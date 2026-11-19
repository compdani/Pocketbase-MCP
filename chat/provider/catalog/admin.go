package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider/codex"
)

// Admin implements chat.ProviderAdmin. The HTTP layer supplies the superuser
// gate; this type is about config and sign-in only.
type Admin struct {
	scope  Scope
	store  Store
	router *Router
	logger *slog.Logger
	client *http.Client
	now    func() time.Time
}

// NewAdmin returns a provider management service.
func NewAdmin(opts RouterOptions) (*Admin, error) {
	if err := opts.Scope.Validate(); err != nil {
		return nil, err
	}
	if opts.Store == nil {
		return nil, errors.New("catalog: managed providers require a store")
	}
	router, err := NewRouter(context.Background(), opts)
	if err != nil {
		return nil, err
	}
	return &Admin{
		scope:  opts.Scope,
		store:  opts.Store,
		router: router,
		logger: opts.Logger,
		client: router.HTTPClient(),
		now:    opts.Now,
	}, nil
}

// NewAdminWithRouter returns a management service over an existing router.
func NewAdminWithRouter(router *Router, opts RouterOptions) (*Admin, error) {
	if router == nil {
		return nil, errors.New("catalog: admin requires a router")
	}
	if opts.Store == nil {
		return nil, errors.New("catalog: managed providers require a store")
	}
	return &Admin{
		scope:  opts.Scope,
		store:  opts.Store,
		router: router,
		logger: opts.Logger,
		client: router.HTTPClient(),
		now:    opts.Now,
	}, nil
}

// Router returns the router this admin reloads.
func (a *Admin) Router() *Router { return a.router }

func (a *Admin) nowTime() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *Admin) ListProviders(ctx context.Context) ([]chat.ProviderView, string, error) {
	providers, err := a.store.ListProviders(ctx, a.scope)
	if err != nil {
		return nil, "", a.storeError(err)
	}
	out := make([]chat.ProviderView, 0, len(providers))
	for _, p := range providers {
		out = append(out, a.view(ctx, p))
	}
	defaultID := ""
	if choice, err := a.router.Default(ctx); err == nil {
		defaultID = choice.ID
	}
	return out, defaultID, nil
}

func (a *Admin) GetProvider(ctx context.Context, id string) (chat.ProviderView, error) {
	p, err := a.store.GetProvider(ctx, a.scope, id)
	if err != nil {
		return chat.ProviderView{}, a.storeError(err)
	}
	return a.view(ctx, p), nil
}

func (a *Admin) SaveProvider(ctx context.Context, id string, in chat.ProviderInput) (chat.ProviderView, error) {
	create := strings.TrimSpace(id) == ""
	if create {
		id = strings.TrimSpace(in.ID)
		if id == "" {
			return chat.ProviderView{}, a.bad("A provider id is required.")
		}
	}
	if in.ID != "" && strings.TrimSpace(in.ID) != id {
		return chat.ProviderView{}, a.bad("A provider id cannot be changed.")
	}

	var p Provider
	if create {
		// Store.SaveProvider is an upsert. A create must not silently replace
		// an existing entry (and drop its stored key), so check first; the
		// unique index still catches a concurrent create.
		if _, err := a.store.GetProvider(ctx, a.scope, id); err == nil {
			return chat.ProviderView{}, a.storeError(ErrProviderExists)
		} else if !errors.Is(err, ErrProviderNotFound) {
			return chat.ProviderView{}, a.storeError(err)
		}
		p = Provider{ID: id, Enabled: true}
	} else {
		existing, err := a.store.GetProvider(ctx, a.scope, id)
		if err != nil {
			return chat.ProviderView{}, a.storeError(err)
		}
		if in.Kind != nil && strings.TrimSpace(*in.Kind) != string(existing.Kind) {
			return chat.ProviderView{}, a.bad("A provider kind cannot be changed after creation.")
		}
		p = existing
	}
	p = applyInput(p, in)
	if err := requireSecretReentry(p, in); err != "" {
		return chat.ProviderView{}, a.bad(err)
	}
	p = clearSecretErrors(p)
	p = p.Normalized()
	if p.Enabled && len(p.Models) == 0 && p.Kind != KindCodex {
		// New provider with no explicit allowlist: try the provider's /models
		// endpoint once. A failure leaves the empty list and the ordinary
		// validation error, so the admin can add models by hand.
		if models, err := a.discover(ctx, p); err == nil && len(models) > 0 {
			p.Models = models
			p.DefaultModel = models[0].ID
		}
	}
	if err := p.Validate(); err != nil {
		return chat.ProviderView{}, a.bad(err.Error())
	}
	if err := a.store.SaveProvider(ctx, a.scope, p); err != nil {
		return chat.ProviderView{}, a.storeError(err)
	}
	if err := a.router.Reload(ctx); err != nil {
		return chat.ProviderView{}, a.internal(err)
	}
	return a.view(ctx, p), nil
}

func applyInput(p Provider, in chat.ProviderInput) Provider {
	p.ID = strings.TrimSpace(p.ID)
	if in.Label != nil {
		p.Label = *in.Label
	}
	if in.Kind != nil {
		p.Kind = Kind(strings.TrimSpace(*in.Kind))
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.BaseURL != nil {
		p.BaseURL = *in.BaseURL
	}
	if in.OAuthBaseURL != nil {
		p.OAuthBaseURL = *in.OAuthBaseURL
	}
	if in.WireAPI != nil {
		p.WireAPI = *in.WireAPI
	}
	if in.Models != nil {
		models := make([]Model, 0, len(*in.Models))
		for _, m := range *in.Models {
			models = append(models, Model{ID: m.ID, Label: m.Label, Efforts: m.Efforts, ContextWindow: m.ContextWindow})
		}
		p.Models = models
	}
	if in.DefaultModel != nil {
		p.DefaultModel = *in.DefaultModel
	}
	if in.Headers != nil {
		p.Headers = in.Headers
	}
	if in.ExtraBody != nil {
		p.ExtraBody = in.ExtraBody
	}
	if in.APIKey != nil {
		p.APIKey = *in.APIKey
	}
	return p
}

func (a *Admin) DeleteProvider(ctx context.Context, id string) error {
	if err := a.store.DeleteProvider(ctx, a.scope, id); err != nil {
		return a.storeError(err)
	}
	var def struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := a.store.LoadSetting(ctx, a.scope, "defaultChoice", &def); err == nil && def.Provider == id {
		_ = a.store.SaveSetting(ctx, a.scope, "defaultChoice", map[string]string{})
	}
	if err := a.router.Reload(ctx); err != nil {
		return a.internal(err)
	}
	return nil
}

func (a *Admin) TestProvider(ctx context.Context, id string) (chat.ProviderTest, error) {
	p, err := a.store.GetProvider(ctx, a.scope, id)
	if err != nil {
		return chat.ProviderTest{}, a.storeError(err)
	}
	return a.test(ctx, p)
}

func (a *Admin) TestProviderConfig(ctx context.Context, in chat.ProviderInput) (chat.ProviderTest, error) {
	id := strings.TrimSpace(in.ID)
	p := Provider{ID: id, Enabled: true}
	if id != "" {
		if existing, err := a.store.GetProvider(ctx, a.scope, id); err == nil {
			p = existing
		}
	}
	if p.ID == "" {
		p.ID = "test-provider"
	}
	p = applyInput(p, in)
	p.ID = id
	if p.ID == "" {
		p.ID = "test-provider"
	}
	if err := requireSecretReentry(p, in); err != "" {
		return chat.ProviderTest{}, a.bad(err)
	}
	return a.test(ctx, clearSecretErrors(p))
}

// requireSecretReentry returns a message when a stored secret could not be
// decrypted and this input does not replace that specific secret. Replacing
// only the other secret would otherwise save the unreadable one as empty.
func requireSecretReentry(p Provider, in chat.ProviderInput) string {
	if p.APIKeyUnreadable && in.APIKey == nil {
		return "The API key could not be decrypted; check the encryption key or re-enter it."
	}
	if p.HeadersUnreadable && in.Headers == nil {
		return "The provider headers could not be decrypted; check the encryption key or re-enter them."
	}
	if p.SecretError != "" && !p.APIKeyUnreadable && !p.HeadersUnreadable && in.APIKey == nil && in.Headers == nil {
		return p.SecretError
	}
	return ""
}

func clearSecretErrors(p Provider) Provider {
	p.SecretError = ""
	p.APIKeyUnreadable = false
	p.HeadersUnreadable = false
	return p
}

func (a *Admin) test(ctx context.Context, p Provider) (chat.ProviderTest, error) {
	if p.SecretError != "" {
		return chat.ProviderTest{}, a.bad(p.SecretError)
	}
	p = p.Normalized()
	if err := p.Validate(); err != nil {
		return chat.ProviderTest{}, a.bad(err.Error())
	}
	start := a.nowTime()
	models, err := a.discover(ctx, p)
	latency := a.nowTime().Sub(start).Milliseconds()
	if err != nil {
		var ae *chat.AdminError
		if errors.As(err, &ae) {
			return chat.ProviderTest{}, err
		}
		return chat.ProviderTest{OK: false, Message: err.Error(), LatencyMS: latency}, nil
	}
	return chat.ProviderTest{OK: true, Message: "Provider responded.", LatencyMS: latency, Models: toOptions(models)}, nil
}

func (a *Admin) RefreshProviderModels(ctx context.Context, id string, save bool) ([]chat.ModelOption, error) {
	p, err := a.store.GetProvider(ctx, a.scope, id)
	if err != nil {
		return nil, a.storeError(err)
	}
	if p.SecretError != "" {
		return nil, a.bad(p.SecretError)
	}
	models, err := a.discover(ctx, p)
	if err != nil {
		return nil, err
	}
	if save {
		if len(models) == 0 {
			return nil, a.bad("The provider returned no models; the current list was kept.")
		}
		// Discovery may not know a model's efforts; keep ones an
		// administrator set rather than wiping them on refresh.
		for i, m := range models {
			if old, ok := p.Model(m.ID); ok && len(m.Efforts) == 0 && len(old.Efforts) > 0 {
				models[i].Efforts = old.Efforts
			}
			if old, ok := p.Model(m.ID); ok && m.ContextWindow == 0 && old.ContextWindow > 0 {
				models[i].ContextWindow = old.ContextWindow
			}
		}
		p.Models = models
		if _, ok := p.Model(p.DefaultModel); !ok {
			p.DefaultModel = models[0].ID
		}
		if err := a.store.SaveProvider(ctx, a.scope, p.Normalized()); err != nil {
			return nil, a.storeError(err)
		}
		if err := a.router.Reload(ctx); err != nil {
			return nil, a.internal(err)
		}
	}
	return toOptions(models), nil
}

func (a *Admin) SetDefaultChoice(ctx context.Context, providerID, model string) error {
	if providerID == "" || model == "" {
		return a.bad("Choose a provider and model.")
	}
	if _, err := a.router.Resolve(ctx, providerID, model); err != nil {
		return a.bad("That model is not available.")
	}
	if err := a.store.SaveSetting(ctx, a.scope, "defaultChoice", map[string]string{
		"provider": providerID,
		"model":    model,
	}); err != nil {
		return a.storeError(err)
	}
	if err := a.router.Reload(ctx); err != nil {
		return a.internal(err)
	}
	return nil
}

// TitleChoice reports the title model: chat.TitleChoiceAuto,
// chat.TitleChoiceOff, or a choice id.
func (a *Admin) TitleChoice(context.Context) (string, error) {
	return a.router.TitleChoice(), nil
}

// SetTitleChoice stores the title model: mode chat.TitleChoiceAuto titles
// with each turn's own model, chat.TitleChoiceOff turns titles off, and an
// empty mode with a provider and model picks that model.
func (a *Admin) SetTitleChoice(ctx context.Context, mode, providerID, model string) error {
	var setting titleSetting
	switch {
	case mode == chat.TitleChoiceOff:
		setting = titleSetting{Mode: chat.TitleChoiceOff}
	case mode == chat.TitleChoiceAuto:
		setting = titleSetting{Mode: chat.TitleChoiceAuto}
	case mode != "":
		return a.bad("Choose auto, off, or a provider and model.")
	case providerID == "" || model == "":
		return a.bad("Choose a provider and model.")
	default:
		if _, err := a.router.Resolve(ctx, providerID, model); err != nil {
			return a.bad("That model is not available.")
		}
		setting = titleSetting{Provider: providerID, Model: model}
	}
	if err := a.store.SaveSetting(ctx, a.scope, titleSettingKey, setting); err != nil {
		return a.storeError(err)
	}
	if err := a.router.Reload(ctx); err != nil {
		return a.internal(err)
	}
	return nil
}

func (a *Admin) BeginSignIn(ctx context.Context, id, adminID string) (chat.SignInStart, error) {
	ctx = context.WithoutCancel(ctx)
	manager, err := a.codexManager(ctx, id)
	if err != nil {
		return chat.SignInStart{}, err
	}
	start, err := manager.BeginSignIn(ctx, adminID)
	if err != nil {
		return chat.SignInStart{}, a.bad(err.Error())
	}
	return chat.SignInStart{AuthURL: start.AuthURL, ExpiresAt: start.ExpiresAt}, nil
}

func (a *Admin) CompleteSignIn(ctx context.Context, id, adminID, callbackURL string) (chat.ProviderView, error) {
	ctx = context.WithoutCancel(ctx)
	manager, err := a.codexManager(ctx, id)
	if err != nil {
		return chat.ProviderView{}, err
	}
	if err := manager.CompleteSignIn(ctx, adminID, callbackURL); err != nil {
		return chat.ProviderView{}, a.bad(err.Error())
	}
	if err := a.router.Reload(ctx); err != nil {
		return chat.ProviderView{}, a.internal(err)
	}
	// A first sign-in often has no model list yet. Populate it once so the new
	// provider immediately appears in /status; a failure is non-fatal.
	if p, err := a.store.GetProvider(ctx, a.scope, id); err == nil && len(p.Models) == 0 {
		_, _ = a.RefreshProviderModels(ctx, id, true)
	}
	return a.GetProvider(ctx, id)
}

func (a *Admin) CancelSignIn(ctx context.Context, id, adminID string) error {
	manager, err := a.codexManager(ctx, id)
	if err != nil {
		return err
	}
	if err := manager.CancelSignIn(ctx, adminID); err != nil {
		return a.bad(err.Error())
	}
	return nil
}

func (a *Admin) SignOut(ctx context.Context, id string) error {
	ctx = context.WithoutCancel(ctx)
	manager, err := a.codexManager(ctx, id)
	if err != nil {
		return err
	}
	if err := manager.SignOut(ctx); err != nil {
		return a.internal(err)
	}
	if err := a.router.Reload(ctx); err != nil {
		return a.internal(err)
	}
	return nil
}

func (a *Admin) ImportCredentials(ctx context.Context, id string, raw json.RawMessage) (chat.ProviderView, error) {
	ctx = context.WithoutCancel(ctx)
	manager, err := a.codexManager(ctx, id)
	if err != nil {
		return chat.ProviderView{}, err
	}
	cred, err := codex.DecodeCredentials(raw)
	if err != nil {
		return chat.ProviderView{}, a.bad(err.Error())
	}
	if err := manager.Import(ctx, cred); err != nil {
		return chat.ProviderView{}, a.bad(err.Error())
	}
	if err := a.router.Reload(ctx); err != nil {
		return chat.ProviderView{}, a.internal(err)
	}
	return a.GetProvider(ctx, id)
}

func (a *Admin) codexManager(ctx context.Context, id string) (*codex.Manager, error) {
	p, err := a.store.GetProvider(ctx, a.scope, id)
	if err != nil {
		return nil, a.storeError(err)
	}
	if p.Kind != KindCodex {
		return nil, a.bad("Sign-in is only available for a codex provider.")
	}
	manager := a.router.Manager(id)
	if manager == nil {
		return nil, a.bad("This codex provider is not available.")
	}
	return manager, nil
}

func (a *Admin) view(ctx context.Context, p Provider) chat.ProviderView {
	p = p.Normalized()
	view := chat.ProviderView{
		ID:           p.ID,
		Label:        p.Label,
		Kind:         string(p.Kind),
		Enabled:      p.Enabled,
		BaseURL:      p.BaseURL,
		OAuthBaseURL: p.OAuthBaseURL,
		WireAPI:      p.WireAPI,
		Models:       toOptions(p.Models),
		DefaultModel: p.DefaultModelID(),
		HasAPIKey:    p.APIKey != "",
		APIKeyHint:   keyHint(p.APIKey),
		HasExtraBody: len(p.ExtraBody) > 0,
		LastError:    a.router.ProviderError(p.ID),
	}
	if p.SecretError != "" {
		view.LastError = p.SecretError
	}
	if view.Models == nil {
		view.Models = []chat.ModelOption{}
	}
	if len(p.Headers) > 0 {
		view.HeaderNames = make([]string, 0, len(p.Headers))
		for name := range p.Headers {
			view.HeaderNames = append(view.HeaderNames, name)
		}
		sort.Strings(view.HeaderNames)
	}
	if p.Kind == KindCodex {
		if manager := a.router.Manager(p.ID); manager != nil {
			status := manager.Status(ctx)
			view.SignedIn = status.SignedIn
			view.AccountEmail = status.Email
			view.Plan = status.Plan
			if status.SignedIn && !status.ExpiresAt.IsZero() {
				expires := status.ExpiresAt
				view.ExpiresAt = &expires
			}
			if status.Error != "" {
				view.LastError = status.Error
			}
		}
	}
	return view
}

// discover lists the models a provider currently offers.
func (a *Admin) discover(ctx context.Context, p Provider) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	switch p.Kind {
	case KindOpenAI:
		base := p.BaseURL
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return a.listHTTPModels(ctx, p, strings.TrimRight(base, "/")+"/models", false)
	case KindAnthropic:
		base := p.BaseURL
		if base == "" {
			base = "https://api.anthropic.com"
		}
		return a.listHTTPModels(ctx, p, strings.TrimRight(base, "/")+"/v1/models", true)
	case KindCodex:
		manager := a.router.Manager(p.ID)
		if manager == nil {
			// An unsaved config has no live manager. Build a temporary one so
			// the settings UI can still test a pasted credential.
			_, temp, err := a.router.BuildTest(ctx, p)
			if err != nil {
				return nil, a.bad(err.Error())
			}
			manager = temp
		}
		if manager == nil {
			return nil, a.bad("This codex provider is not available.")
		}
		token, err := manager.Token(ctx, false)
		if err != nil {
			return nil, a.bad("Sign in to the ChatGPT plan first.")
		}
		base := p.BaseURL
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		list, err := codex.ListModels(ctx, a.client, base, token)
		if err != nil {
			return nil, a.bad("The provider did not return a model list.")
		}
		out := make([]Model, 0, len(list))
		for _, m := range list {
			out = append(out, Model{ID: m.Slug, Label: m.DisplayName, Efforts: NormalizeEfforts(m.Efforts)})
		}
		return out, nil
	}
	return nil, a.bad("Unsupported provider kind.")
}

func (a *Admin) listHTTPModels(ctx context.Context, p Provider, endpoint string, anthropic bool) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, a.bad("The provider model list URL is invalid.")
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	if anthropic {
		if p.APIKey != "" && req.Header.Get("x-api-key") == "" {
			req.Header.Set("x-api-key", p.APIKey)
		}
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	} else if p.APIKey != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, a.bad("The provider could not be reached.")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, a.bad(fmt.Sprintf("The provider returned HTTP %d.", resp.StatusCode))
	}
	var body struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, a.bad("The provider returned an unreadable model list.")
	}
	list := append(body.Data, body.Models...)
	out := make([]Model, 0, len(list))
	for _, m := range list {
		if v, ok := m["visibility"].(string); ok && v != "list" {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			id, _ = m["slug"].(string)
		}
		if id == "" {
			continue
		}
		label, _ := m["display_name"].(string)
		if label == "" {
			label, _ = m["name"].(string)
		}
		if label == "" {
			label = id
		}
		efforts := reasoningLevels(m["supported_reasoning_levels"])
		if len(efforts) == 0 && !anthropic {
			efforts = DefaultEfforts(id)
		}
		out = append(out, Model{ID: id, Label: label, Efforts: NormalizeEfforts(efforts)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func toOptions(models []Model) []chat.ModelOption {
	out := make([]chat.ModelOption, 0, len(models))
	for _, m := range models {
		out = append(out, chat.ModelOption{ID: m.ID, Label: m.Label, Efforts: m.Efforts, ContextWindow: m.ContextWindow})
	}
	return out
}

func keyHint(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "…"
	}
	return "…" + key[len(key)-4:]
}

func (a *Admin) bad(message string) error {
	return &chat.AdminError{Status: http.StatusBadRequest, Code: chat.CodeInvalidRequest, Message: message}
}

func (a *Admin) internal(err error) error {
	if a.logger != nil {
		a.logger.Warn("MCP chat: provider admin error", "error", err)
	}
	return &chat.AdminError{Status: http.StatusInternalServerError, Code: chat.CodeInternal, Message: "The provider operation failed."}
}

func (a *Admin) storeError(err error) error {
	switch {
	case errors.Is(err, ErrProviderNotFound):
		return &chat.AdminError{Status: http.StatusNotFound, Code: chat.CodeNotFound, Message: "Provider not found."}
	case errors.Is(err, ErrProviderExists):
		return &chat.AdminError{Status: http.StatusConflict, Code: chat.CodeConflict, Message: "A provider with that id already exists."}
	case errors.Is(err, ErrNoEncryptionKey):
		return &chat.AdminError{Status: http.StatusInternalServerError, Code: chat.CodeInternal, Message: "Provider secrets require PB_ENCRYPTION_KEY."}
	default:
		return a.internal(err)
	}
}

// reasoningLevels reads a model list's supported_reasoning_levels, which is
// either a list of effort names or a list of {"effort": ...} objects.
func reasoningLevels(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		switch x := item.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			if e, ok := x["effort"].(string); ok {
				out = append(out, e)
			}
		}
	}
	return out
}
