// Package logx builds production-ready log/slog loggers: JSON or text output, level
// control, service metadata, automatic enrichment from execctx (correlation id, actor,
// tenant), redaction of sensitive keys, structured error rendering, fan-out to several
// handlers and a rotating file writer.
//
// Application code depends only on *slog.Logger, the standard library type.
package logx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
)

// Options configures New.
type Options struct {
	Level slog.Leveler // default slog.LevelInfo
	// Format is "json" (default, for log shippers) or "text" (development).
	Format string
	Output io.Writer // default os.Stdout
	// Attrs are added to every record (service, version, env...).
	Attrs []slog.Attr
	// Redact lists attribute keys whose values are replaced with "[REDACTED]"
	// (case-insensitive, any group depth). Defaults to DefaultRedactKeys.
	Redact    []string
	AddSource bool
	// DisableContext turns off execctx enrichment.
	DisableContext bool
}

// DefaultRedactKeys covers common secrets.
var DefaultRedactKeys = []string{
	"password", "passwd", "pwd", "secret", "token", "access_token", "refresh_token", "accesstoken", "refreshtoken",
	"api_key", "apikey", "authorization", "cookie", "set-cookie", "credit_card", "card_number", "cvv", "private_key", "client_secret",
}

// New returns a configured *slog.Logger.
func New(o Options) *slog.Logger {
	return slog.New(NewHandler(o))
}

// NewHandler returns the slog.Handler used by New (useful for composition with Fanout).
func NewHandler(o Options) slog.Handler {
	out := o.Output
	if out == nil {
		out = os.Stdout
	}
	level := o.Level
	if level == nil {
		level = slog.LevelInfo
	}
	redact := o.Redact
	if redact == nil {
		redact = DefaultRedactKeys
	}
	set := make(map[string]struct{}, len(redact))
	for _, k := range redact {
		set[strings.ToLower(k)] = struct{}{}
	}
	hopts := &slog.HandlerOptions{
		Level:     level,
		AddSource: o.AddSource,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if _, ok := set[strings.ToLower(a.Key)]; ok {
				return slog.String(a.Key, "[REDACTED]")
			}
			if a.Value.Kind() == slog.KindAny {
				if err, ok := a.Value.Any().(error); ok {
					return slog.Attr{Key: a.Key, Value: ErrorValue(err)}
				}
			}
			return a
		},
	}
	var h slog.Handler
	if strings.EqualFold(o.Format, "text") {
		h = slog.NewTextHandler(out, hopts)
	} else {
		h = slog.NewJSONHandler(out, hopts)
	}
	if len(o.Attrs) > 0 {
		h = h.WithAttrs(o.Attrs)
	}
	if !o.DisableContext {
		h = ContextHandler{Handler: h}
	}
	return h
}

// ContextHandler adds execctx fields (correlation_id, request_id, actor_id, tenant_id)
// to every record logged with a *Context method (InfoContext, ErrorContext...).
type ContextHandler struct {
	slog.Handler
}

func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		for k, v := range execctx.Fields(ctx) {
			r.AddAttrs(slog.String(k, v))
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithGroup(name)}
}

// Err returns an "err" attribute rendered as a structured group.
func Err(err error) slog.Attr { return slog.Attr{Key: "err", Value: ErrorValue(err)} }

// ErrorValue renders an error as a group: message, plus code/kind/meta for *errs.Error
// and the cause chain (bounded).
func ErrorValue(err error) slog.Value {
	if err == nil {
		return slog.StringValue("<nil>")
	}
	attrs := []slog.Attr{slog.String("message", err.Error())}
	if e, ok := errs.As(err); ok {
		attrs = append(attrs, slog.String("code", e.Code), slog.String("kind", e.Kind.String()))
		if len(e.Meta) > 0 {
			meta := make([]any, 0, len(e.Meta)*2)
			for k, v := range e.Meta {
				meta = append(meta, k, v)
			}
			attrs = append(attrs, slog.Group("meta", meta...))
		}
		if len(e.Issues) > 0 {
			attrs = append(attrs, slog.Any("issues", e.Issues))
		}
	}
	var chain []string
	for c, i := errors.Unwrap(err), 0; c != nil && i < 5; c, i = errors.Unwrap(c), i+1 {
		chain = append(chain, c.Error())
	}
	if len(chain) > 0 {
		attrs = append(attrs, slog.Any("causes", chain))
	}
	return slog.GroupValue(attrs...)
}

// Fanout sends each record to every handler (e.g. stdout JSON + rotating file for warn+).
type Fanout []slog.Handler

func (f Fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f Fanout) Handle(ctx context.Context, r slog.Record) error {
	var errsOut []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errsOut = append(errsOut, err)
			}
		}
	}
	return errors.Join(errsOut...)
}

func (f Fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(Fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f Fanout) WithGroup(name string) slog.Handler {
	out := make(Fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// Discard is a logger that drops everything.
var Discard = slog.New(slog.DiscardHandler)

// OrDiscard returns l, or Discard when l is nil (convenient for optional loggers).
func OrDiscard(l *slog.Logger) *slog.Logger {
	if l == nil {
		return Discard
	}
	return l
}
