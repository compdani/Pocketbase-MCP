// Package catalog owns the named provider catalog: provider configuration,
// per-turn routing and the superuser management service.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/codex"
)

// Kind is a built-in provider protocol.
type Kind string

const (
	KindOpenAI    Kind = "openai"
	KindAnthropic Kind = "anthropic"
	KindCodex     Kind = "codex"
)

// Scope partitions installation-level provider configuration. Namespace and
// Mount match the chat ownership partition, so several apps can share a fork
// database without seeing each other's providers.
type Scope struct {
	Namespace string
	Mount     string
}

// Validate checks that a scope names a partition.
func (s Scope) Validate() error {
	if strings.TrimSpace(s.Namespace) == "" {
		return errors.New("catalog: provider scope requires a namespace")
	}
	if strings.TrimSpace(s.Mount) == "" {
		return errors.New("catalog: provider scope requires a mount")
	}
	return nil
}

// Model is one allowed model and its display label.
type Model struct {
	ID    string
	Label string
	// Efforts lists the reasoning efforts the model accepts (values from
	// provider.ReasoningEfforts). Empty means the model takes no effort, and
	// a turn naming one is refused. Discovery fills it for Codex plan models
	// and known OpenAI reasoning models; administrators can edit it.
	Efforts []string `json:",omitempty"`
	// ContextWindow is the model's input window in tokens. Zero means the
	// built-in default for known model ids (chat.DefaultContextWindow), or
	// unknown. Set it for a gateway that serves a smaller window.
	ContextWindow int `json:",omitempty"`
}

// Window returns the model's context window: its own, or the default for
// its id.
func (m Model) Window() int {
	if m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return chat.DefaultContextWindow(m.ID)
}

// Provider is one catalog entry. APIKey is a runtime secret: the adapter
// decrypts it on load and encrypts it on save. It is never serialized to the
// admin API.
type Provider struct {
	ID           string
	Label        string
	Kind         Kind
	Enabled      bool
	BaseURL      string
	OAuthBaseURL string
	WireAPI      string
	Models       []Model
	DefaultModel string
	Headers      map[string]string
	ExtraBody    map[string]any
	APIKey       string
	// SecretError is a runtime-only note set by a store when a stored secret
	// cannot be decrypted. It keeps one broken provider from taking the whole
	// catalog down; the admin view asks for re-entry.
	SecretError string
	// APIKeyUnreadable and HeadersUnreadable say which stored secret failed to
	// decrypt, so an update must re-enter that secret before it is saved.
	APIKeyUnreadable  bool
	HeadersUnreadable bool
}

var providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Validate checks one provider before it is stored or built.
func (p Provider) Validate() error {
	if !providerIDPattern.MatchString(p.ID) {
		return fmt.Errorf("provider id %q must match %s", p.ID, providerIDPattern)
	}
	switch p.Kind {
	case KindOpenAI, KindAnthropic, KindCodex:
	default:
		return fmt.Errorf("provider %q has unsupported kind %q", p.ID, p.Kind)
	}
	if p.WireAPI != "" {
		switch p.Kind {
		case KindOpenAI:
			if p.WireAPI != "chat" && p.WireAPI != "responses" {
				return fmt.Errorf("provider %q: wire API must be chat or responses", p.ID)
			}
		case KindCodex:
			if p.WireAPI != "responses" {
				return fmt.Errorf("provider %q: codex requires the responses wire API", p.ID)
			}
		default:
			return fmt.Errorf("provider %q: wire API is only valid for openai or codex", p.ID)
		}
	}
	for _, candidate := range []struct {
		name  string
		value string
	}{{"base URL", p.BaseURL}, {"OAuth base URL", p.OAuthBaseURL}} {
		if candidate.value == "" {
			continue
		}
		u, err := url.Parse(candidate.value)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("provider %q has invalid %s %q", p.ID, candidate.name, candidate.value)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("provider %q has unsupported %s scheme %q", p.ID, candidate.name, u.Scheme)
		}
		if u.User != nil {
			return fmt.Errorf("provider %q must not embed credentials in its %s", p.ID, candidate.name)
		}
	}
	for name, value := range p.Headers {
		if strings.ContainsAny(name, "\r\n:") || strings.TrimSpace(name) == "" {
			return fmt.Errorf("provider %q has invalid header name %q", p.ID, name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("provider %q header %q contains a line break", p.ID, name)
		}
		if p.Kind == KindCodex && strings.EqualFold(name, "Authorization") {
			return fmt.Errorf("provider %q: codex providers set Authorization from the signed-in plan, not from a header", p.ID)
		}
	}
	if p.Kind == KindAnthropic && len(p.ExtraBody) > 0 {
		return fmt.Errorf("provider %q: extra body is not supported by the Anthropic adapter", p.ID)
	}
	seenModels := make(map[string]struct{}, len(p.Models))
	for _, m := range p.Models {
		for _, e := range m.Efforts {
			if !provider.ValidReasoningEffort(e) {
				return fmt.Errorf("provider %q model %q has unknown reasoning effort %q", p.ID, m.ID, e)
			}
		}
		if m.ContextWindow < 0 {
			return fmt.Errorf("provider %q model %q has a negative context window", p.ID, m.ID)
		}
		if len(m.Efforts) > 0 && p.Kind == KindAnthropic {
			return fmt.Errorf("provider %q: reasoning effort is not supported for Anthropic models", p.ID)
		}
		if _, ok := seenModels[m.ID]; ok {
			return fmt.Errorf("provider %q has duplicate model %q", p.ID, m.ID)
		}
		seenModels[m.ID] = struct{}{}
	}
	if p.Enabled {
		if len(p.Models) == 0 {
			return fmt.Errorf("provider %q has no models", p.ID)
		}
		if p.DefaultModelID() == "" {
			return fmt.Errorf("provider %q has no default model", p.ID)
		}
		if p.DefaultModel != "" {
			if _, ok := p.Model(p.DefaultModel); !ok {
				return fmt.Errorf("provider %q default model %q is not in its model list", p.ID, p.DefaultModel)
			}
		}
	}
	return nil
}

// DefaultModelID returns the configured default, or the first model.
func (p Provider) DefaultModelID() string {
	if strings.TrimSpace(p.DefaultModel) != "" {
		return strings.TrimSpace(p.DefaultModel)
	}
	if len(p.Models) > 0 {
		return p.Models[0].ID
	}
	return ""
}

// Model returns one allowed model.
func (p Provider) Model(id string) (Model, bool) {
	for _, m := range p.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// Normalized trims and fills the defaults a stored provider should have.
func (p Provider) Normalized() Provider {
	p.ID = strings.TrimSpace(p.ID)
	p.Label = strings.TrimSpace(p.Label)
	p.Kind = Kind(strings.TrimSpace(string(p.Kind)))
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	p.OAuthBaseURL = strings.TrimRight(strings.TrimSpace(p.OAuthBaseURL), "/")
	p.WireAPI = strings.TrimSpace(p.WireAPI)
	p.DefaultModel = strings.TrimSpace(p.DefaultModel)
	if p.Kind == KindOpenAI && p.WireAPI == "" {
		p.WireAPI = "chat"
	}
	if p.Kind == KindCodex {
		p.WireAPI = "responses"
		if p.BaseURL == "" {
			p.BaseURL = "https://api.openai.com/v1"
		}
	}
	if p.Label == "" {
		p.Label = p.ID
	}
	models := make([]Model, 0, len(p.Models))
	for _, m := range p.Models {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" {
			continue
		}
		m.Label = strings.TrimSpace(m.Label)
		if m.Label == "" {
			m.Label = m.ID
		}
		m.Efforts = NormalizeEfforts(m.Efforts)
		models = append(models, m)
	}
	p.Models = models
	if p.DefaultModel == "" && len(models) > 0 {
		p.DefaultModel = models[0].ID
	}
	if p.Headers == nil {
		p.Headers = map[string]string{}
	}
	return p
}

// Errors returned by a Store or Admin.
var (
	ErrProviderNotFound = errors.New("catalog: provider not found")
	ErrProviderExists   = errors.New("catalog: provider already exists")
	ErrSettingNotFound  = errors.New("catalog: setting not found")
	ErrNoEncryptionKey  = errors.New("catalog: provider secrets require an encryption key")
)

// Store persists provider configuration and the associated secrets. The
// adapter encrypts API keys and codex credentials before they reach the
// database.
type Store interface {
	ListProviders(ctx context.Context, scope Scope) ([]Provider, error)
	GetProvider(ctx context.Context, scope Scope, id string) (Provider, error)
	SaveProvider(ctx context.Context, scope Scope, p Provider) error
	DeleteProvider(ctx context.Context, scope Scope, id string) error

	LoadCredentials(ctx context.Context, scope Scope, id string) (*codex.Credentials, error)
	SaveCredentials(ctx context.Context, scope Scope, id string, c *codex.Credentials) error
	ClearCredentials(ctx context.Context, scope Scope, id string) error

	LoadSetting(ctx context.Context, scope Scope, key string, dst any) error
	SaveSetting(ctx context.Context, scope Scope, key string, value any) error
	// HostID returns the install's stable ext_agent_host_id, creating it once.
	HostID(ctx context.Context, scope Scope) (string, error)
}

// NormalizeEfforts trims, lowercases and de-duplicates efforts and orders
// them as provider.ReasoningEfforts does. Unknown values are kept (at the end)
// so Validate can report them.
func NormalizeEfforts(efforts []string) []string {
	if len(efforts) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var unknown []string
	for _, e := range efforts {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		if !provider.ValidReasoningEffort(e) {
			unknown = append(unknown, e)
		}
	}
	out := make([]string, 0, len(seen))
	for _, e := range provider.ReasoningEfforts {
		if seen[e] {
			out = append(out, e)
		}
	}
	out = append(out, unknown...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// DefaultEfforts guesses the efforts for a model id an OpenAI-compatible
// /models list returns, which says nothing about reasoning. The gpt-5 family
// (except chat-tuned ids) and the o-series take low/medium/high; anything
// else gets none. A gateway prefix such as "openai/" is ignored.
func DefaultEfforts(modelID string) []string {
	id := strings.ToLower(modelID)
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if strings.Contains(id, "chat") || strings.Contains(id, "audio") || strings.Contains(id, "realtime") || strings.Contains(id, "search") {
		return nil
	}
	for _, prefix := range []string{"gpt-5", "o1", "o3", "o4"} {
		if strings.HasPrefix(id, prefix) {
			return []string{"low", "medium", "high"}
		}
	}
	return nil
}
