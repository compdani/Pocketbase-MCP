package pocketbasemcp

import (
	"fmt"
	"slices"
	"strings"
)

// PolicyFunc is a host-registered policy callback.
type PolicyFunc func(Principal) bool

// ValidatePolicyName checks a policy name against built-ins and registered callbacks.
func ValidatePolicyName(name string, registered map[string]PolicyFunc) error {
	if name == "" || name == "authenticated" || name == "superuser" {
		return nil
	}
	if rest, ok := strings.CutPrefix(name, "collection:"); ok {
		if strings.TrimSpace(rest) == "" {
			return fmt.Errorf("unknown policy %q", name)
		}
		return nil
	}
	if rest, ok := strings.CutPrefix(name, "field:"); ok {
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return fmt.Errorf("unknown policy %q (expected field:<name>:<value>)", name)
		}
		return nil
	}
	if _, ok := registered[name]; ok {
		return nil
	}
	return fmt.Errorf("unknown policy %q", name)
}

// EvaluatePolicy evaluates a built-in or registered policy. Empty means authenticated.
func EvaluatePolicy(name string, p Principal, registered map[string]PolicyFunc) bool {
	if name == "" || name == "authenticated" {
		return p.ID != "" || p.IsSuperuser
	}
	if name == "superuser" {
		return p.IsSuperuser
	}
	if rest, ok := strings.CutPrefix(name, "collection:"); ok {
		return p.CollectionName == rest
	}
	if rest, ok := strings.CutPrefix(name, "field:"); ok {
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 {
			return false
		}
		vals, ok := p.RecordValues[parts[0]]
		if !ok {
			return false
		}
		return slices.Contains(vals, parts[1])
	}
	if fn, ok := registered[name]; ok && fn != nil {
		return fn(p)
	}
	return false
}

// PolicyReason returns a non-sensitive reason when a policy fails.
func PolicyReason(name string) string {
	if name == "" || name == "authenticated" {
		return "requires authentication"
	}
	if name == "superuser" {
		return "requires a superuser"
	}
	if strings.HasPrefix(name, "collection:") {
		return "requires a different auth collection"
	}
	if strings.HasPrefix(name, "field:") {
		return "policy not satisfied"
	}
	return "policy not satisfied"
}
