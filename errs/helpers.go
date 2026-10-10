package errs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

func isContextErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

/* ------------------------------------------------------------------ */
/* Convenience constructors                                            */
/* ------------------------------------------------------------------ */

// Invalid returns a validation error with the given issues.
func Invalid(issues ...Issue) *Error {
	msg := "validation failed"
	if len(issues) == 1 {
		if issues[0].Path != "" {
			msg = issues[0].Path + ": " + issues[0].Message
		} else {
			msg = issues[0].Message
		}
	} else if len(issues) > 1 {
		msg = fmt.Sprintf("validation failed with %d issues", len(issues))
	}
	e := New(KindValidation, "", msg)
	e.Issues = issues
	return e
}

// InvalidField is a one-issue validation error.
func InvalidField(path, code, message string) *Error {
	return Invalid(Issue{Path: path, Code: code, Message: message})
}

// Rule returns a business-rule violation with a domain code, e.g. Rule("ORDER_EMPTY", "order has no lines").
func Rule(code, message string) *Error { return New(KindBusinessRule, code, message) }

// NotFoundf returns a not-found error with a formatted message.
func NotFoundf(format string, args ...any) *Error { return Newf(KindNotFound, "", format, args...) }

// ResourceNotFound returns e.g. code USER_NOT_FOUND, message "user '42' not found".
func ResourceNotFound(resource string, id any) *Error {
	e := Newf(KindNotFound, constCase(resource)+"_NOT_FOUND", "%s '%v' not found", resource, id)
	e.Details = map[string]any{"resource": resource, "id": id}
	return e
}

// Duplicate returns a conflict for a unique field, e.g. Duplicate("user", "email").
func Duplicate(resource, field string) *Error {
	e := Newf(KindConflict, "DUPLICATE", "%s with this %s already exists", resource, field)
	e.Details = map[string]any{"resource": resource, "field": field}
	return e
}

// ConcurrentModification signals an optimistic-locking failure.
func ConcurrentModification(resource string, id any) *Error {
	e := Newf(KindConflict, "CONCURRENT_MODIFICATION", "%s was modified by another process", resource)
	e.Details = map[string]any{"resource": resource, "id": id}
	return e
}

// Authentication errors.
var (
	ErrMissingToken       = New(KindUnauthenticated, "MISSING_TOKEN", "authentication token is missing")
	ErrInvalidToken       = New(KindUnauthenticated, "INVALID_TOKEN", "invalid or malformed token")
	ErrExpiredToken       = New(KindUnauthenticated, "EXPIRED_TOKEN", "token has expired")
	ErrInvalidCredentials = New(KindUnauthenticated, "INVALID_CREDENTIALS", "invalid credentials")
	ErrAccountDisabled    = New(KindUnauthenticated, "ACCOUNT_DISABLED", "account is disabled")
	ErrAccountLocked      = New(KindUnauthenticated, "ACCOUNT_LOCKED", "account is locked")
)

// MissingPermissions returns a forbidden error listing the required permissions.
func MissingPermissions(required ...string) *Error {
	e := New(KindForbidden, "INSUFFICIENT_PERMISSIONS", "you do not have permission to perform this action")
	e.Details = map[string]any{"required": required}
	return e
}

// TooManyRequests returns a rate-limit error with a retry hint.
func TooManyRequests(retryAfterSeconds int) *Error {
	e := New(KindRateLimited, "", "too many requests")
	e.RetryAfterSeconds = retryAfterSeconds
	return e
}

// Internalf wraps an unexpected condition (a bug). The message is never shown to clients.
func Internalf(format string, args ...any) *Error { return Newf(KindInternal, "", format, args...) }

/* ------------------------------------------------------------------ */
/* Catalogs                                                            */
/* ------------------------------------------------------------------ */

// Def is a reusable error definition. Calling New/Newf creates an *Error that matches
// the definition with errors.Is.
//
//	var (
//	    ErrEmailInUse  = errs.Define(errs.KindConflict, "ACCOUNT_EMAIL_IN_USE", "email is already in use")
//	    ErrOrderLocked = errs.Define(errs.KindBusinessRule, "ORDER_LOCKED", "order %s is locked")
//	)
//	return ErrOrderLocked.Newf(id)
//	if errors.Is(err, ErrEmailInUse) { ... }
type Def struct {
	tmpl *Error
}

// Define declares a catalog entry; message may contain fmt verbs used by Newf.
func Define(kind Kind, code, message string) Def {
	return Def{tmpl: New(kind, code, message)}
}

// Error makes Def usable as an errors.Is target.
func (d Def) Error() string { return d.tmpl.Error() }

// Code returns the definition's stable code.
func (d Def) Code() string { return d.tmpl.Code }

// Kind returns the definition's kind.
func (d Def) Kind() Kind { return d.tmpl.Kind }

// New returns a fresh *Error for this definition.
func (d Def) New() *Error { return d.tmpl.clone() }

// Newf formats the definition's message with args.
func (d Def) Newf(args ...any) *Error {
	e := d.tmpl.clone()
	e.Message = fmt.Sprintf(d.tmpl.Message, args...)
	return e
}

// Wrap returns a fresh *Error for this definition wrapping cause.
func (d Def) Wrap(cause error) *Error { return d.tmpl.clone().WithCause(cause) }

func constCase(s string) string {
	var b strings.Builder
	prev := rune(0)
	for i, r := range s {
		switch {
		case r == ' ' || r == '-':
			b.WriteByte('_')
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(prev) || unicode.IsDigit(prev)):
			b.WriteByte('_')
			b.WriteRune(r)
		default:
			b.WriteRune(unicode.ToUpper(r))
		}
		prev = r
	}
	return b.String()
}
