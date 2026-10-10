// Package events provides an in-process event bus implementing app.EventBus.
//
// For cross-service delivery, put a broker adapter (Kafka, RabbitMQ, NATS, SNS...) behind
// the same app.EventPublisher port, fed by the transactional outbox in package pg.
package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/domain"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/logx"
	"github.com/werhani-houssemeddine/go-archkit/resilience"
)

// Options configure a Bus.
type Options struct {
	Logger *slog.Logger
	// Retry is applied to each handler independently (nil = no retries).
	Retry *resilience.RetryPolicy
	// Async makes Publish return immediately; use Drain/Close to wait for delivery.
	Async bool
	// OnHandlerError is called when a handler finally fails (dead letter, alerting).
	OnHandlerError func(ctx context.Context, e domain.Event, handler string, err error)
}

type subscription struct {
	id      uint64
	name    string
	handler app.EventHandler
}

// Bus is an in-memory publish/subscribe bus. Handler failures are isolated: one failing
// handler never affects other handlers or the publisher. Handlers run concurrently and
// receive a context carrying the event's correlation id and the event id as causation id.
type Bus struct {
	opts   Options
	mu     sync.RWMutex
	subs   map[string][]subscription
	nextID atomic.Uint64
	wg     sync.WaitGroup
}

// NewBus creates a bus.
func NewBus(o Options) *Bus {
	o.Logger = logx.OrDiscard(o.Logger)
	return &Bus{opts: o, subs: map[string][]subscription{}}
}

// Subscribe registers h for eventType ("*" = every event). name identifies the handler in logs.
func (b *Bus) Subscribe(eventType, name string, h app.EventHandler) func() {
	id := b.nextID.Add(1)
	if name == "" {
		name = fmt.Sprintf("handler#%d", id)
	}
	b.mu.Lock()
	b.subs[eventType] = append(b.subs[eventType], subscription{id: id, name: name, handler: h})
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		list := b.subs[eventType]
		for i, s := range list {
			if s.id == id {
				b.subs[eventType] = append(list[:i:i], list[i+1:]...)
				return
			}
		}
	}
}

// On subscribes a typed handler for an event definition.
func On[P any](b app.EventSubscriber, def domain.EventDef[P], name string, h func(ctx context.Context, e domain.Event, payload P) error) func() {
	return b.Subscribe(def.Type, name, func(ctx context.Context, e domain.Event) error {
		p, err := def.Decode(e)
		if err != nil {
			return err
		}
		return h(ctx, e, p)
	})
}

// Publish delivers events to their handlers. It never returns handler errors (they are
// logged and passed to OnHandlerError); it only returns ctx errors.
func (b *Bus) Publish(ctx context.Context, evs ...domain.Event) error {
	if b.opts.Async {
		detached := execctx.Detach(ctx)
		for _, e := range evs {
			b.wg.Add(1)
			go func(e domain.Event) {
				defer b.wg.Done()
				b.dispatch(detached, e)
			}(e)
		}
		return nil
	}
	for _, e := range evs {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.dispatch(ctx, e)
	}
	return nil
}

// Drain waits for asynchronous deliveries (call during graceful shutdown).
func (b *Bus) Drain() { b.wg.Wait() }

// HandlerCount returns the number of handlers for eventType.
func (b *Bus) HandlerCount(eventType string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[eventType])
}

func (b *Bus) dispatch(ctx context.Context, e domain.Event) {
	b.mu.RLock()
	handlers := append(append([]subscription(nil), b.subs[e.Type]...), b.subs["*"]...)
	b.mu.RUnlock()
	var wg sync.WaitGroup
	for _, s := range handlers {
		wg.Add(1)
		go func(s subscription) {
			defer wg.Done()
			b.deliver(ctx, e, s)
		}(s)
	}
	wg.Wait()
}

func (b *Bus) deliver(ctx context.Context, e domain.Event, s subscription) {
	hctx := execctx.WithCausationID(ctx, e.ID)
	if c := e.Metadata["correlation_id"]; c != "" {
		hctx = execctx.WithCorrelationID(hctx, c)
	}
	run := func(ctx context.Context) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic in event handler %s: %v", s.name, r)
			}
		}()
		return s.handler(ctx, e)
	}
	var err error
	if b.opts.Retry != nil {
		err = resilience.Retry(hctx, *b.opts.Retry, run)
	} else {
		err = run(hctx)
	}
	if err != nil {
		b.opts.Logger.ErrorContext(hctx, "event handler failed", slog.String("event_type", e.Type), slog.String("event_id", e.ID), slog.String("handler", s.name), logx.Err(err))
		if b.opts.OnHandlerError != nil {
			b.opts.OnHandlerError(hctx, e, s.name, err)
		}
	}
}
