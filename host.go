package pocketbasemcp

import (
	"context"
	"errors"
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

// AccessError is a user-safe refusal from PocketBase record access. Message
// is what the caller sees (a missing and a hidden record look alike, so
// existence never leaks); Kind is for the server-side audit log only:
// "not_found", "denied", "invalid" or "refused" (see Refused).
type AccessError struct {
	Message string
	Kind    string
}

func (e AccessError) Error() string { return e.Message }

// NotFound reports a missing record (or one outside this app's scope).
func NotFound(msg string) error { return AccessError{Message: msg, Kind: "not_found"} }

// Denied reports a refusal by policy or PocketBase rules.
func Denied(msg string) error { return AccessError{Message: msg, Kind: "denied"} }

// Invalid reports input that PocketBase rejected (validation).
func Invalid(msg string) error { return AccessError{Message: msg, Kind: "invalid"} }

// Refused reports a change the tool refused before writing anything, for a
// reason the user can fix (the host's handler answered 400, say). On MCP it is
// a tool error like Invalid. When it ends an approved chat proposal, the
// proposal goes back to pending with msg as its failure, so the user can edit
// it and approve again instead of starting over.
func Refused(msg string) error { return AccessError{Message: msg, Kind: "refused"} }

// UserMessage returns the caller-safe message of a ToolError or AccessError.
func UserMessage(err error) (string, bool) {
	var ae AccessError
	if errors.As(err, &ae) {
		return ae.Message, true
	}
	var te ToolError
	if errors.As(err, &te) {
		return string(te), true
	}
	return "", false
}

// AuditOutcome maps a tool failure to an audit outcome.
func AuditOutcome(err error) string {
	var ae AccessError
	if errors.As(err, &ae) && ae.Kind != "" {
		return ae.Kind
	}
	return "error"
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
