package pocketbasemcp

// FiltersFor returns the declared filters for a collection (legacy fallback included in BuildCollectionConfig).
func FiltersFor(cfg *CollectionConfig) []ResolvedFilter {
	if cfg == nil {
		return nil
	}
	if len(cfg.Filters) > 0 {
		return cfg.Filters
	}
	// fallback for hand-built configs in tests
	var out []ResolvedFilter
	for _, f := range cfg.Fields {
		switch f.Type {
		case "text", "editor", "email", "url", "json":
			out = append(out, ResolvedFilter{Argument: f.Name, Field: f.Name, Operator: "contains", Info: f})
		case "select", "relation", "bool":
			out = append(out, ResolvedFilter{Argument: f.Name, Field: f.Name, Operator: "eq", Info: f})
		case "number":
			out = append(out, ResolvedFilter{Argument: f.Name, Field: f.Name, Operator: "eq", Info: f}, ResolvedFilter{Argument: f.Name + "_min", Field: f.Name, Operator: "gte", Info: f}, ResolvedFilter{Argument: f.Name + "_max", Field: f.Name, Operator: "lte", Info: f})
		case "date", "autodate":
			out = append(out, ResolvedFilter{Argument: f.Name + "_from", Field: f.Name, Operator: "gte", Info: f}, ResolvedFilter{Argument: f.Name + "_to", Field: f.Name, Operator: "lte", Info: f})
		}
	}
	return out
}

func (s ResolvedFilter) MatchDescription() string {
	switch s.Operator {
	case "contains":
		return "contains (case-insensitive)"
	case "gte":
		return ">="
	case "lte":
		return "<="
	case "gt":
		return ">"
	case "lt":
		return "<"
	case "neq":
		return "not equals"
	case "in":
		return "is one of"
	}
	return "equals"
}

// Schema returns the JSON Schema fragment for this filter argument.
func (s ResolvedFilter) Schema() map[string]any {
	f := s.Info
	out := map[string]any{"type": "string"}
	var desc string
	switch s.Operator {
	case "contains":
		desc = s.Field + " contains this text."
		if f.Multiple {
			desc = s.Field + " includes this value."
		}
	case "in":
		out = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		if f.Type == "number" {
			out["items"] = map[string]any{"type": "number"}
		} else if f.Type == "bool" {
			out["items"] = map[string]any{"type": "boolean"}
		}
		desc = s.Field + " is one of the given values."
		if len(f.Values) > 0 {
			out["items"] = map[string]any{"type": "string", "enum": f.Values}
		}
	default:
		switch f.Type {
		case "bool":
			out["type"] = "boolean"
			desc = s.Field + " " + s.MatchDescription() + "."
		case "number":
			out["type"] = "number"
			desc = s.Field + " " + s.MatchDescription() + "."
		case "date", "autodate":
			out["format"] = "date-time"
			desc = s.Field + " " + s.MatchDescription() + " (date or RFC3339 datetime, UTC)."
		case "relation":
			desc = "Id of a related " + f.RelCollection + " record (" + s.Field + " " + s.MatchDescription() + ")."
		default:
			desc = s.Field + " " + s.MatchDescription() + "."
		}
		if len(f.Values) > 0 && f.Type != "relation" {
			out["enum"] = f.Values
		}
	}
	out["description"] = desc
	return out
}

func fieldValueSchema(f FieldInfo) map[string]any {
	switch f.Type {
	case "number":
		return map[string]any{"type": "number", "description": f.Name}
	case "bool":
		return map[string]any{"type": "boolean", "description": f.Name}
	case "date", "autodate":
		return map[string]any{"type": "string", "format": "date-time", "description": f.Name}
	case "select":
		if f.Multiple {
			m := map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": f.Name}
			if len(f.Values) > 0 {
				m["items"] = map[string]any{"type": "string", "enum": f.Values}
			}
			return m
		}
		m := map[string]any{"type": "string", "description": f.Name}
		if len(f.Values) > 0 {
			m["enum"] = f.Values
		}
		return m
	case "relation":
		if f.Multiple {
			return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": f.Name + " (related record ids)"}
		}
		return map[string]any{"type": "string", "description": f.Name + " (related record id)"}
	case "json":
		return map[string]any{"description": f.Name}
	default:
		return map[string]any{"type": "string", "description": f.Name}
	}
}
