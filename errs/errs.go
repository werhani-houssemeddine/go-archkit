// Package errs provides one error model for every layer of an application.
//
// Every *Error carries a semantic Kind (what went wrong, independent of the transport),
// a stable machine Code (for clients and i18n), an optional public message, details,
// diagnostic metadata and a wrapped cause. The HTTP layer maps Kind to a status code;
// domain code never knows about HTTP.
//
// Errors work with the standard library: errors.Is matches on Code (and Kind sentinels),
// errors.As extracts *Error, and Unwrap exposes the cause.
package errs

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies an error by meaning, not by transport.
type Kind uint8

const (
	KindUnknown         Kind = iota
	KindValidation           // input is malformed
	KindBusinessRule         // input is well-formed but breaks a domain rule
	KindNotFound             // resource does not exist
	KindConflict             // duplicate, version mismatch, state conflict
	KindUnauthenticated      // who are you?
	KindForbidden            // you can't do that
	KindRateLimited          // slow down
	KindTimeout              // deadline exceeded
	KindUnavailable          // dependency down / circuit open
	KindInfrastructure       // database, network, filesystem failure
	KindInternal             // bug or unexpected state
)

var kindNames = map[Kind]string{
	KindUnknown: "unknown", KindValidation: "validation", KindBusinessRule: "business_rule", KindNotFound: "not_found",
	KindConflict: "conflict", KindUnauthenticated: "unauthenticated", KindForbidden: "forbidden", KindRateLimited: "rate_limited",
	KindTimeout: "timeout", KindUnavailable: "unavailable", KindInfrastructure: "infrastructure", KindInternal: "internal",
}

func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return "unknown"
}

// DefaultCode is used when an error is created without an explicit code.
func (k Kind) DefaultCode() string {
	switch k {
	case KindValidation:
		return "VALIDATION_FAILED"
	case KindBusinessRule:
		return "BUSINESS_RULE_VIOLATION"
	case KindNotFound:
		return "NOT_FOUND"
	case KindConflict:
		return "CONFLICT"
	case KindUnauthenticated:
		return "UNAUTHENTICATED"
	case KindForbidden:
		return "FORBIDDEN"
	case KindRateLimited:
		return "RATE_LIMITED"
	case KindTimeout:
		return "TIMEOUT"
	case KindUnavailable:
		return "SERVICE_UNAVAILABLE"
	case KindInfrastructure:
		return "INFRASTRUCTURE_ERROR"
	default:
		return "INTERNAL_ERROR"
	}
}

// Operational reports whether errors of this kind are expected failures (true) or bugs (false).
func (k Kind) Operational() bool { return k != KindInternal && k != KindUnknown }

// Issue is one validation problem at a path such as "items[2].price".
type Issue struct {
	Path    string         `json:"path"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Params  map[string]any `json:"params,omitempty"`
}

// Error is the library's error type. Create it with New or the helpers below; treat it
// as immutable (the With* methods return copies).
type Error struct {
	Kind    Kind
	Code    string
	Message string
	// Public is a safe-to-display message; when empty, Message is used for operational
	// errors and a generic text for internal ones.
	Public string
	// Details are sent to clients (keep them non-sensitive).
	Details any
	// Meta is diagnostic data for logs only; never sent to clients.
	Meta   map[string]any
	Issues []Issue
	// RetryAfterSeconds hints clients when to retry (rate limits, maintenance).
	RetryAfterSeconds int
	cause             error
	sentinelKind      bool
}

// New creates an error of the given kind. An empty code becomes the kind's default code.
func New(kind Kind, code, message string) *Error {
	if code == "" {
		code = kind.DefaultCode()
	}
	return &Error{Kind: kind, Code: code, Message: message}
}

// Newf is New with fmt formatting for the message.
func Newf(kind Kind, code, format string, args ...any) *Error {
	return New(kind, code, fmt.Sprintf(format, args...))
}

// Wrap creates an error of the given kind that wraps cause.
func Wrap(cause error, kind Kind, code, message string) *Error {
	e := New(kind, code, message)
	e.cause = cause
	return e
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.cause != nil {
		b.WriteString(": ")
		b.WriteString(e.cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.cause }

// Is matches another *Error by Code, so `errors.Is(err, ErrUserNotFound)` works with
// errors created from the same catalog entry, and kind sentinels match by Kind.
func (e *Error) Is(target error) bool {
	var t *Error
	switch v := target.(type) {
	case *Error:
		t = v
	case Def:
		t = v.tmpl
	default:
		return false
	}
	if t == nil {
		return false
	}
	if t.sentinelKind {
		return e.Kind == t.Kind
	}
	return t.Code != "" && e.Code == t.Code
}

func (e *Error) clone() *Error {
	c := *e
	if e.Meta != nil {
		c.Meta = make(map[string]any, len(e.Meta))
		for k, v := range e.Meta {
			c.Meta[k] = v
		}
	}
	c.Issues = append([]Issue(nil), e.Issues...)
	return &c
}

// WithCause returns a copy wrapping cause.
func (e *Error) WithCause(cause error) *Error { c := e.clone(); c.cause = cause; return c }

// WithDetails returns a copy with client-visible details.
func (e *Error) WithDetails(details any) *Error { c := e.clone(); c.Details = details; return c }

// WithPublic returns a copy with a safe-to-display message.
func (e *Error) WithPublic(msg string) *Error { c := e.clone(); c.Public = msg; return c }

// WithMeta returns a copy with diagnostic key/value pairs added (logs only).
func (e *Error) WithMeta(kv ...any) *Error {
	c := e.clone()
	if c.Meta == nil {
		c.Meta = map[string]any{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		c.Meta[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return c
}

// FieldErrors groups issue messages by path: {"email": ["invalid email"]}.
func (e *Error) FieldErrors() map[string][]string {
	out := map[string][]string{}
	for _, is := range e.Issues {
		key := is.Path
		if key == "" {
			key = "_root"
		}
		out[key] = append(out[key], is.Message)
	}
	return out
}

/* ------------------------------------------------------------------ */
/* Kind sentinels: errors.Is(err, errs.NotFound) etc.                  */
/* ------------------------------------------------------------------ */

func kindSentinel(k Kind) *Error { return &Error{Kind: k, Code: k.DefaultCode(), sentinelKind: true} }

// Sentinels match any *Error of the same kind with errors.Is.
var (
	Validation      = kindSentinel(KindValidation)
	BusinessRule    = kindSentinel(KindBusinessRule)
	NotFound        = kindSentinel(KindNotFound)
	Conflict        = kindSentinel(KindConflict)
	Unauthenticated = kindSentinel(KindUnauthenticated)
	Forbidden       = kindSentinel(KindForbidden)
	RateLimited     = kindSentinel(KindRateLimited)
	Timeout         = kindSentinel(KindTimeout)
	Unavailable     = kindSentinel(KindUnavailable)
	Infrastructure  = kindSentinel(KindInfrastructure)
	Internal        = kindSentinel(KindInternal)
)

/* ------------------------------------------------------------------ */
/* Inspection                                                          */
/* ------------------------------------------------------------------ */

// As returns the *Error in err's chain, if any.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// KindOf returns the Kind of the first *Error in the chain. Context deadline/cancel
// map to KindTimeout; anything else unknown maps to KindInternal.
func KindOf(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	if e, ok := As(err); ok {
		return e.Kind
	}
	if isContextErr(err) {
		return KindTimeout
	}
	return KindInternal
}

// CodeOf returns the code of the first *Error in the chain, or "" when none.
func CodeOf(err error) string {
	if e, ok := As(err); ok {
		return e.Code
	}
	return ""
}

// From normalizes any error into an *Error (unknown errors become KindInternal,
// context errors become KindTimeout), preserving the original as cause.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := As(err); ok {
		return e
	}
	if isContextErr(err) {
		return Wrap(err, KindTimeout, "", "operation timed out or was cancelled")
	}
	return Wrap(err, KindInternal, "", "unexpected internal error")
}
