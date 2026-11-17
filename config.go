package pocketbasemcp

import (
	"context"

	"github.com/compdani/pocketbase-mcp/protocol"
)

// AuthConfig controls which PocketBase auth collections' tokens /mcp accepts.
type AuthConfig struct {
	// Collections lists accepted auth collection names (e.g. ["users"]).
	// Tokens from other collections are rejected with 401. If empty, every
	// non-superuser token is rejected: accepted collections must be explicit.
	Collections []string
	// AllowSuperusers permits _superusers tokens. Default false.
	AllowSuperusers bool
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
type ToolSpec struct {
	Name         string
	Title        string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Annotations  *protocol.ToolAnnotations
	Write        bool // requires AllowWrites
	Handler      func(ctx context.Context, p Principal, args map[string]any) (any, error)
}

// Config configures a Server. Zero values get documented defaults.
type Config struct {
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
