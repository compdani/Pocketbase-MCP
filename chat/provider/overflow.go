package provider

import (
	"errors"
	"strings"
)

// ContextOverflowError is implemented by an error that reports a request the
// provider refused because its input does not fit the model's context window.
// The runner retries such a step once with a smaller history instead of
// failing the turn.
type ContextOverflowError interface {
	error
	ContextOverflow() bool
}

// IsContextOverflow reports whether err, or an error it wraps, is a context
// overflow.
func IsContextOverflow(err error) bool {
	var overflow ContextOverflowError
	return errors.As(err, &overflow) && overflow.ContextOverflow()
}

// ErrContextOverflow is a ready-made overflow error for adapters and tests that
// have no richer error to report.
var ErrContextOverflow error = overflowError("provider: the request does not fit the model's context window")

type overflowError string

func (e overflowError) Error() string         { return string(e) }
func (e overflowError) ContextOverflow() bool { return true }

// LooksLikeContextOverflow classifies a provider's error code and message. It
// knows the shapes the bundled adapters see:
//
//   - Anthropic: 400 invalid_request_error, "prompt is too long: N tokens > M maximum";
//   - OpenAI Chat Completions and Responses: code "context_length_exceeded";
//   - OpenAI-compatible gateways and local servers that only word it in the
//     message ("maximum context length", "context window", "too many tokens").
//
// It is deliberately narrow: a false positive only costs one smaller retry,
// but a false negative is the status quo.
func LooksLikeContextOverflow(code, message string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "context_length_exceeded" || code == "context_window_exceeded" {
		return true
	}
	m := strings.ToLower(message)
	for _, needle := range []string{
		"prompt is too long",
		"context_length_exceeded",
		"maximum context length",
		"context length exceeded",
		"exceeds the context window",
		"exceeds the model's context window",
		"input is too long",
	} {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}
