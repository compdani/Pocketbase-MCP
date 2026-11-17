// Package schema implements the deliberately small MCP JSON Schema subset.
package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Schema struct{ raw map[string]any }

func norm(v any) (any, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var x any
	e = json.Unmarshal(b, &x)
	return x, e
}
func ptr(p, k string) string {
	return p + "/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}
func fail(p, s string) error { return fmt.Errorf("%s: %s", p, s) }
func Compile(raw map[string]any) (*Schema, error) {
	x, e := norm(raw)
	if e != nil {
		return nil, e
	}
	m, ok := x.(map[string]any)
	if !ok {
		return nil, fail("", "schema must be an object")
	}
	if e = check(m, ""); e != nil {
		return nil, e
	}
	return &Schema{m}, nil
}
func check(m map[string]any, p string) error {
	for k, v := range m {
		q := ptr(p, k)
		switch k {
		case "title", "description":
			if _, ok := v.(string); !ok {
				return fail(q, "must be a string")
			}
		case "default", "const":
		case "type":
			ts, ok := v.([]any)
			if !ok {
				ts = []any{v}
			}
			if len(ts) == 0 {
				return fail(q, "empty type array")
			}
			seen := map[string]bool{}
			for _, t := range ts {
				s, ok := t.(string)
				if !ok || !strings.Contains("|object|array|string|number|integer|boolean|null|", "|"+s+"|") || seen[s] {
					return fail(q, "invalid type")
				}
				seen[s] = true
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return fail(q, "must be an object")
			}
			for n, x := range props {
				s, ok := x.(map[string]any)
				if !ok {
					return fail(ptr(q, n), "must be a schema")
				}
				if e := check(s, ptr(q, n)); e != nil {
					return e
				}
			}
		case "items", "additionalProperties":
			if k == "additionalProperties" && v == false {
				continue
			}
			s, ok := v.(map[string]any)
			if !ok {
				return fail(q, "must be a schema (or false for additionalProperties)")
			}
			if e := check(s, q); e != nil {
				return e
			}
		case "required":
			a, ok := v.([]any)
			if !ok {
				return fail(q, "must be a string array")
			}
			seen := map[string]bool{}
			for _, x := range a {
				s, ok := x.(string)
				if !ok || seen[s] {
					return fail(q, "must contain unique strings")
				}
				seen[s] = true
			}
		case "enum":
			a, ok := v.([]any)
			if !ok || len(a) == 0 {
				return fail(q, "must be a nonempty array")
			}
			for i, x := range a {
				for _, y := range a[:i] {
					if reflect.DeepEqual(x, y) {
						return fail(q, "duplicate enum value")
					}
				}
			}
		case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minLength", "maxLength", "minItems", "maxItems":
			n, ok := v.(float64)
			if !ok {
				return fail(q, "must be a number")
			}
			if strings.HasPrefix(k, "minL") || strings.HasPrefix(k, "maxL") || strings.HasSuffix(k, "Items") {
				if n < 0 || math.Trunc(n) != n {
					return fail(q, "must be a nonnegative integer")
				}
			}
		case "pattern":
			s, ok := v.(string)
			if !ok {
				return fail(q, "must be a string")
			}
			if _, e := regexp.Compile(s); e != nil {
				return fail(q, e.Error())
			}
		case "format":
			if v != "date-time" && v != "email" && v != "uri" {
				return fail(q, "unsupported format")
			}
		default:
			return fail(q, "unsupported keyword")
		}
	}
	return nil
}
func (s *Schema) Validate(v any) error {
	x, e := norm(v)
	if e != nil {
		return e
	}
	return validate(s.raw, x, "")
}
func matches(t string, v any) bool {
	switch t {
	case "null":
		return v == nil
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number", "integer":
		n, ok := v.(float64)
		return ok && (t == "number" || math.Trunc(n) == n)
	}
	return false
}
func validate(m map[string]any, v any, p string) error {
	if t, ok := m["type"]; ok {
		ts, ok := t.([]any)
		if !ok {
			ts = []any{t}
		}
		yes := false
		for _, t := range ts {
			yes = yes || matches(t.(string), v)
		}
		if !yes {
			return fail(p, "type mismatch")
		}
	}
	if c, ok := m["const"]; ok && !reflect.DeepEqual(c, v) {
		return fail(p, "does not equal const")
	}
	if a, ok := m["enum"].([]any); ok {
		yes := false
		for _, x := range a {
			yes = yes || reflect.DeepEqual(x, v)
		}
		if !yes {
			return fail(p, "not in enum")
		}
	}
	switch x := v.(type) {
	case map[string]any:
		if a, ok := m["required"].([]any); ok {
			for _, n := range a {
				if _, ok := x[n.(string)]; !ok {
					return fail(ptr(p, n.(string)), "required property missing")
				}
			}
		}
		props, _ := m["properties"].(map[string]any)
		for k, v := range x {
			if s, ok := props[k].(map[string]any); ok {
				if e := validate(s, v, ptr(p, k)); e != nil {
					return e
				}
			} else if a, ok := m["additionalProperties"]; ok {
				if a == false {
					return fail(ptr(p, k), "additional property forbidden")
				}
				if e := validate(a.(map[string]any), v, ptr(p, k)); e != nil {
					return e
				}
			}
		}
	case []any:
		if e := bounds(m, float64(len(x)), p, "minItems", "maxItems"); e != nil {
			return e
		}
		if s, ok := m["items"].(map[string]any); ok {
			for i, v := range x {
				if e := validate(s, v, ptr(p, fmt.Sprint(i))); e != nil {
					return e
				}
			}
		}
	case string:
		if e := bounds(m, float64(utf8.RuneCountInString(x)), p, "minLength", "maxLength"); e != nil {
			return e
		}
		if pat, ok := m["pattern"].(string); ok && !regexp.MustCompile(pat).MatchString(x) {
			return fail(p, "pattern mismatch")
		}
		if f, ok := m["format"].(string); ok {
			valid := true
			switch f {
			case "date-time":
				_, e := time.Parse(time.RFC3339, x)
				valid = e == nil
			case "email":
				a, e := mail.ParseAddress(x)
				valid = e == nil && a.Address == x
			case "uri":
				u, e := url.Parse(x)
				valid = e == nil && u.IsAbs()
			}
			if !valid {
				return fail(p, "invalid "+f)
			}
		}
	case float64:
		if e := bounds(m, x, p, "minimum", "maximum"); e != nil {
			return e
		}
		if n, ok := m["exclusiveMinimum"].(float64); ok && x <= n {
			return fail(p, "exclusiveMinimum violated")
		}
		if n, ok := m["exclusiveMaximum"].(float64); ok && x >= n {
			return fail(p, "exclusiveMaximum violated")
		}
	}
	return nil
}
func bounds(m map[string]any, x float64, p, lo, hi string) error {
	if n, ok := m[lo].(float64); ok && x < n {
		return fail(p, lo+" violated")
	}
	if n, ok := m[hi].(float64); ok && x > n {
		return fail(p, hi+" violated")
	}
	return nil
}
