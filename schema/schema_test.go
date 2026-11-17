package schema

import (
	"strings"
	"testing"
)

func TestSubset(t *testing.T) {
	cases := []struct {
		s   map[string]any
		v   any
		bad string
	}{
		{map[string]any{"type": "integer"}, 1.5, "type"},
		{map[string]any{"type": "integer"}, 1.0, ""},
		{map[string]any{"type": []string{"null", "string"}}, nil, ""},
		{map[string]any{"type": "string", "minLength": 2}, "😀é", ""},
		{map[string]any{"maxLength": 1}, "😀é", "maxLength"},
		{map[string]any{"exclusiveMinimum": 2}, 2, "exclusiveMinimum"},
		{map[string]any{"minimum": 2, "maximum": 3}, 3, ""},
		{map[string]any{"enum": []any{1, "a"}}, 2, "enum"},
		{map[string]any{"const": nil}, false, "const"},
		{map[string]any{"pattern": "^a+$"}, "b", "pattern"},
		{map[string]any{"format": "date-time"}, "2026-11-17T12:00:00Z", ""},
		{map[string]any{"format": "email"}, "Name <a@b.com>", "email"},
		{map[string]any{"format": "uri"}, "/relative", "uri"},
		{map[string]any{"type": "object", "required": []string{"a/b"}}, map[string]any{}, "/a~1b"},
		{map[string]any{"additionalProperties": false}, map[string]any{"a": 1}, "/a"},
		{map[string]any{"additionalProperties": map[string]any{"type": "boolean"}}, map[string]any{"a": 1}, "/a"},
		{map[string]any{"items": map[string]any{"properties": map[string]any{"x": map[string]any{"type": "integer"}}}}, []any{map[string]any{"x": 1.2}}, "/0/x"},
		{map[string]any{"minItems": 2}, []any{1}, "minItems"},
	}
	for _, c := range cases {
		s, e := Compile(c.s)
		if e != nil {
			t.Fatal(e)
		}
		e = s.Validate(c.v)
		if c.bad == "" && e != nil {
			t.Error(e)
		}
		if c.bad != "" && (e == nil || !strings.Contains(e.Error(), c.bad)) {
			t.Errorf("%v: %v", c, e)
		}
	}
}
func TestNestedAndBoundaries(t *testing.T) {
	cases := []struct {
		s   map[string]any
		v   any
		bad string
	}{
		// Nested arrays of arrays of integers.
		{map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}}, []any{[]any{1, 2}, []any{3}}, ""},
		{map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}}, []any{[]any{1.5}}, "/0/0"},
		// Type arrays: a float is neither integer nor null.
		{map[string]any{"type": []string{"integer", "null"}}, 2.5, "type"},
		{map[string]any{"type": []string{"integer", "null"}}, nil, ""},
		// Exclusive maximum and inclusive boundaries.
		{map[string]any{"exclusiveMaximum": 5}, 5, "exclusiveMaximum"},
		{map[string]any{"exclusiveMaximum": 5}, 4.5, ""},
		{map[string]any{"minimum": 2, "maximum": 3}, 2, ""},
		// Array size boundaries are inclusive.
		{map[string]any{"minItems": 1, "maxItems": 2}, []any{1, 2}, ""},
		{map[string]any{"maxItems": 1}, []any{1, 2}, "maxItems"},
		// Object inside array inside object, pointer path is exact.
		{map[string]any{"type": "object", "properties": map[string]any{"rows": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"qty": map[string]any{"type": "integer", "minimum": 1}}, "required": []string{"qty"}, "additionalProperties": false}}}}, map[string]any{"rows": []any{map[string]any{"qty": 0}}}, "/rows/0/qty"},
		{map[string]any{"type": "object", "properties": map[string]any{"rows": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"qty": map[string]any{"type": "integer"}}, "additionalProperties": false}}}}, map[string]any{"rows": []any{map[string]any{"qty": 1, "extra": true}}}, "/rows/0/extra"},
		// Formats: valid values pass, malformed ones fail.
		{map[string]any{"format": "date-time"}, "2026-11-17T12:00:00+02:00", ""},
		{map[string]any{"format": "date-time"}, "17 Nov 2026", "date-time"},
		{map[string]any{"format": "email"}, "a@b.com", ""},
		{map[string]any{"format": "uri"}, "https://example.com/x?y=1", ""},
		// Enum/const compare numbers after JSON normalization.
		{map[string]any{"enum": []any{1, 2}}, 2, ""},
		{map[string]any{"const": "fixed"}, "fixed", ""},
		// String length counts runes, not bytes.
		{map[string]any{"minLength": 3}, "😀éx", ""},
		{map[string]any{"minLength": 4}, "😀éx", "minLength"},
	}
	for _, c := range cases {
		s, e := Compile(c.s)
		if e != nil {
			t.Fatal(e)
		}
		e = s.Validate(c.v)
		if c.bad == "" && e != nil {
			t.Errorf("%v vs %v: unexpected %v", c.s, c.v, e)
		}
		if c.bad != "" && (e == nil || !strings.Contains(e.Error(), c.bad)) {
			t.Errorf("%v vs %v: got %v, want error containing %q", c.s, c.v, e, c.bad)
		}
	}
	// Unsupported keywords are rejected at any depth.
	for _, s := range []map[string]any{
		{"type": "array", "items": map[string]any{"allOf": []any{}}},
		{"type": "object", "properties": map[string]any{"a": map[string]any{"$dynamicRef": "#x"}}},
		{"type": "string", "format": "hostname"},
	} {
		if _, e := Compile(s); e == nil {
			t.Errorf("accepted unsupported schema %v", s)
		}
	}
}

func TestRejectUnsupported(t *testing.T) {
	for _, s := range []map[string]any{
		{"$ref": "x"}, {"oneOf": []any{}}, {"properties": map[string]any{"a": map[string]any{"not": map[string]any{}}}}, {"additionalProperties": true}, {"pattern": "["}, {"format": "uuid"}, {"type": []string{}}, {"required": []string{"a", "a"}}, {"minLength": -1}, {"items": true},
	} {
		if _, e := Compile(s); e == nil {
			t.Errorf("accepted %v", s)
		}
	}
}
