// Package httperr builds the typed HTTP errors the provider adapters report
// for a non-2xx response.
//
// The error deliberately carries only what a caller can act on: the status
// code, the provider's error type and code when the body names them, and a
// short, sanitized message. The raw body and the credential are never part of
// the message.
package httperr

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxDetail bounds the detail string carried by an Error.
const MaxDetail = 512

// Error is one provider HTTP failure.
//
// Both adapters expose it as their own HTTPError type alias, so a caller can
// test for it without importing this internal package.
type Error struct {
	// Provider is the adapter name that produced the error.
	Provider string
	// StatusCode is the HTTP status of the response.
	StatusCode int
	// Type is the provider's error type, for example "invalid_request_error".
	Type string
	// Code is the provider's error code, for example "rate_limit_exceeded".
	Code string
	// Message is the provider's human-readable message, sanitized and bounded.
	Message string
	// Detail is the raw body when the body was not JSON in a shape we
	// recognize, sanitized and bounded. It is empty otherwise.
	Detail string
}

// New builds an Error from a response status and a (bounded) body. secret, when
// non-empty, is redacted from everything kept in the error.
func New(providerName string, status int, body []byte, secret string) *Error {
	e := &Error{Provider: providerName, StatusCode: status}
	parseBody(body, e)
	sanitize(e, secret)
	return e
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(e.Provider)
		b.WriteString(": ")
	}
	fmt.Fprintf(&b, "http %d", e.StatusCode)
	if e.Type != "" {
		b.WriteString(" (")
		b.WriteString(e.Type)
		if e.Code != "" {
			b.WriteString("/")
			b.WriteString(e.Code)
		}
		b.WriteString(")")
	} else if e.Code != "" {
		b.WriteString(" (")
		b.WriteString(e.Code)
		b.WriteString(")")
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Retryable reports whether the same request may succeed later without
// changing it: throttling (429), request timeout (408) and server failures
// (5xx). It is advisory; the adapters never retry on their own.
func (e *Error) Retryable() bool {
	return e.StatusCode == 408 || e.StatusCode == 429 || e.StatusCode >= 500
}

// parseBody extracts the provider's error object from a response body. Both
// wire formats nest it under "error", as an object or, for some gateways, as
// a string.
func parseBody(body []byte, e *Error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		e.Detail = trimmed
		return
	}

	if raw, ok := top["error"]; ok {
		if parseErrorValue(raw, e) {
			return
		}
	}
	// Some gateways report the fields at the top level.
	parseErrorValue(body, e)
	if e.Message == "" && e.Type == "" && e.Code == "" {
		e.Detail = trimmed
	}
}

func parseErrorValue(raw json.RawMessage, e *Error) bool {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString == "" {
			return false
		}
		e.Message = asString
		return true
	}

	var obj struct {
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	if obj.Type != "" {
		e.Type = obj.Type
	}
	e.Code = rawCode(obj.Code)
	if obj.Message != "" {
		e.Message = obj.Message
	}
	return e.Type != "" || e.Code != "" || e.Message != ""
}

// rawCode renders a code that may be a string, a number or absent.
func rawCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	return ""
}

// sanitize removes the credential and bounds every retained string.
func sanitize(e *Error, secret string) {
	clean := func(s string) string {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
		s = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == '\t' {
				return ' '
			}
			return r
		}, s)
		s = strings.TrimSpace(s)
		if len(s) > MaxDetail {
			cut := MaxDetail
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			s = s[:cut] + "…"
		}
		return s
	}
	e.Type = clean(e.Type)
	e.Code = clean(e.Code)
	e.Message = clean(e.Message)
	e.Detail = clean(e.Detail)
}
