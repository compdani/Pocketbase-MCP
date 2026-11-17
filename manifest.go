package pocketbasemcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Manifest is the versioned JSON contract (schemaVersion "1").
type Manifest struct {
	SchemaVersion     string               `json:"schemaVersion"`
	ToolMode          string               `json:"toolMode,omitempty"`
	MaxGeneratedTools int                  `json:"maxGeneratedTools,omitempty"`
	Collections       []ManifestCollection `json:"collections"`
}

type ManifestCollection struct {
	Collection   string                  `json:"collection"`
	Alias        string                  `json:"alias,omitempty"`
	Description  string                  `json:"description,omitempty"`
	Optional     bool                    `json:"optional,omitempty"`
	Operations   []string                `json:"operations"`
	Presentation Presentation            `json:"presentation,omitempty"`
	Fields       FieldAllowlists         `json:"fields,omitempty"`
	Search       SearchSpec              `json:"search,omitempty"`
	Relations    map[string]RelationSpec `json:"relations,omitempty"`
	Policy       string                  `json:"policy,omitempty"`
}

type Presentation struct {
	TitleFields   []string `json:"titleFields,omitempty"`
	TitleSep      string   `json:"titleSep,omitempty"`
	URLField      string   `json:"urlField,omitempty"`
	SummaryFields []string `json:"summaryFields,omitempty"`
	QueryFields   []string `json:"queryFields,omitempty"`
	Expand        []string `json:"expand,omitempty"`
}

type FieldAllowlists struct {
	Read   []string `json:"read,omitempty"`
	Create []string `json:"create,omitempty"`
	Update []string `json:"update,omitempty"`
}

type SearchSpec struct {
	TextFields   []string            `json:"textFields,omitempty"`
	Filters      map[string][]string `json:"filters,omitempty"`
	SortFields   []string            `json:"sortFields,omitempty"`
	DefaultSort  []string            `json:"defaultSort,omitempty"`
	DefaultLimit int                 `json:"defaultLimit,omitempty"`
	MaxLimit     int                 `json:"maxLimit,omitempty"`
}

type RelationSpec struct {
	Target   string   `json:"target"`
	Fields   []string `json:"fields"`
	MaxDepth int      `json:"maxDepth,omitempty"`
}

var validOperations = []string{"describe", "search", "fetch", "create", "update", "delete"}
var validOperators = []string{"eq", "neq", "gt", "gte", "lt", "lte", "in", "contains"}
var aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func (c *ManifestCollection) AliasOrCollection() string {
	if c.Alias != "" {
		return c.Alias
	}
	return c.Collection
}

func (m Manifest) Normalized() Manifest {
	out := m
	if out.ToolMode == "" {
		out.ToolMode = "per_collection"
	}
	if out.MaxGeneratedTools == 0 {
		out.MaxGeneratedTools = 40
	}
	if out.Collections == nil {
		out.Collections = []ManifestCollection{}
	}
	cp := make([]ManifestCollection, len(out.Collections))
	copy(cp, out.Collections)
	for i := range cp {
		if cp[i].Alias == "" {
			cp[i].Alias = cp[i].Collection
		}
		if cp[i].Policy == "" {
			cp[i].Policy = "authenticated"
		}
		if cp[i].Search.DefaultLimit == 0 {
			cp[i].Search.DefaultLimit = 20
		}
		if cp[i].Search.MaxLimit == 0 {
			cp[i].Search.MaxLimit = 50
		}
		for k, r := range cp[i].Relations {
			if r.MaxDepth == 0 {
				r.MaxDepth = 1
				cp[i].Relations[k] = r
			}
		}
	}
	out.Collections = cp
	return out
}

// Revision is a stable hash of the normalized manifest.
func (m Manifest) Revision() string {
	b, _ := json.Marshal(m.Normalized())
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

func LoadManifestFile(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(b)
}

// GeneratedToolNames returns generated tool names for a normalized manifest (excluding discovery/custom).
func (m Manifest) GeneratedToolNames() []string {
	n := m.Normalized()
	var out []string
	if n.ToolMode == "generic" {
		for _, op := range []string{"search", "fetch", "create", "update", "delete"} {
			// generic tool exists only if at least one collection declares op
			for _, c := range n.Collections {
				if slices.Contains(c.Operations, op) {
					out = append(out, op)
					break
				}
			}
		}
		return out
	}
	for _, c := range n.Collections {
		a := c.AliasOrCollection()
		for _, op := range []string{"search", "fetch", "create", "update", "delete"} {
			if slices.Contains(c.Operations, op) {
				out = append(out, op+"_"+a)
			}
		}
	}
	return out
}

func (m Manifest) GeneratedCount() int { return len(m.GeneratedToolNames()) }

// ValidateStructural checks manifest-only rules. registeredPolicies and extraToolNames (custom tools) may be nil.
func (m Manifest) ValidateStructural(registeredPolicies map[string]PolicyFunc, extraToolNames []string) error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if m.SchemaVersion != "1" {
		add("manifest: schemaVersion must be \"1\", got %q", m.SchemaVersion)
	}
	mode := m.ToolMode
	if mode == "" {
		mode = "per_collection"
	}
	if mode != "per_collection" && mode != "generic" {
		add("manifest: toolMode must be per_collection or generic, got %q", m.ToolMode)
	}
	if m.MaxGeneratedTools < 0 {
		add("manifest: maxGeneratedTools must not be negative")
	}
	maxTools := m.MaxGeneratedTools
	if maxTools == 0 {
		maxTools = 40
	}
	seenAlias := map[string]string{}
	seenCollectionAlias := map[string]bool{}
	toolNames := map[string]string{"describe_access": "built-in", "describe_collection": "built-in", "search": "generic", "fetch": "generic", "create": "generic", "update": "generic", "delete": "generic", "create_record": "reserved", "update_record": "reserved", "delete_record": "reserved"}
	for _, n := range extraToolNames {
		if prev, dup := toolNames[n]; dup {
			add("manifest: custom tool name %q collides with %s", n, prev)
		}
		toolNames[n] = "custom tool"
	}
	// In per_collection mode generic names are not generated, remove them from collision set except custom check above handled.
	// Generated names computed below will detect alias collisions.
	seenGenerated := map[string]string{}
	for i, c := range m.Collections {
		where := fmt.Sprintf("manifest: collections[%d] (%q)", i, c.Collection)
		if strings.TrimSpace(c.Collection) == "" {
			add("%s: collection is required", where)
			continue
		}
		alias := c.AliasOrCollection()
		if !aliasRe.MatchString(alias) {
			add("%s: alias %q is invalid (letters, digits, _ and - only)", where, alias)
		}
		if prev, dup := seenAlias[alias]; dup {
			add("%s: alias %q collides with collection %q (tool-name collision)", where, alias, prev)
		}
		seenAlias[alias] = c.Collection
		key := c.Collection + "\x00" + alias
		if seenCollectionAlias[key] {
			add("%s: duplicate collection/alias entry", where)
		}
		seenCollectionAlias[key] = true
		if len(c.Operations) == 0 {
			add("%s: operations must list at least one operation", where)
		}
		seenOp := map[string]bool{}
		for _, op := range c.Operations {
			if !slices.Contains(validOperations, op) {
				add("%s: unknown operation %q", where, op)
			}
			if seenOp[op] {
				add("%s: duplicate operation %q", where, op)
			}
			seenOp[op] = true
		}
		// policy
		if err := ValidatePolicyName(c.Policy, registeredPolicies); err != nil {
			add("%s: %v", where, err)
		}
		// search limits
		if c.Search.DefaultLimit < 0 || c.Search.MaxLimit < 0 {
			add("%s: search limits must not be negative", where)
		}
		if c.Search.DefaultLimit > 0 && c.Search.MaxLimit > 0 && c.Search.DefaultLimit > c.Search.MaxLimit {
			add("%s: search.defaultLimit %d exceeds maxLimit %d", where, c.Search.DefaultLimit, c.Search.MaxLimit)
		}
		for field, ops := range c.Search.Filters {
			if field == "" {
				add("%s: search.filters has an empty field name", where)
			}
			if len(ops) == 0 {
				add("%s: search.filters[%q] must list at least one operator", where, field)
			}
			for _, op := range ops {
				if !slices.Contains(validOperators, op) {
					add("%s: search.filters[%q]: unknown operator %q (valid: %s)", where, field, op, strings.Join(validOperators, ", "))
				}
			}
		}
		for relName, rel := range c.Relations {
			if relName == "" {
				add("%s: relations has an empty field name", where)
			}
			if rel.MaxDepth < 0 || rel.MaxDepth > 3 {
				add("%s: relations[%q].maxDepth must be between 1 and 3", where, relName)
			}
			// target/fields checked against schema in BuildCollectionConfig; here require target if fields present check basic
			_ = rel
		}
		// duplicate field lists
		for label, list := range map[string][]string{"fields.read": c.Fields.Read, "fields.create": c.Fields.Create, "fields.update": c.Fields.Update, "presentation.titleFields": c.Presentation.TitleFields, "presentation.summaryFields": c.Presentation.SummaryFields, "search.textFields": c.Search.TextFields, "search.sortFields": c.Search.SortFields} {
			seen := map[string]bool{}
			for _, f := range list {
				if seen[f] {
					add("%s: duplicate entry %q in %s", where, f, label)
				}
				seen[f] = true
			}
		}
	}
	if mode == "per_collection" {
		names := m.Normalized().GeneratedToolNames()
		for _, n := range names {
			if prev, dup := seenGenerated[n]; dup {
				add("manifest: generated tool name %q collides (%s)", n, prev)
			}
			seenGenerated[n] = n
			if prev, ok := toolNames[n]; ok && prev == "custom tool" {
				add("manifest: generated tool name %q collides with %s", n, prev)
			}
			// also alias like "access" could generate describe? search_access etc not colliding with describe_access; ok
		}
		if len(names) > maxTools {
			add("manifest: per_collection mode would generate %d tools, exceeding maxGeneratedTools %d; consider toolMode \"generic\"", len(names), maxTools)
		}
	} else if mode == "generic" {
		for _, n := range extraToolNames {
			if slices.Contains([]string{"search", "fetch", "create", "update", "delete"}, n) {
				add("manifest: custom tool name %q collides with generic generated tool", n)
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ManifestFromPresets converts the milestone-1 presets (thin conversion).
func ManifestFromPresets(presets []CollectionPreset, toolMode string, maxTools int) Manifest {
	m := Manifest{SchemaVersion: "1", ToolMode: toolMode, MaxGeneratedTools: maxTools}
	for _, p := range presets {
		m.Collections = append(m.Collections, ManifestCollection{
			Collection: p.Name, Alias: p.Alias, Description: p.Description,
			Operations:   []string{"describe", "search", "fetch", "create", "update", "delete"},
			Presentation: Presentation{TitleFields: p.TitleFields, TitleSep: p.TitleSep, URLField: p.URLField, SummaryFields: p.SummaryFields, QueryFields: p.QueryFields, Expand: p.Expand},
			Policy:       "authenticated",
		})
		// Relations from Expand and dotted QueryFields. A related field that is
		// searchable must also be readable in the projection (legacy presets
		// showed it in related summaries), so it is added to that relation's fields.
		last := &m.Collections[len(m.Collections)-1]
		for _, e := range p.Expand {
			if last.Relations == nil {
				last.Relations = map[string]RelationSpec{}
			}
			last.Relations[e] = RelationSpec{MaxDepth: 1}
		}
		for _, qf := range p.QueryFields {
			rel, fn, ok := strings.Cut(qf, ".")
			if !ok {
				continue
			}
			if last.Relations == nil {
				last.Relations = map[string]RelationSpec{}
			}
			spec, exists := last.Relations[rel]
			if !exists {
				spec = RelationSpec{MaxDepth: 1}
			}
			if len(spec.Fields) == 0 {
				spec.Fields = []string{"id"}
			}
			if !slices.Contains(spec.Fields, fn) {
				spec.Fields = append(spec.Fields, fn)
			}
			last.Relations[rel] = spec
		}
	}
	return m
}

func OperatorValidForType(op, fieldType string) bool {
	switch fieldType {
	case "bool":
		return op == "eq" || op == "neq"
	case "number", "date", "autodate":
		return slices.Contains([]string{"eq", "neq", "gt", "gte", "lt", "lte", "in"}, op)
	case "text", "editor", "email", "url":
		return slices.Contains([]string{"eq", "neq", "contains", "in"}, op)
	case "select":
		return slices.Contains([]string{"eq", "neq", "in", "contains"}, op)
	case "relation":
		return slices.Contains([]string{"eq", "neq", "in", "contains"}, op)
	case "json":
		return slices.Contains([]string{"eq", "contains"}, op)
	case "file":
		return false
	default:
		return op == "eq" || op == "neq"
	}
}
