package pocketbasemcp

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// SchemaCollection is the host-neutral schema of one PB collection for validation.
type SchemaCollection struct {
	Name       string
	ID         string
	Fields     []FieldInfo // all fields including hidden/password markers
	ListRule   RuleState
	ViewRule   RuleState
	CreateRule RuleState
	UpdateRule RuleState
	DeleteRule RuleState
	Visible    bool
}

// TargetResolver resolves a collection name or ID to its schema.
type TargetResolver func(nameOrID string) (SchemaCollection, bool)

func isTextSearchableType(t string) bool {
	switch t {
	case "text", "editor", "email", "url", "select":
		return true
	}
	return false
}
func isSortableInfo(f FieldInfo) bool {
	switch f.Type {
	case "text", "email", "url", "number", "bool", "date", "autodate":
		return true
	case "select":
		return !f.Multiple
	}
	return false
}
func isWritableInfo(f FieldInfo) bool {
	switch f.Type {
	case "file", "autodate", "password":
		return false
	}
	if f.Hidden || f.Name == "id" {
		return false
	}
	return true
}

// BuildCollectionConfig validates one manifest entry against a resolved schema and builds the snapshot config.
func BuildCollectionConfig(entry ManifestCollection, meta SchemaCollection, resolve TargetResolver) (*CollectionConfig, error) {
	if strings.HasPrefix(meta.Name, "mcp_oauth_") {
		return nil, fmt.Errorf("collection %q is private OAuth storage and cannot be exposed", meta.Name)
	}
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	allMap := map[string]FieldInfo{}
	safeFields := []FieldInfo{}
	safeMap := map[string]FieldInfo{}
	// ensure id pseudo-field
	hasID := false
	for _, f := range meta.Fields {
		allMap[f.Name] = f
		if f.Name == "id" {
			hasID = true
		}
		if f.Hidden || f.Type == "password" {
			continue
		}
		safeFields = append(safeFields, f)
		safeMap[f.Name] = f
	}
	if !hasID {
		idf := FieldInfo{Name: "id", Type: "text"}
		allMap["id"] = idf
		safeMap["id"] = idf
		// id first
		safeFields = append([]FieldInfo{idf}, safeFields...)
	}
	checkField := func(label, name string) (FieldInfo, bool) {
		f, ok := allMap[name]
		if !ok {
			add("collection %q: %s references unknown field %q", meta.Name, label, name)
			return FieldInfo{}, false
		}
		if f.Hidden {
			add("collection %q: %s references hidden field %q", meta.Name, label, name)
			return f, false
		}
		if f.Type == "password" {
			add("collection %q: %s references password field %q which must never be exposed", meta.Name, label, name)
			return f, false
		}
		return f, true
	}
	// defaults for allowlists
	var readFields, createFields, updateFields []string
	if entry.Fields.Read != nil {
		readFields = append([]string(nil), entry.Fields.Read...)
		for _, n := range readFields {
			checkField("fields.read", n)
		}
	} else {
		for _, f := range safeFields {
			readFields = append(readFields, f.Name)
		}
	}
	if entry.Fields.Create != nil {
		createFields = append([]string(nil), entry.Fields.Create...)
		for _, n := range createFields {
			if f, ok := checkField("fields.create", n); ok {
				if f.Type == "file" {
					add("collection %q: fields.create references file field %q (file writes are not supported)", meta.Name, n)
				}
				if f.Type == "autodate" || n == "id" {
					add("collection %q: fields.create references non-writable field %q", meta.Name, n)
				}
			}
		}
	} else {
		for _, f := range safeFields {
			if isWritableInfo(f) {
				createFields = append(createFields, f.Name)
			}
		}
	}
	if entry.Fields.Update != nil {
		updateFields = append([]string(nil), entry.Fields.Update...)
		for _, n := range updateFields {
			if f, ok := checkField("fields.update", n); ok {
				if f.Type == "file" {
					add("collection %q: fields.update references file field %q (file writes are not supported)", meta.Name, n)
				}
				if f.Type == "autodate" || n == "id" {
					add("collection %q: fields.update references non-writable field %q", meta.Name, n)
				}
			}
		}
	} else {
		for _, f := range safeFields {
			if isWritableInfo(f) {
				updateFields = append(updateFields, f.Name)
			}
		}
	}
	readSet := map[string]bool{}
	for _, n := range readFields {
		readSet[n] = true
	}
	// presentation
	for _, n := range entry.Presentation.TitleFields {
		if f, ok := checkField("presentation.titleFields", n); ok && !readSet[n] {
			_ = f
			add("collection %q: presentation.titleFields field %q is not in fields.read", meta.Name, n)
		}
	}
	if entry.Presentation.URLField != "" {
		if _, ok := checkField("presentation.urlField", entry.Presentation.URLField); ok && !readSet[entry.Presentation.URLField] {
			add("collection %q: presentation.urlField field %q is not in fields.read", meta.Name, entry.Presentation.URLField)
		}
	}
	summary := entry.Presentation.SummaryFields
	if summary == nil {
		// default: read fields excluding relations, capped reasonably (all)
		for _, n := range readFields {
			if f := safeMap[n]; f.Type != "relation" {
				summary = append(summary, n)
			}
		}
	} else {
		for _, n := range summary {
			if _, ok := checkField("presentation.summaryFields", n); ok && !readSet[n] {
				add("collection %q: presentation.summaryFields field %q is not in fields.read", meta.Name, n)
			}
		}
	}
	// queryFields (dotted allowed)
	for _, qf := range entry.Presentation.QueryFields {
		if rel, fn, ok := strings.Cut(qf, "."); ok {
			rf, rok := checkField("presentation.queryFields", rel)
			if !rok {
				continue
			}
			if rf.Type != "relation" || rf.RelCollection == "" {
				add("collection %q: presentation.queryFields %q: %q is not a relation field", meta.Name, qf, rel)
				continue
			}
			if resolve == nil {
				continue
			}
			target, tok := resolve(rf.RelCollection)
			if !tok {
				add("collection %q: presentation.queryFields %q: relation target %q not found", meta.Name, qf, rf.RelCollection)
				continue
			}
			found := false
			for _, tf := range target.Fields {
				if tf.Name == fn && !tf.Hidden && tf.Type != "password" {
					found = true
					if !isTextSearchableType(tf.Type) {
						add("collection %q: presentation.queryFields %q: target field is not text-searchable", meta.Name, qf)
					}
				}
			}
			if !found {
				add("collection %q: presentation.queryFields %q: unknown target field %q", meta.Name, qf, fn)
			}
		} else {
			checkField("presentation.queryFields", qf)
		}
	}
	// relations
	relations := map[string]RelationSpec{}
	for name, rel := range entry.Relations {
		f, ok := checkField("relations", name)
		if !ok {
			continue
		}
		if f.Type != "relation" {
			add("collection %q: relations[%q] is not a relation field (type %q)", meta.Name, name, f.Type)
			continue
		}
		targetName := rel.Target
		if targetName == "" {
			targetName = f.RelCollection
		}
		if f.RelCollection != "" && targetName != "" {
			// resolve both to canonical names for comparison
			if resolve != nil {
				if t, tok := resolve(targetName); tok {
					if t.Name != f.RelCollection && t.ID != f.RelCollection && targetName != f.RelCollection {
						// f.RelCollection is canonical name from adapter
						if t.Name != f.RelCollection {
							add("collection %q: relations[%q].target %q does not match relation target %q", meta.Name, name, rel.Target, f.RelCollection)
						}
					}
					// validate relation fields against target
					fields := rel.Fields
					if fields == nil {
						fields = []string{"id"}
						// add title-ish? keep id only default; adapters related summaries add title separately
					}
					tmap := map[string]FieldInfo{}
					for _, tf := range t.Fields {
						tmap[tf.Name] = tf
					}
					if _, has := tmap["id"]; !has {
						tmap["id"] = FieldInfo{Name: "id", Type: "text"}
					}
					for _, fn := range fields {
						tf, exists := tmap[fn]
						if !exists {
							add("collection %q: relations[%q].fields references unknown field %q on target %q", meta.Name, name, fn, t.Name)
						} else if tf.Hidden || tf.Type == "password" {
							add("collection %q: relations[%q].fields references hidden/password field %q", meta.Name, name, fn)
						}
					}
					rel.Target = t.Name
					rel.Fields = fields
				} else {
					add("collection %q: relations[%q].target %q not found", meta.Name, name, targetName)
				}
			}
		} else if targetName == "" {
			add("collection %q: relations[%q].target is required", meta.Name, name)
		}
		if rel.MaxDepth == 0 {
			rel.MaxDepth = 1
		}
		relations[name] = rel
	}
	for _, e := range entry.Presentation.Expand {
		if _, ok := relations[e]; !ok {
			// allow expand without explicit relations entry if field is relation: synthesize
			if f, exists := safeMap[e]; exists && f.Type == "relation" {
				relations[e] = RelationSpec{Target: f.RelCollection, Fields: []string{"id"}, MaxDepth: 1}
			} else {
				add("collection %q: presentation.expand %q has no relations entry", meta.Name, e)
			}
		}
	}
	// search
	var textFields []string
	if entry.Search.TextFields != nil {
		textFields = append([]string(nil), entry.Search.TextFields...)
		for _, n := range textFields {
			if f, ok := checkField("search.textFields", n); ok && !isTextSearchableType(f.Type) {
				add("collection %q: search.textFields field %q type %q is not text-searchable", meta.Name, n, f.Type)
			}
		}
	} else {
		for _, f := range safeFields {
			if isTextSearchableType(f.Type) && readSet[f.Name] {
				textFields = append(textFields, f.Name)
			}
		}
	}
	searchFields := append([]string(nil), textFields...)
	searchFields = append(searchFields, entry.Presentation.QueryFields...)
	// sort fields
	var sortFields []string
	if entry.Search.SortFields != nil {
		sortFields = append([]string(nil), entry.Search.SortFields...)
		for _, n := range sortFields {
			if n == "id" {
				continue
			}
			if f, ok := checkField("search.sortFields", n); ok && !isSortableInfo(f) {
				add("collection %q: search.sortFields field %q type %q is not sortable", meta.Name, n, f.Type)
			}
		}
		if !slices.Contains(sortFields, "id") {
			sortFields = append(sortFields, "id")
		}
	} else {
		sortFields = []string{"id"}
		for _, f := range safeFields {
			if f.Name != "id" && isSortableInfo(f) && readSet[f.Name] {
				sortFields = append(sortFields, f.Name)
			}
		}
	}
	// default sort
	defaultSort := entry.Search.DefaultSort
	if len(defaultSort) == 0 {
		if _, ok := safeMap["updated"]; ok && readSet["updated"] {
			defaultSort = []string{"-updated"}
			if !slices.Contains(sortFields, "updated") {
				sortFields = append(sortFields, "updated")
			}
		} else {
			defaultSort = []string{"-id"}
		}
	} else {
		for _, s := range defaultSort {
			name := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
			if !slices.Contains(sortFields, name) {
				// check existence then report sort-specific error
				if _, exists := allMap[name]; !exists {
					add("collection %q: search.defaultSort references unknown field %q", meta.Name, name)
				} else {
					add("collection %q: search.defaultSort field %q is not in search.sortFields", meta.Name, name)
				}
			}
		}
	}
	// filters
	var filters []ResolvedFilter
	if entry.Search.Filters != nil {
		fields := make([]string, 0, len(entry.Search.Filters))
		for k := range entry.Search.Filters {
			fields = append(fields, k)
		}
		sort.Strings(fields)
		for _, fname := range fields {
			f, ok := checkField("search.filters", fname)
			if !ok {
				continue
			}
			for _, op := range entry.Search.Filters[fname] {
				if !OperatorValidForType(op, f.Type) {
					add("collection %q: search.filters[%q]: operator %q is not valid for field type %q", meta.Name, fname, op, f.Type)
					continue
				}
				arg := fname
				if op != "eq" {
					arg = fname + "__" + op
				}
				filters = append(filters, ResolvedFilter{Argument: arg, Field: fname, Operator: op, Info: f})
			}
		}
	} else {
		// legacy derivation preserving old argument names
		for _, f := range safeFields {
			if f.Name == "id" || !readSet[f.Name] {
				continue
			}
			switch f.Type {
			case "text", "editor", "email", "url", "json":
				filters = append(filters, ResolvedFilter{Argument: f.Name, Field: f.Name, Operator: "contains", Info: f})
			case "select", "relation", "bool":
				filters = append(filters, ResolvedFilter{Argument: f.Name, Field: f.Name, Operator: "eq", Info: f})
			case "number":
				filters = append(filters,
					ResolvedFilter{Argument: f.Name, Field: f.Name, Operator: "eq", Info: f},
					ResolvedFilter{Argument: f.Name + "_min", Field: f.Name, Operator: "gte", Info: f},
					ResolvedFilter{Argument: f.Name + "_max", Field: f.Name, Operator: "lte", Info: f})
			case "date", "autodate":
				filters = append(filters,
					ResolvedFilter{Argument: f.Name + "_from", Field: f.Name, Operator: "gte", Info: f},
					ResolvedFilter{Argument: f.Name + "_to", Field: f.Name, Operator: "lte", Info: f})
			}
		}
	}
	defaultLimit := entry.Search.DefaultLimit
	if defaultLimit == 0 {
		defaultLimit = 20
	}
	maxLimit := entry.Search.MaxLimit
	if maxLimit == 0 {
		maxLimit = 50
	}
	// Search, sort and filter may only use readable fields; otherwise a caller
	// could infer values of fields omitted from output.
	readable := func(n string) bool { return n == "id" || readSet[n] }
	for _, n := range textFields {
		if !readable(n) {
			add("collection %q: search.textFields field %q is not in fields.read", meta.Name, n)
		}
	}
	for _, n := range sortFields {
		if !readable(n) {
			add("collection %q: search.sortFields field %q is not in fields.read", meta.Name, n)
		}
	}
	for _, f := range filters {
		if !readable(f.Field) {
			add("collection %q: search.filters field %q is not in fields.read", meta.Name, f.Field)
		}
	}
	for _, qf := range entry.Presentation.QueryFields {
		rel, fn, dotted := strings.Cut(qf, ".")
		if !readable(rel) {
			add("collection %q: presentation.queryFields %q: %q is not in fields.read", meta.Name, qf, rel)
			continue
		}
		if dotted {
			spec, ok := relations[rel]
			if !ok || !slices.Contains(spec.Fields, fn) {
				add("collection %q: presentation.queryFields %q: %q must be listed in relations[%q].fields", meta.Name, qf, fn, rel)
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	alias := entry.AliasOrCollection()
	cfg := &CollectionConfig{
		Name: meta.Name, CollectionID: meta.ID, Alias: alias, Description: entry.Description,
		Operations: append([]string(nil), entry.Operations...), Policy: entry.Policy,
		TitleFields: entry.Presentation.TitleFields, TitleSep: entry.Presentation.TitleSep, URLField: entry.Presentation.URLField,
		SummaryFields: summary, QueryFields: entry.Presentation.QueryFields, Expand: entry.Presentation.Expand,
		Fields: safeFields, FieldMap: safeMap, AllFieldMap: allMap,
		SearchFields: searchFields, SortFields: sortFields,
		ReadFields: readFields, CreateFields: createFields, UpdateFields: updateFields,
		Filters: filters, Relations: relations, DefaultSort: defaultSort,
		DefaultLimit: defaultLimit, MaxLimit: maxLimit,
		ListRule: meta.ListRule, ViewRule: meta.ViewRule, CreateRule: meta.CreateRule, UpdateRule: meta.UpdateRule, DeleteRule: meta.DeleteRule,
	}
	if cfg.Policy == "" {
		cfg.Policy = "authenticated"
	}
	return cfg, nil
}

func RuleStateFromPtr(r *string) RuleState {
	if r == nil {
		return RuleNil
	}
	if *r == "" {
		return RulePublic
	}
	return RuleConditional
}
