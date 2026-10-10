// Package httpx is the HTTP presentation layer on net/http (no framework): a router with
// groups and middleware on top of http.ServeMux patterns, typed handlers that bind and
// validate input, RFC 9457 problem+json error responses, and production middleware
// (request context, recovery, access log, authentication, authorization, rate limit,
// timeout, CORS, security headers, body limit) plus health endpoints.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/logx"
)

// DefaultStatus maps error kinds to HTTP status codes.
var DefaultStatus = map[errs.Kind]int{
	errs.KindValidation:      http.StatusBadRequest,
	errs.KindBusinessRule:    http.StatusUnprocessableEntity,
	errs.KindNotFound:        http.StatusNotFound,
	errs.KindConflict:        http.StatusConflict,
	errs.KindUnauthenticated: http.StatusUnauthorized,
	errs.KindForbidden:       http.StatusForbidden,
	errs.KindRateLimited:     http.StatusTooManyRequests,
	errs.KindTimeout:         http.StatusGatewayTimeout,
	errs.KindUnavailable:     http.StatusServiceUnavailable,
	errs.KindInfrastructure:  http.StatusInternalServerError,
	errs.KindInternal:        http.StatusInternalServerError,
}

// Problem is an RFC 9457 problem details body.
type Problem struct {
	Type          string       `json:"type"`
	Title         string       `json:"title"`
	Status        int          `json:"status"`
	Detail        string       `json:"detail,omitempty"`
	Instance      string       `json:"instance,omitempty"`
	Code          string       `json:"code"`
	CorrelationID string       `json:"correlationId,omitempty"`
	Errors        []errs.Issue `json:"errors,omitempty"`
	Details       any          `json:"details,omitempty"`
}

// ErrorMapper turns any error into a problem+json response. It is the single place where
// error kinds become status codes. 5xx responses never expose internal messages,
// details or causes unless Expose is set (development).
type ErrorMapper struct {
	Logger *slog.Logger
	// Expose includes internal messages and details for 5xx (development only).
	Expose bool
	// StatusByCode overrides the status for specific error codes.
	StatusByCode map[string]int
	// TypeBaseURL makes `type` "<base><code>" instead of "about:blank".
	TypeBaseURL string
	// Translate localizes issue messages per request (code + params are always present).
	Translate func(r *http.Request, issue errs.Issue) string
}

var builtinStatusByCode = map[string]int{
	"PAYLOAD_TOO_LARGE":      http.StatusRequestEntityTooLarge,
	"UNSUPPORTED_MEDIA_TYPE": http.StatusUnsupportedMediaType,
	"METHOD_NOT_ALLOWED":     http.StatusMethodNotAllowed,
}

// Status returns the HTTP status for err.
func (m *ErrorMapper) Status(e *errs.Error) int {
	if m != nil {
		if s, ok := m.StatusByCode[e.Code]; ok {
			return s
		}
	}
	if s, ok := builtinStatusByCode[e.Code]; ok {
		return s
	}
	if s, ok := DefaultStatus[e.Kind]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Problem builds the problem body for err.
func (m *ErrorMapper) Problem(r *http.Request, err error) Problem {
	e := errs.From(err)
	status := m.Status(e)
	expose := m != nil && m.Expose
	p := Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Code: e.Code}
	if m != nil && m.TypeBaseURL != "" {
		p.Type = m.TypeBaseURL + e.Code
	}
	switch {
	case e.Public != "":
		p.Detail = e.Public
	case status < 500 || expose:
		p.Detail = e.Message
		if expose && e.Unwrap() != nil {
			p.Detail = e.Error()
		}
	case e.Kind.Operational():
		p.Detail = e.Message
	default:
		p.Code = "INTERNAL_ERROR"
		p.Detail = "an unexpected error occurred"
	}
	if r != nil {
		p.Instance = r.URL.Path
		p.CorrelationID = execctx.CorrelationID(r.Context())
	}
	if len(e.Issues) > 0 {
		p.Errors = make([]errs.Issue, len(e.Issues))
		for i, is := range e.Issues {
			if m != nil && m.Translate != nil && r != nil {
				is.Message = m.Translate(r, is)
			}
			p.Errors[i] = is
		}
	} else if e.Details != nil && (status < 500 || expose) {
		p.Details = e.Details
	}
	return p
}

// Write sends the problem response and logs server errors.
func (m *ErrorMapper) Write(w http.ResponseWriter, r *http.Request, err error) {
	e := errs.From(err)
	p := m.Problem(r, e)
	if m != nil {
		log := logx.OrDiscard(m.Logger)
		ctx := r.Context()
		attrs := []slog.Attr{slog.Int("status", p.Status), slog.String("code", e.Code), slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("route", r.Pattern)}
		switch {
		case p.Status >= 500:
			log.LogAttrs(ctx, slog.LevelError, "request failed", append(attrs, logx.Err(err))...)
		case p.Status == 401 || p.Status == 403 || p.Status == 429:
			log.LogAttrs(ctx, slog.LevelInfo, "request rejected", attrs...)
		default:
			log.LogAttrs(ctx, slog.LevelDebug, "request error", attrs...)
		}
	}
	h := w.Header()
	if e.RetryAfterSeconds > 0 {
		h.Set("Retry-After", strconv.Itoa(e.RetryAfterSeconds))
	}
	if p.Status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", `Bearer realm="api"`)
	}
	h.Set("Content-Type", "application/problem+json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
