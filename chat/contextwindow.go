package chat

import "strings"

// DefaultContextWindow returns the input context window, in tokens, of a
// model the module knows, or 0 when it doesn't. Zero means "unknown": the
// runner then keeps its message-count history bound and never summarizes, so
// a wrong guess can't cut a conversation short.
//
// The table only lists families whose window was checked against the
// vendors' published model tables (November 2026). A gateway may serve a
// smaller window than the vendor; set the catalog model's ContextWindow (or
// RunnerConfig.ContextWindow) to override it.
func DefaultContextWindow(model string) int {
	m := strings.ToLower(strings.TrimSpace(model))
	// Strip a gateway's vendor prefix ("anthropic/claude-…", "openai/gpt-…").
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	// Bedrock-style ids carry an "anthropic." prefix.
	m = strings.TrimPrefix(m, "anthropic.")
	switch {
	case strings.HasPrefix(m, "claude-haiku-4-5"),
		strings.HasPrefix(m, "claude-opus-4-5"), strings.HasPrefix(m, "claude-sonnet-4-5"),
		strings.HasPrefix(m, "claude-opus-4-1"), strings.HasPrefix(m, "claude-opus-4-0"),
		strings.HasPrefix(m, "claude-sonnet-4-0"), strings.HasPrefix(m, "claude-3"):
		return 200_000
	case strings.HasPrefix(m, "claude-fable-"), strings.HasPrefix(m, "claude-mythos-"),
		strings.HasPrefix(m, "claude-opus-4-6"), strings.HasPrefix(m, "claude-opus-4-7"),
		strings.HasPrefix(m, "claude-opus-4-8"), strings.HasPrefix(m, "claude-sonnet-4-6"),
		strings.HasPrefix(m, "claude-opus-5"), strings.HasPrefix(m, "claude-sonnet-5"),
		strings.HasPrefix(m, "claude-haiku-5"):
		return 1_000_000
	case m == "gpt-4o" || strings.HasPrefix(m, "gpt-4o-"):
		return 128_000
	case m == "gpt-4.1" || strings.HasPrefix(m, "gpt-4.1-"):
		return 1_047_576
	case m == "o3" || strings.HasPrefix(m, "o3-") || m == "o4-mini" || strings.HasPrefix(m, "o4-mini-"):
		return 200_000
	case m == "gpt-5" || m == "gpt-5-mini" || m == "gpt-5-nano" || strings.HasPrefix(m, "gpt-5-2025-") ||
		strings.HasPrefix(m, "gpt-5-mini-2025-") || strings.HasPrefix(m, "gpt-5-nano-2025-"):
		// 400k in total, of which 272k may be input.
		return 272_000
	}
	return 0
}
