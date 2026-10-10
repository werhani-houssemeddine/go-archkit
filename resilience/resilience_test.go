package resilience_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/resilience"
)

func TestRetry(t *testing.T) {
	n := 0
	err := resilience.Retry(context.Background(), resilience.RetryPolicy{BaseDelay: time.Millisecond}, func(context.Context) error {
		n++
		if n < 3 {
			return errors.New("flaky")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	n = 0
	err = resilience.Retry(context.Background(), resilience.RetryPolicy{BaseDelay: time.Millisecond}, func(context.Context) error {
		n++
		return errs.NotFoundf("nope")
	})
	if n != 1 || !errors.Is(err, errs.NotFound) {
		t.Fatal("non-transient errors are not retried")
	}
	v, err := resilience.RetryValue(context.Background(), resilience.RetryPolicy{Retries: -1}, func(context.Context) (int, error) { return 7, nil })
	if v != 7 || err != nil {
		t.Fatal("RetryValue")
	}
}

func TestCircuitBreaker(t *testing.T) {
	now := time.Unix(0, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	cb := resilience.NewCircuitBreaker(resilience.BreakerOptions{FailureThreshold: 2, ResetTimeout: time.Second, Now: clock})
	fail := func(context.Context) error { return errors.New("down") }
	ctx := context.Background()
	_ = cb.Execute(ctx, fail)
	_ = cb.Execute(ctx, fail)
	if cb.State() != resilience.Open {
		t.Fatal("should be open")
	}
	err := cb.Execute(ctx, func(context.Context) error { return nil })
	if !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("fail fast: %v", err)
	}
	mu.Lock()
	now = now.Add(1500 * time.Millisecond)
	mu.Unlock()
	if err := cb.Execute(ctx, func(context.Context) error { return nil }); err != nil || cb.State() != resilience.Closed {
		t.Fatalf("half-open trial should close: %v %v", err, cb.State())
	}
}

func TestBulkheadAndMapConcurrent(t *testing.T) {
	b := resilience.NewBulkhead(2, 1)
	var active, peak, rejected atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := b.Execute(context.Background(), func(context.Context) error {
				a := active.Add(1)
				for {
					p := peak.Load()
					if a <= p || peak.CompareAndSwap(p, a) {
						break
					}
				}
				time.Sleep(30 * time.Millisecond)
				active.Add(-1)
				return nil
			})
			if errors.Is(err, resilience.ErrBulkheadFull) {
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 2 || rejected.Load() != 1 {
		t.Fatalf("peak=%d rejected=%d", peak.Load(), rejected.Load())
	}
	out, err := resilience.MapConcurrent(context.Background(), []int{3, 1, 2}, 2, func(_ context.Context, x int) (int, error) {
		time.Sleep(time.Duration(x) * time.Millisecond)
		return x * 2, nil
	})
	if err != nil || out[0] != 6 || out[1] != 2 || out[2] != 4 {
		t.Fatalf("order: %v %v", out, err)
	}
}

func TestTokenBucketAndFallback(t *testing.T) {
	now := time.Unix(0, 0)
	tb := &resilience.TokenBucket{Capacity: 2, Refill: 1, Interval: time.Second, Now: func() time.Time { return now }}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if d, _ := tb.Allow(ctx, "ip", 1); !d.Allowed {
			t.Fatal("burst")
		}
	}
	_, err := resilience.Enforce(ctx, tb, "ip", 1)
	if e, ok := errs.As(err); !ok || e.Kind != errs.KindRateLimited || e.RetryAfterSeconds != 1 {
		t.Fatalf("limit: %v", err)
	}
	now = now.Add(time.Second)
	if d, _ := tb.Allow(ctx, "ip", 1); !d.Allowed {
		t.Fatal("refill")
	}

	var calls []string
	send := resilience.Fallback([]resilience.Strategy[string, string]{
		{Name: "secondary", Priority: 2, Run: func(_ context.Context, m string) (string, error) {
			calls = append(calls, "secondary")
			return "s:" + m, nil
		}},
		{Name: "primary", Priority: 1, Run: func(context.Context, string) (string, error) {
			calls = append(calls, "primary")
			return "", errors.New("p down")
		}},
	}, nil)
	if out, err := send(ctx, "hi"); err != nil || out != "s:hi" || len(calls) != 2 || calls[0] != "primary" {
		t.Fatalf("fallback: %v %v %v", out, err, calls)
	}
	allFail := resilience.Fallback([]resilience.Strategy[int, int]{{Name: "a", Run: func(context.Context, int) (int, error) { return 0, errors.New("a") }}}, nil)
	if _, err := allFail(ctx, 1); !errors.Is(err, resilience.ErrAllStrategiesFailed) {
		t.Fatalf("all failed: %v", err)
	}
}

func TestWithTimeout(t *testing.T) {
	err := resilience.WithTimeout(context.Background(), 10*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, errs.Timeout) {
		t.Fatalf("got %v", err)
	}
}
