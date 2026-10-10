package events_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/domain"
	"github.com/werhani-houssemeddine/go-archkit/events"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/resilience"
)

type Registered struct{ UserID string }

var UserRegistered = domain.DefineEvent[Registered]("user.registered")

func TestBusIsolationRetryCorrelation(t *testing.T) {
	var mu sync.Mutex
	var seen, dead []string
	attempts := 0
	bus := events.NewBus(events.Options{
		Retry: &resilience.RetryPolicy{Retries: 2, BaseDelay: time.Millisecond},
		OnHandlerError: func(_ context.Context, _ domain.Event, h string, _ error) {
			mu.Lock()
			dead = append(dead, h)
			mu.Unlock()
		},
	})
	events.On(bus, UserRegistered, "welcome", func(ctx context.Context, e domain.Event, p Registered) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, p.UserID+":"+execctx.CorrelationID(ctx)+":"+execctx.CausationID(ctx))
		return nil
	})
	bus.Subscribe(UserRegistered.Type, "mailer", func(context.Context, domain.Event) error { return errors.New("smtp down") })
	bus.Subscribe(UserRegistered.Type, "flaky", func(context.Context, domain.Event) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			return errors.New("transient")
		}
		return nil
	})
	unsub := bus.Subscribe("*", "audit", func(_ context.Context, e domain.Event) error {
		mu.Lock()
		seen = append(seen, "*:"+e.Type)
		mu.Unlock()
		return nil
	})
	ev := UserRegistered.New(execctx.WithCorrelationID(context.Background(), "c-42"), Registered{UserID: "u1"})
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	sort.Strings(seen)
	if len(seen) != 2 || seen[0] != "*:user.registered" || seen[1] != "u1:c-42:"+ev.ID {
		t.Fatalf("seen %v", seen)
	}
	if len(dead) != 1 || dead[0] != "mailer" || attempts != 3 {
		t.Fatalf("dead=%v attempts=%d", dead, attempts)
	}
	unsub()
	if bus.HandlerCount("*") != 0 {
		t.Fatal("unsubscribe")
	}
}

func TestAsyncDrain(t *testing.T) {
	bus := events.NewBus(events.Options{Async: true})
	done := make(chan struct{}, 1)
	bus.Subscribe("x", "h", func(ctx context.Context, e domain.Event) error {
		if ctx.Err() != nil {
			t.Error("async handlers must not inherit request cancellation")
		}
		done <- struct{}{}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	_ = bus.Publish(ctx, domain.NewEvent(ctx, "x", nil))
	cancel()
	bus.Drain()
	if len(done) != 1 {
		t.Fatal("not delivered")
	}
}
