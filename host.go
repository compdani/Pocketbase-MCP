package pocketbasemcp

import (
	"context"
)

// Principal is the host-neutral authenticated caller. No PocketBase types leak here.
type Principal struct {
	ID             string
	CollectionName string
	CollectionID   string
	IsSuperuser    bool
	// RecordValues holds safe string values of the caller's auth record
	// (text/select and similar) for field:<name>:<value> policies.
	RecordValues map[string][]string `json:"-"`
	// Token is the caller's original PB auth token, forwarded unchanged to
	// record API subrequests. Never logged or serialized.
	Token string `json:"-"`
}

// FieldInfo is the neutral schema description of one collection field.
type FieldInfo struct {
	Name          string
	Type          string // PocketBase field type string, e.g. "text", "select"
	Values        []string
	Multiple      bool
	RelCollection string
	Hidden        bool
}

// RuleState describes a PocketBase operation rule without exposing its expression.
type RuleState int

const (
	RuleNil         RuleState = iota // nil rule: only superusers
	RulePublic                       // empty-string rule: any authenticated caller
	RuleConditional                  // non-empty expression: depends on request/record
)

// ResolvedFilter is one declared search filter argument.
type ResolvedFilter struct {
	Argument string // tool argument name, e.g. "status" or "price__gte"
	Field    string
	Operator string // eq, neq, gt, gte, lt, lte, in, contains
	Info     FieldInfo
}

// CollectionConfig is an immutable snapshot of one collection's MCP surface.
type CollectionConfig struct {
	Name          string // PocketBase collection name
	CollectionID  string
	Alias         string
	Description   string
	Operations    []string
	Policy        string
	TitleFields   []string
	TitleSep      string
	URLField      string
	SummaryFields []string
	QueryFields   []string
	Expand        []string
	Fields        []FieldInfo // safe fields available for schemas (excludes hidden/password)
	FieldMap      map[string]FieldInfo
	AllFieldMap   map[string]FieldInfo // includes hidden/password for validation errors
	SearchFields  []string             // effective query text fields (may include dotted query fields)
	SortFields    []string
	ReadFields    []string
	CreateFields  []string
	UpdateFields  []string
	Filters       []ResolvedFilter
	Relations     map[string]RelationSpec
	DefaultSort   []string
	DefaultLimit  int
	MaxLimit      int
	ListRule      RuleState
	ViewRule      RuleState
	CreateRule    RuleState
	UpdateRule    RuleState
	DeleteRule    RuleState
}

func (c *CollectionConfig) HasOperation(op string) bool {
	if c == nil {
		return false
	}
	for _, o := range c.Operations {
		if o == op {
			return true
		}
	}
	return false
}

func (c *CollectionConfig) RuleFor(op string) RuleState {
	switch op {
	case "search":
		return c.ListRule
	case "fetch", "describe":
		return c.ViewRule
	case "create":
		return c.CreateRule
	case "update":
		return c.UpdateRule
	case "delete":
		return c.DeleteRule
	}
	return RuleNil
}

// ToolError is a user-visible tool failure (returned as isError, not a JSON-RPC error).
type ToolError string

func (e ToolError) Error() string { return string(e) }

// Host is the port adapters implement. All methods are per-server; no globals.
type Host interface {
	// Authenticate validates a raw token (without "Bearer " prefix).
	Authenticate(token string) (Principal, error)
	// LookupCollection resolves a collection name or alias.
	LookupCollection(nameOrAlias string) (*CollectionConfig, bool)
	// VisibleCollections returns configs the principal may see in tools/list
	// (already filtered by host rules / instance visibility).
	VisibleCollections(p Principal) []*CollectionConfig
	// AllCollections returns every registered config in configured order.
	AllCollections() []*CollectionConfig
	Search(ctx context.Context, p Principal, cfg *CollectionConfig, args map[string]any, baseURL string) (any, error)
	Fetch(ctx context.Context, p Principal, cfg *CollectionConfig, args map[string]any, baseURL string) (any, error)
	// Write executes create/update/delete via the host router. operation is "create", "update" or "delete".
	Write(ctx context.Context, p Principal, operation string, cfg *CollectionConfig, args map[string]any, baseURL string) (any, error)
}
