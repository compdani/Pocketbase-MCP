package chat

import (
	"context"
	"encoding/json"
	"time"
)

// ProviderView is the admin-facing summary of one configured provider. It
// never contains an API key, a header value or an OAuth token.
type ProviderView struct {
	ID           string        `json:"id"`
	Label        string        `json:"label"`
	Kind         string        `json:"kind"`
	Enabled      bool          `json:"enabled"`
	BaseURL      string        `json:"baseURL,omitempty"`
	OAuthBaseURL string        `json:"oauthBaseURL,omitempty"`
	WireAPI      string        `json:"wireAPI,omitempty"`
	Models       []ModelOption `json:"models"`
	DefaultModel string        `json:"defaultModel,omitempty"`
	// HeaderNames lists configured header names; values are write-only.
	HeaderNames []string `json:"headerNames,omitempty"`
	// HasExtraBody reports whether gateway-specific body fields are set.
	HasExtraBody bool `json:"hasExtraBody,omitempty"`
	// HasAPIKey and APIKeyHint are the only API-key information ever exposed.
	HasAPIKey  bool   `json:"hasApiKey"`
	APIKeyHint string `json:"apiKeyHint,omitempty"`
	// SignedIn, AccountEmail, Plan and ExpiresAt describe a codex provider.
	SignedIn     bool       `json:"signedIn,omitempty"`
	AccountEmail string     `json:"accountEmail,omitempty"`
	Plan         bool       `json:"plan,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	// LastError is a safe configuration or sign-in error, never a secret.
	LastError string `json:"lastError,omitempty"`
}

// ProviderInput is one create or update request. A nil pointer or nil map
// means "leave unchanged"; an explicitly empty value clears the field. APIKey
// is write-only: nil keeps the stored key, an empty string removes it, and any
// other value replaces it.
type ProviderInput struct {
	ID           string            `json:"id,omitempty"`
	Label        *string           `json:"label,omitempty"`
	Kind         *string           `json:"kind,omitempty"`
	Enabled      *bool             `json:"enabled,omitempty"`
	BaseURL      *string           `json:"baseURL,omitempty"`
	OAuthBaseURL *string           `json:"oauthBaseURL,omitempty"`
	WireAPI      *string           `json:"wireAPI,omitempty"`
	Models       *[]ModelOption    `json:"models,omitempty"`
	DefaultModel *string           `json:"defaultModel,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	ExtraBody    map[string]any    `json:"extraBody,omitempty"`
	APIKey       *string           `json:"apiKey,omitempty"`
}

// ModelOption is one model id and optional display label.
type ModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	// Efforts lists the reasoning efforts the model accepts; empty means none.
	Efforts []string `json:"efforts,omitempty"`
	// ContextWindow is the model's input window in tokens; zero means the
	// built-in default for known model ids.
	ContextWindow int `json:"contextWindow,omitempty"`
}

// ProviderTest is the outcome of a provider reachability/auth check.
type ProviderTest struct {
	OK        bool          `json:"ok"`
	Message   string        `json:"message,omitempty"`
	LatencyMS int64         `json:"latencyMs"`
	Models    []ModelOption `json:"models,omitempty"`
}

// SignInStart is the browser-facing start of a Codex sign-in.
type SignInStart struct {
	AuthURL   string    `json:"authUrl"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AdminError is a safe, administrator-facing failure with an explicit HTTP
// status and wire code. Provider admin implementations return it instead of a
// raw store or OAuth error so secrets and internals never reach the browser.
type AdminError struct {
	Status  int
	Code    string
	Message string
}

func (e *AdminError) Error() string { return e.Message }

// ProviderAdmin is the superuser-only provider management surface. The chat
// handler mounts it only when an authenticator is also configured. Methods
// must return errors that are safe to show to an administrator and never
// include a key or token.
type ProviderAdmin interface {
	ListProviders(ctx context.Context) ([]ProviderView, string, error)
	GetProvider(ctx context.Context, id string) (ProviderView, error)
	SaveProvider(ctx context.Context, id string, in ProviderInput) (ProviderView, error)
	DeleteProvider(ctx context.Context, id string) error

	TestProvider(ctx context.Context, id string) (ProviderTest, error)
	TestProviderConfig(ctx context.Context, in ProviderInput) (ProviderTest, error)
	RefreshProviderModels(ctx context.Context, id string, save bool) ([]ModelOption, error)
	SetDefaultChoice(ctx context.Context, providerID, model string) error

	BeginSignIn(ctx context.Context, id, adminID string) (SignInStart, error)
	CompleteSignIn(ctx context.Context, id, adminID, callbackURL string) (ProviderView, error)
	CancelSignIn(ctx context.Context, id, adminID string) error
	SignOut(ctx context.Context, id string) error
	ImportCredentials(ctx context.Context, id string, raw json.RawMessage) (ProviderView, error)
}

// TitleAdmin is an optional ProviderAdmin extension for the model that titles
// conversations. When the admin implements it, GET /providers reports
// titleChoice and POST /providers/actions/title changes it.
type TitleAdmin interface {
	// TitleChoice reports TitleChoiceAuto, TitleChoiceOff or a choice id.
	TitleChoice(ctx context.Context) (string, error)
	// SetTitleChoice stores the title model: mode TitleChoiceAuto or
	// TitleChoiceOff, or an empty mode with a provider and model.
	SetTitleChoice(ctx context.Context, mode, providerID, model string) error
}
