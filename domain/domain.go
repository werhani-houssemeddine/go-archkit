// Package domain provides DDD building blocks: aggregate roots with domain events and
// optimistic-concurrency versions, typed event definitions, business rules,
// specifications, repository ports and pagination.
//
// Domain packages should import only this package, errs, ids and clock.
package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/ids"
)

/* ================================================================== */
/* Events                                                              */
/* ================================================================== */

// Event is something meaningful that happened in the domain. Payload is the typed value
// when created in-process, or json.RawMessage after a round-trip through storage /
// a broker; use EventDef.Decode to read it in both cases.
type Event struct {
	ID               string            `json:"id"`
	Type             string            `json:"type"` // "sales.order_placed.v1"
	OccurredAt       time.Time         `json:"occurredAt"`
	AggregateID      string            `json:"aggregateId,omitempty"`
	AggregateType    string            `json:"aggregateType,omitempty"`
	AggregateVersion int               `json:"aggregateVersion,omitempty"`
	Payload          any               `json:"payload"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// EventOption customizes NewEvent.
type EventOption func(*Event)

// ForAggregate sets the aggregate id and type.
func ForAggregate(id, typ string) EventOption {
	return func(e *Event) { e.AggregateID, e.AggregateType = id, typ }
}

// At sets the occurrence time (default now, UTC).
func At(t time.Time) EventOption { return func(e *Event) { e.OccurredAt = t } }

// NewEvent builds an event, capturing correlation id, causation id, actor and tenant
// from the context into Metadata.
func NewEvent(ctx context.Context, typ string, payload any, opts ...EventOption) Event {
	e := Event{ID: ids.NewV7().String(), Type: typ, OccurredAt: time.Now().UTC(), Payload: payload, Metadata: map[string]string{}}
	if ctx != nil {
		for k, v := range map[string]string{
			"correlation_id": execctx.CorrelationID(ctx),
			"causation_id":   execctx.CausationID(ctx),
			"tenant_id":      execctx.TenantID(ctx),
		} {
			if v != "" {
				e.Metadata[k] = v
			}
		}
		if a := execctx.ActorFrom(ctx); a != nil {
			e.Metadata["actor_id"] = a.ID
		}
	}
	for _, o := range opts {
		o(&e)
	}
	return e
}

// EventDef declares a typed event once and reuses it for creation, subscription and decoding.
//
//	var OrderPlaced = domain.DefineEvent[OrderPlacedPayload]("sales.order_placed.v1")
//	o.Record(OrderPlaced.New(ctx, OrderPlacedPayload{...}, domain.ForAggregate(o.ID, "Order")))
//	p, err := OrderPlaced.Decode(event)
type EventDef[P any] struct{ Type string }

// DefineEvent declares a typed event.
func DefineEvent[P any](typ string) EventDef[P] { return EventDef[P]{Type: typ} }

// New creates an event of this type.
func (d EventDef[P]) New(ctx context.Context, payload P, opts ...EventOption) Event {
	return NewEvent(ctx, d.Type, payload, opts...)
}

// Is reports whether e has this definition's type.
func (d EventDef[P]) Is(e Event) bool { return e.Type == d.Type }

// Decode returns the typed payload whether it is in-memory (P / *P) or serialized.
func (d EventDef[P]) Decode(e Event) (P, error) {
	var zero P
	if e.Type != d.Type {
		return zero, fmt.Errorf("domain: event type %q is not %q", e.Type, d.Type)
	}
	switch p := e.Payload.(type) {
	case P:
		return p, nil
	case *P:
		return *p, nil
	case json.RawMessage:
		var out P
		return out, json.Unmarshal(p, &out)
	case []byte:
		var out P
		return out, json.Unmarshal(p, &out)
	default:
		raw, err := json.Marshal(p)
		if err != nil {
			return zero, err
		}
		var out P
		return out, json.Unmarshal(raw, &out)
	}
}

/* ================================================================== */
/* Aggregate root                                                      */
/* ================================================================== */

// Aggregate is implemented by any struct embedding AggregateRoot.
type Aggregate interface {
	Version() int
	MarkPersisted(version int)
	PendingEvents() []Event
	PullEvents() []Event
}

// AggregateRoot is embedded in aggregates. It records domain events to dispatch after
// persistence and carries the version used for optimistic locking.
//
//	type Order struct {
//	    domain.AggregateRoot
//	    ID     string
//	    status Status
//	}
type AggregateRoot struct {
	version int
	events  []Event
}

// Version as loaded from storage (0 = never persisted).
func (a *AggregateRoot) Version() int { return a.version }

// Restore sets the version when rebuilding an aggregate from storage.
func (a *AggregateRoot) Restore(version int) { a.version = version }

// MarkPersisted is called by repositories after a successful save.
func (a *AggregateRoot) MarkPersisted(version int) { a.version = version }

// Record appends a domain event.
func (a *AggregateRoot) Record(e Event) {
	if e.AggregateVersion == 0 {
		e.AggregateVersion = a.version + 1
	}
	a.events = append(a.events, e)
}

// PendingEvents returns recorded events without clearing them.
func (a *AggregateRoot) PendingEvents() []Event { return append([]Event(nil), a.events...) }

// PullEvents returns and clears recorded events.
func (a *AggregateRoot) PullEvents() []Event { ev := a.events; a.events = nil; return ev }

/* ================================================================== */
/* Business rules                                                      */
/* ================================================================== */

// Rule returns a business-rule error when broken is true, nil otherwise.
//
//	if err := domain.Rule(len(o.lines) == 0, "ORDER_EMPTY", "order has no lines"); err != nil { return err }
func Rule(broken bool, code, message string) error {
	if broken {
		return errs.Rule(code, message)
	}
	return nil
}

// FirstBroken returns the first non-nil error (evaluate several rules in order).
func FirstBroken(rules ...error) error {
	for _, r := range rules {
		if r != nil {
			return r
		}
	}
	return nil
}

/* ================================================================== */
/* Specification                                                       */
/* ================================================================== */

// Spec is a named, composable predicate.
type Spec[T any] func(T) bool

// IsSatisfiedBy evaluates the spec.
func (s Spec[T]) IsSatisfiedBy(v T) bool { return s(v) }

// And combines specs with logical AND.
func (s Spec[T]) And(o Spec[T]) Spec[T] { return func(v T) bool { return s(v) && o(v) } }

// Or combines specs with logical OR.
func (s Spec[T]) Or(o Spec[T]) Spec[T] { return func(v T) bool { return s(v) || o(v) } }

// Not negates the spec.
func (s Spec[T]) Not() Spec[T] { return func(v T) bool { return !s(v) } }

// Filter returns the items satisfying the spec.
func (s Spec[T]) Filter(items []T) []T {
	var out []T
	for _, it := range items {
		if s(it) {
			out = append(out, it)
		}
	}
	return out
}

/* ================================================================== */
/* Repository port                                                     */
/* ================================================================== */

// Repository is the collection-like port for aggregates, implemented in infrastructure
// (pg.Repository, testkit.MemoryRepository).
type Repository[A any, ID comparable] interface {
	// Get returns the aggregate or an errs.KindNotFound error.
	Get(ctx context.Context, id ID) (A, error)
	Exists(ctx context.Context, id ID) (bool, error)
	// Save inserts or updates; versioned aggregates are checked for concurrent changes.
	Save(ctx context.Context, a A) error
	Delete(ctx context.Context, a A) error
}
