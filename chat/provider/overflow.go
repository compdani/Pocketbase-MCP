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

// Failure kinds a provider error can report through FailureKind. They are the
// chat failure codes a turn records, so a client can show a specific message.
const (
	// KindAuth: the provider rejected the credentials (a wrong or revoked
	// key, an expired plan sign-in).
	KindAuth = "provider_auth"
	// KindUsageLimit: the account ran out of quota or hit its plan's usage
	// limit; waiting or switching models helps, retrying at once doesn't.
	KindUsageLimit = "usage_limit"
	// KindRateLimited: too many requests for now.
	KindRateLimited = "rate_limited"
)

// FailureKindError is implemented by a provider error that knows which kind
// of failure it is.
type FailureKindError interface {
	error
	FailureKind() string
}

// FailureKind returns the kind of a provider error (KindAuth, KindUsageLimit,
// KindRateLimited), or "" when it is none of them.
func FailureKind(err error) string {
	var kinded FailureKindError
	if errors.As(err, &kinded) {
		return kinded.FailureKind()
	}
	return ""
}

// ClassifyFailure maps an HTTP status, error code and message to a failure
// kind. Adapters use it for HTTP errors and for errors reported inside a
// stream (status 0).
func ClassifyFailure(status int, code, message string) string {
	text := strings.ToLower(code + " " + message)
	for _, needle := range []string{"insufficient_quota", "usage_limit", "quota", "billing", "credit balance", "usage limit"} {
		if strings.Contains(text, needle) {
			return KindUsageLimit
		}
	}
	switch {
	case status == 401 || status == 403 || strings.Contains(text, "invalid_api_key") || strings.Contains(text, "authentication_error"):
		return KindAuth
	case status == 402:
		return KindUsageLimit
	case status == 429 || strings.Contains(text, "rate_limit"):
		return KindRateLimited
	}
	return ""
}

// AuthError marks a failure to obtain credentials (for example a plan
// sign-in that can no longer be refreshed) as KindAuth.
type AuthError struct{ Err error }

func (e *AuthError) Error() string       { return e.Err.Error() }
func (e *AuthError) Unwrap() error       { return e.Err }
func (e *AuthError) FailureKind() string { return KindAuth }
