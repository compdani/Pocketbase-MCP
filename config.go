package pocketbasemcp

import (
	"context"
	"log/slog"
	"time"

	"github.com/compdani/pocketbase-mcp/protocol"
)

// AuthConfig controls which PocketBase auth collections' tokens /mcp accepts.
type AuthConfig struct {
	// Collection is the single auth collection that owns OAuth login identity
	// (e.g. "users"). It is also the default for Collections when OAuth is on.
	Collection string
	// Collections lists accepted auth collection names (e.g. ["users"]).
	// Tokens from other collections are rejected with 401. If empty and OAuth
	// is off, every non-superuser token is rejected: accepted collections must
	// be explicit. If empty and OAuth is on, it defaults to [Collection].
	Collections []string
	// AllowSuperusers permits _superusers tokens. Default false.
	AllowSuperusers bool
}

// OAuthMode values for OAuthConfig.Mode.
const (
	OAuthModeAuto     = "auto"
	OAuthModeNative   = "native"
	OAuthModeModule   = "module"
	OAuthModeExternal = "external"
	OAuthModeOff      = "off"
)

// OAuthConfig controls OAuth ownership (plan section 3).
type OAuthConfig struct {
	Enabled bool
	// Mode: auto|native|module|external|off. Empty means auto when Enabled.
	Mode string
	// Collection overrides Auth.Collection for the OAuth login identity.
	Collection string
	// PublicBaseURL is the externally visible base (no trailing slash).
	// Falls back to Config.BaseURL / Config.PublicURL / request host.
	PublicBaseURL string
	// ExternalIssuer is required in external mode.
	ExternalIssuer string
	// IssuerBaseURL overrides, in native (pb-postgress) mode, the base URL of
	// the app that hosts the sign-in (eg. "https://pocket.example.com").
	// Empty uses the collection's OAuth2 server "sign-in app URL" setting,
	// then this app's Application URL.
	IssuerBaseURL string
	// TokenDuration, when >0, issues non-refreshable static auth tokens with
	// this lifetime from the module token endpoint. Zero uses the collection's
	// normal auth token duration.
	TokenDuration time.Duration
	// Endpoint path overrides (stock module mode). Defaults are applied by
	// the stock provider: /oauth/login, /oauth/token, /oauth/userinfo,
	// /oauth/register.
	LoginPath    string
	TokenPath    string
	UserinfoPath string
	RegisterPath string
	// DisableRegistration turns off Dynamic Client Registration; only
	// hand-created clients can sign in.
	DisableRegistration bool
	// RegistrationRedirectHosts, when non-empty, limits self-registered
	// clients to redirect URIs on these hosts (exact host match; loopback is
	// always allowed for native clients). Strongly recommended: tokens are
	// full PocketBase sessions.
	RegistrationRedirectHosts []string
	// IDTokenSigningKey is retained for config compatibility. ID tokens are
	// not issued in v1, so it is unused; never set it to the PB token secret.
	IDTokenSigningKey string
}

// CollectionPreset is the minimal Go config struct for milestone 1
// (the full JSON manifest is milestone 2). It mirrors customjs presets.
type CollectionPreset struct {
	Name          string // PocketBase collection name
	Alias         string // tool-name alias; defaults to Name
	Description   string
	TitleFields   []string
	TitleSep      string
	URLField      string
	SummaryFields []string
	QueryFields   []string // extra dotted "relation.field" free-text fields
	Expand        []string
}

// AliasOrName returns the tool alias for the preset.
func (p CollectionPreset) AliasOrName() string {
	if p.Alias != "" {
		return p.Alias
	}
	return p.Name
}

// ToolSpec is a custom tool registered by Go code (registry hook).
//
// ExecutionLocation "" or "server" means the tool's handler runs here.
// "editor" means the tool is a chat-only draft patch: the model's call becomes
// an editor proposal, approval authorizes the user's browser to apply it
// locally, and the browser reports applied/stale/failed. An editor tool must
// declare Channels "chat", leave Handler nil, leave Approval "", classify as a
// write (which it always is: Effects.Kind may stay empty) and declare an object
// InputSchema that requires the string properties "editorId" and
// "baseRevision". It is offered only when chat proposals are enabled and never
// needs Config.AllowWrites, because it writes nothing on the server.
type ToolSpec struct {
	Name              string
	Title             string
	Description       string
	InputSchema       map[string]any
	OutputSchema      map[string]any
	Annotations       *protocol.ToolAnnotations
	Write             bool // deprecated alias for Effects.Kind == "write"
	Policy            string
	Effects           Effects
	Channels          string
	ExecutionLocation string
	Timeout           time.Duration
	MaxResponseBytes  int64
	// Approval selects the review policy for a write tool offered on chat.
	// "" (the default) means every chat call becomes a proposal the user must
	// approve; "auto" lets the host run it without review when its chat
	// options also enable auto-approval. It never widens AllowWrites, the
	// tool's policy or PocketBase rules, and it is invalid on a read tool or
	// on an editor tool, whose every change needs review.
	Approval string
	Handler  func(ctx context.Context, call CallContext, args map[string]any) (any, error)
	// Validate, when set, checks a call's arguments after InputSchema accepts
	// them: before Handler runs on MCP or chat, and before a chat write or
	// editor call becomes a proposal (and again when a proposal is edited or
	// approved). It is for checks JSON Schema cannot express, such as the
	// operations of a host-defined editor patch. Return Invalid(msg) or a
	// ToolError to give the model a specific reason; any other error is
	// reported as a generic refusal. It must not have side effects.
	Validate func(ctx context.Context, p Principal, args map[string]any) error
	// Summarize, when set, describes a chat proposal for this tool in one line
	// from its validated arguments, for the reviewer (for example "replace
	// block hero-2 and add 1 block"). The server writes it, never the model.
	// The tool's title and whether it changes data or the editor are still
	// shown around it. Empty output falls back to the default summary.
	Summarize func(args map[string]any) string
}

// Config configures a Server. Zero values get documented defaults.
type Effects struct {
	Kind           string // read or write
	ExternalAccess bool
	Idempotent     bool
}
type Config struct {
	MaxToolTimeout          time.Duration
	MaxResponseBytes        int64
	DisableOutputValidation bool
	Logger                  *slog.Logger
	AuditFunc               AuditFunc
	// AuditIncludePayload opts audit events into carrying a redacted copy
	// of the call arguments (keys naming tokens, passwords or secrets are
	// replaced). Default false: no payloads are audited.
	AuditIncludePayload bool

	Path           string // canonical MCP path, default "/mcp"
	AliasPaths     []string
	ServerName     string // default "github.com/compdani/pocketbase-mcp"
	ServerTitle    string
	ServerVersion  string // default "0.1.0"
	Instructions   string
	AllowedOrigins []string // empty = allow any origin
	MaxBodyBytes   int64    // default 1<<20
	ToolMode       string   // deprecated: prefer Manifest.ToolMode; used when Manifest is nil
	AllowWrites    bool     // gates create/update/delete tools
	Auth           AuthConfig
	Manifest       *Manifest
	Collections    []CollectionPreset // deprecated thin conversion to Manifest
	CustomTools    []ToolSpec
	Policies       map[string]PolicyFunc
	BaseURL        string // optional public base URL for record URLs
	PublicURL      string // synonym for the public base URL (OAuth issuer / discovery)
	OAuth          OAuthConfig
}

// EffectiveManifest returns the normalized manifest (from Manifest or converted presets).
func (c Config) EffectiveManifest() Manifest {
	if c.Manifest != nil {
		return c.Manifest.Normalized()
	}
	m := ManifestFromPresets(c.Collections, c.ToolMode, 0)
	return m.Normalized()
}

// EffectiveToolMode returns the manifest tool mode.
func (c Config) EffectiveToolMode() string { return c.EffectiveManifest().ToolMode }

func (c Config) withDefaults() Config {
	if c.MaxToolTimeout <= 0 {
		c.MaxToolTimeout = 30 * time.Second
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = 4 << 20
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Path == "" {
		c.Path = "/mcp"
	}
	if c.ServerName == "" {
		c.ServerName = "github.com/compdani/pocketbase-mcp"
	}
	if c.ServerVersion == "" {
		c.ServerVersion = "0.1.0"
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 1 << 20
	}
	if c.ToolMode == "" {
		c.ToolMode = "per_collection"
	}
	if c.Instructions == "" {
		c.Instructions = "Use describe_collection to see a collection's fields. Search tools return summaries with paging info; call the matching fetch tool for full records."
	}
	return c
}
