// Package execctx carries cross-cutting request data through context.Context:
// correlation id, request id, the authenticated actor, tenant and locale.
//
// Loggers, repositories, event publishers and audit trails read it from the context,
// so these values never have to be threaded through function parameters.
package execctx

import (
	"context"
	"slices"

	"github.com/werhani-houssemeddine/go-archkit/ids"
)

// Actor is the authenticated principal performing an operation. Projects put their
// own claims in Attributes.
type Actor struct {
	ID          string         `json:"id"`
	Type        string         `json:"type,omitempty"` // "user", "service", "system"...
	Roles       []string       `json:"roles,omitempty"`
	Permissions []string       `json:"permissions,omitempty"`
	TenantID    string         `json:"tenantId,omitempty"`
	Attributes  map[string]any `json:"attributes,omitempty"`
}

// HasRole reports whether the actor has any of the roles.
func (a *Actor) HasRole(roles ...string) bool {
	if a == nil {
		return false
	}
	for _, r := range roles {
		if slices.Contains(a.Roles, r) {
			return true
		}
	}
	return false
}

type key int

const (
	correlationKey key = iota
	requestKey
	actorKey
	tenantKey
	localeKey
	causationKey
)

// WithCorrelationID returns a context carrying the correlation id (propagated across services).
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey, id)
}

// CorrelationID returns the correlation id or "".
func CorrelationID(ctx context.Context) string { s, _ := ctx.Value(correlationKey).(string); return s }

// EnsureCorrelationID returns ctx unchanged if it has a correlation id, otherwise adds a new one.
func EnsureCorrelationID(ctx context.Context) context.Context {
	if CorrelationID(ctx) != "" {
		return ctx
	}
	return WithCorrelationID(ctx, ids.NewV7().String())
}

// WithRequestID returns a context carrying the request id (unique per inbound request/job).
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestKey, id)
}

// RequestID returns the request id or "".
func RequestID(ctx context.Context) string { s, _ := ctx.Value(requestKey).(string); return s }

// WithCausationID records the id of the message/event that caused the current work.
func WithCausationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, causationKey, id)
}

// CausationID returns the causation id or "".
func CausationID(ctx context.Context) string { s, _ := ctx.Value(causationKey).(string); return s }

// WithActor returns a context carrying the authenticated actor (and its tenant, if any).
func WithActor(ctx context.Context, a *Actor) context.Context {
	ctx = context.WithValue(ctx, actorKey, a)
	if a != nil && a.TenantID != "" && TenantID(ctx) == "" {
		ctx = WithTenantID(ctx, a.TenantID)
	}
	return ctx
}

// ActorFrom returns the actor, or nil for anonymous calls.
func ActorFrom(ctx context.Context) *Actor { a, _ := ctx.Value(actorKey).(*Actor); return a }

// WithTenantID returns a context carrying the tenant id.
func WithTenantID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, tenantKey, id)
}

// TenantID returns the tenant id or "".
func TenantID(ctx context.Context) string { s, _ := ctx.Value(tenantKey).(string); return s }

// WithLocale returns a context carrying the client's locale (e.g. "fr-TN").
func WithLocale(ctx context.Context, locale string) context.Context {
	return context.WithValue(ctx, localeKey, locale)
}

// Locale returns the locale or "".
func Locale(ctx context.Context) string { s, _ := ctx.Value(localeKey).(string); return s }

// Fields returns key/value pairs suitable for log enrichment.
func Fields(ctx context.Context) map[string]string {
	out := map[string]string{}
	if v := CorrelationID(ctx); v != "" {
		out["correlation_id"] = v
	}
	if v := RequestID(ctx); v != "" {
		out["request_id"] = v
	}
	if a := ActorFrom(ctx); a != nil {
		out["actor_id"] = a.ID
	}
	if v := TenantID(ctx); v != "" {
		out["tenant_id"] = v
	}
	return out
}

// Detach returns a new background context that keeps the execctx values of ctx but not
// its cancellation or deadline: use it for work that must outlive the request
// (after-commit hooks, async event handlers).
func Detach(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
