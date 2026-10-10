// Package cqrs is an in-process command/query dispatcher with a pipeline of behaviors
// (logging, validation, authorization, transactions, metrics...). Every use case gets
// the same cross-cutting treatment without repeating it in handlers or HTTP code.
//
//	type PlaceOrder struct{ CustomerID string; Lines []Line }
//	func (PlaceOrder) Permissions() []string { return []string{"orders:create"} }
//	func (c PlaceOrder) Validate() error { ... }
//
//	m := cqrs.New(cqrs.Logging(log), cqrs.Authorization(), cqrs.Validation(), cqrs.Transaction(db))
//	cqrs.Handle(m, func(ctx context.Context, c PlaceOrder) (OrderID, error) { ... })
//	id, err := cqrs.Send[OrderID](ctx, m, PlaceOrder{...})
package cqrs

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/authz"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/logx"
	"github.com/werhani-houssemeddine/go-archkit/validate"
)

// Query marks a request as a read-only query (not wrapped in a transaction).
// Embed it: `type GetOrder struct { cqrs.Query; ID string }`.
type Query struct{}

func (Query) isQuery() {}

type queryMarker interface{ isQuery() }

// Permissioned requests declare the permissions the actor needs.
type Permissioned interface{ Permissions() []string }

// Public requests opt out of the authentication requirement for permissioned pipelines.
type Public interface{ Public() bool }

// Named requests override the name used in logs/metrics (default: Go type name).
type Named interface{ RequestName() string }

// Info describes the request flowing through the pipeline.
type Info struct {
	Name    string
	IsQuery bool
	Request any
}

// Next continues the pipeline.
type Next func(ctx context.Context) (any, error)

// Behavior wraps request handling.
type Behavior func(ctx context.Context, info Info, next Next) (any, error)

type handlerFunc func(ctx context.Context, req any) (any, error)

// Mediator dispatches requests to their single handler through the behaviors.
// Register handlers at startup; Send is safe for concurrent use.
type Mediator struct {
	mu        sync.RWMutex
	handlers  map[reflect.Type]handlerFunc
	behaviors []Behavior
}

// New returns a mediator; behaviors run outermost first.
func New(behaviors ...Behavior) *Mediator {
	return &Mediator{handlers: map[reflect.Type]handlerFunc{}, behaviors: behaviors}
}

// Use appends behaviors.
func (m *Mediator) Use(b ...Behavior) { m.behaviors = append(m.behaviors, b...) }

// Handle registers the handler for request type Req. Registering twice panics (a wiring bug).
func Handle[Req, Res any](m *Mediator, h func(ctx context.Context, req Req) (Res, error)) {
	t := reflect.TypeFor[Req]()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.handlers[t]; dup {
		panic(fmt.Sprintf("cqrs: handler already registered for %v", t))
	}
	m.handlers[t] = func(ctx context.Context, req any) (any, error) { return h(ctx, req.(Req)) }
}

// Send dispatches req and returns the typed result.
func Send[Res any](ctx context.Context, m *Mediator, req any) (Res, error) {
	var zero Res
	t := reflect.TypeOf(req)
	m.mu.RLock()
	h, ok := m.handlers[t]
	m.mu.RUnlock()
	if !ok {
		return zero, errs.Internalf("cqrs: no handler registered for %v", t)
	}
	info := Info{Name: nameOf(req, t), Request: req}
	_, info.IsQuery = req.(queryMarker)

	var call func(i int, ctx context.Context) (any, error)
	call = func(i int, ctx context.Context) (any, error) {
		if i == len(m.behaviors) {
			return h(ctx, req)
		}
		return m.behaviors[i](ctx, info, func(ctx context.Context) (any, error) { return call(i+1, ctx) })
	}
	out, err := call(0, ctx)
	if err != nil {
		return zero, err
	}
	res, ok := out.(Res)
	if !ok && out != nil {
		return zero, errs.Internalf("cqrs: handler for %s returned %T, expected %v", info.Name, out, reflect.TypeFor[Res]())
	}
	return res, nil
}

// Exec dispatches a request whose result is ignored.
func Exec(ctx context.Context, m *Mediator, req any) error {
	_, err := Send[any](ctx, m, req)
	return err
}

func nameOf(req any, t reflect.Type) string {
	if n, ok := req.(Named); ok {
		return n.RequestName()
	}
	return t.String()
}

/* ------------------------------------------------------------------ */
/* Built-in behaviors                                                  */
/* ------------------------------------------------------------------ */

// Logging logs each request with duration and outcome (debug on success, warn on failure).
func Logging(log *slog.Logger) Behavior {
	log = logx.OrDiscard(log)
	return func(ctx context.Context, info Info, next Next) (any, error) {
		start := time.Now()
		out, err := next(ctx)
		attrs := []slog.Attr{slog.String("request", info.Name), slog.Bool("query", info.IsQuery), slog.Duration("duration", time.Since(start))}
		if err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "request failed", append(attrs, logx.Err(err))...)
		} else {
			log.LogAttrs(ctx, slog.LevelDebug, "request handled", attrs...)
		}
		return out, err
	}
}

// Validation calls Validate() on requests implementing validate.Validatable.
func Validation() Behavior {
	return func(ctx context.Context, info Info, next Next) (any, error) {
		if v, ok := info.Request.(validate.Validatable); ok {
			if err := v.Validate(); err != nil {
				return nil, err
			}
		}
		return next(ctx)
	}
}

// Authorization enforces Permissions() declared by requests against the context actor.
func Authorization() Behavior {
	return func(ctx context.Context, info Info, next Next) (any, error) {
		if p, ok := info.Request.(Permissioned); ok {
			if pub, ok := info.Request.(Public); !ok || !pub.Public() {
				if err := authz.Require(ctx, p.Permissions()...); err != nil {
					return nil, err
				}
			}
		}
		return next(ctx)
	}
}

// Transaction wraps commands (not queries) in a unit of work.
func Transaction(uow app.UnitOfWork) Behavior {
	return func(ctx context.Context, info Info, next Next) (any, error) {
		if info.IsQuery {
			return next(ctx)
		}
		var out any
		err := uow.Transact(ctx, func(ctx context.Context) error {
			var err error
			out, err = next(ctx)
			return err
		})
		return out, err
	}
}

// Recover converts panics in handlers into internal errors.
func Recover() Behavior {
	return func(ctx context.Context, info Info, next Next) (out any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = errs.Internalf("panic in %s: %v", info.Name, r)
			}
		}()
		return next(ctx)
	}
}
