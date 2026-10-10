// Package resilience protects calls to dependencies: retry with backoff and jitter,
// circuit breaker, bulkhead (concurrency limiter), token-bucket rate limiter, ordered
// fallback and timeouts. All functions honor context cancellation.
package resilience

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/errs"
)

/* ================================================================== */
/* Retry                                                               */
/* ================================================================== */

// RetryPolicy configures Retry. The zero value retries 3 times with exponential backoff
// from 100ms (max 10s) and full jitter, skipping non-transient errors.
type RetryPolicy struct {
	Retries   int           // retries after the first attempt (default 3; use -1 for none)
	BaseDelay time.Duration // default 100ms
	MaxDelay  time.Duration // default 10s
	// Backoff overrides the delay for attempt n (1-based).
	Backoff  func(attempt int) time.Duration
	NoJitter bool
	// RetryIf decides if err is retryable (default IsTransient).
	RetryIf func(err error) bool
	OnRetry func(attempt int, err error, delay time.Duration)
}

// IsTransient reports whether retrying might help: false for validation, business rule,
// not found, conflict, auth errors and context cancellation.
func IsTransient(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	switch errs.KindOf(err) {
	case errs.KindValidation, errs.KindBusinessRule, errs.KindNotFound, errs.KindConflict, errs.KindUnauthenticated, errs.KindForbidden:
		return false
	}
	return true
}

// Delay computes the wait before retry attempt n (1-based).
func (p RetryPolicy) Delay(attempt int) time.Duration {
	base, maxD := p.BaseDelay, p.MaxDelay
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if maxD <= 0 {
		maxD = 10 * time.Second
	}
	var d time.Duration
	if p.Backoff != nil {
		d = p.Backoff(attempt)
	} else {
		d = time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	}
	if d > maxD || d < 0 {
		d = maxD
	}
	if !p.NoJitter && d > 0 {
		d = time.Duration(rand.Int64N(int64(d) + 1))
	}
	return d
}

// Retry runs fn until it succeeds, a non-retryable error occurs, retries are exhausted
// or ctx is done.
func Retry(ctx context.Context, p RetryPolicy, fn func(ctx context.Context) error) error {
	retries := p.Retries
	if retries == 0 {
		retries = 3
	} else if retries < 0 {
		retries = 0
	}
	retryIf := p.RetryIf
	if retryIf == nil {
		retryIf = IsTransient
	}
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if attempt > retries || !retryIf(err) || ctx.Err() != nil {
			return err
		}
		d := p.Delay(attempt)
		if p.OnRetry != nil {
			p.OnRetry(attempt, err, d)
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
	}
}

// RetryValue is Retry for functions returning a value.
func RetryValue[T any](ctx context.Context, p RetryPolicy, fn func(ctx context.Context) (T, error)) (T, error) {
	var out T
	err := Retry(ctx, p, func(ctx context.Context) error {
		v, err := fn(ctx)
		if err == nil {
			out = v
		}
		return err
	})
	return out, err
}

/* ================================================================== */
/* Timeout                                                             */
/* ================================================================== */

// WithTimeout runs fn with a deadline; a deadline overrun becomes errs KindTimeout.
func WithTimeout(ctx context.Context, d time.Duration, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	err := fn(ctx)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errs.Wrap(err, errs.KindTimeout, "", "operation timed out after "+d.String())
	}
	return err
}

/* ================================================================== */
/* Circuit breaker                                                     */
/* ================================================================== */

// State of a circuit breaker.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string { return [...]string{"closed", "open", "half_open"}[s] }

// BreakerOptions configure a CircuitBreaker.
type BreakerOptions struct {
	Name             string
	FailureThreshold int           // consecutive failures to open (default 5)
	ResetTimeout     time.Duration // open duration before probing (default 30s)
	HalfOpenMaxCalls int           // concurrent trial calls when half-open (default 1)
	SuccessThreshold int           // successes in half-open to close (default 1)
	// IsFailure decides which errors count (default: all except non-transient ones).
	IsFailure     func(err error) bool
	OnStateChange func(name string, from, to State)
	Now           func() time.Time
}

// CircuitBreaker fails fast with errs KindUnavailable (CIRCUIT_OPEN) while a dependency
// is failing, then lets limited trial calls decide whether to close.
type CircuitBreaker struct {
	opts      BreakerOptions
	mu        sync.Mutex
	state     State
	failures  int
	successes int
	openedAt  time.Time
	inFlight  int
}

// ErrCircuitOpen is returned (with details) while the circuit is open.
var ErrCircuitOpen = errs.Define(errs.KindUnavailable, "CIRCUIT_OPEN", "circuit %s is open")

// NewCircuitBreaker applies defaults.
func NewCircuitBreaker(o BreakerOptions) *CircuitBreaker {
	if o.Name == "" {
		o.Name = "circuit"
	}
	if o.FailureThreshold <= 0 {
		o.FailureThreshold = 5
	}
	if o.ResetTimeout <= 0 {
		o.ResetTimeout = 30 * time.Second
	}
	if o.HalfOpenMaxCalls <= 0 {
		o.HalfOpenMaxCalls = 1
	}
	if o.SuccessThreshold <= 0 {
		o.SuccessThreshold = 1
	}
	if o.IsFailure == nil {
		o.IsFailure = IsTransient
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &CircuitBreaker{opts: o}
}

// State returns the current state.
func (b *CircuitBreaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refresh()
	return b.state
}

func (b *CircuitBreaker) refresh() {
	if b.state == Open && b.opts.Now().Sub(b.openedAt) >= b.opts.ResetTimeout {
		b.transition(HalfOpen)
	}
}

func (b *CircuitBreaker) transition(to State) {
	if b.state == to {
		return
	}
	from := b.state
	b.state = to
	if to == HalfOpen {
		b.successes = 0
	}
	if b.opts.OnStateChange != nil {
		go b.opts.OnStateChange(b.opts.Name, from, to)
	}
}

// Execute runs fn through the breaker.
func (b *CircuitBreaker) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	b.mu.Lock()
	b.refresh()
	state := b.state
	if state == Open || (state == HalfOpen && b.inFlight >= b.opts.HalfOpenMaxCalls) {
		retryIn := b.opts.ResetTimeout - b.opts.Now().Sub(b.openedAt)
		b.mu.Unlock()
		e := ErrCircuitOpen.Newf(b.opts.Name).WithDetails(map[string]any{"circuit": b.opts.Name})
		e.RetryAfterSeconds = int(math.Ceil(max(retryIn, 0).Seconds()))
		return e
	}
	if state == HalfOpen {
		b.inFlight++
	}
	b.mu.Unlock()

	err := fn(ctx)

	b.mu.Lock()
	defer b.mu.Unlock()
	if state == HalfOpen {
		b.inFlight--
	}
	if err != nil && b.opts.IsFailure(err) {
		b.failures++
		if b.state == HalfOpen || b.failures >= b.opts.FailureThreshold {
			b.openedAt = b.opts.Now()
			b.transition(Open)
		}
	} else if b.state == HalfOpen {
		b.successes++
		if b.successes >= b.opts.SuccessThreshold {
			b.failures = 0
			b.transition(Closed)
		}
	} else {
		b.failures = 0
	}
	return err
}

/* ================================================================== */
/* Bulkhead                                                            */
/* ================================================================== */

// Bulkhead limits concurrent executions; callers wait (up to ctx) for a slot, and are
// rejected immediately when MaxWaiting callers are already queued.
type Bulkhead struct {
	slots   chan struct{}
	waiting chan struct{}
}

// NewBulkhead allows maxConcurrent executions and maxWaiting queued callers (<0 = unbounded).
func NewBulkhead(maxConcurrent, maxWaiting int) *Bulkhead {
	b := &Bulkhead{slots: make(chan struct{}, maxConcurrent)}
	if maxWaiting >= 0 {
		b.waiting = make(chan struct{}, maxWaiting)
	}
	return b
}

// ErrBulkheadFull is returned when the queue is full.
var ErrBulkheadFull = errs.Define(errs.KindUnavailable, "BULKHEAD_FULL", "too many concurrent requests")

// Execute runs fn when a slot is available.
func (b *Bulkhead) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	select {
	case b.slots <- struct{}{}:
	default:
		if b.waiting != nil {
			select {
			case b.waiting <- struct{}{}:
				defer func() { <-b.waiting }()
			default:
				return ErrBulkheadFull.New()
			}
		}
		select {
		case b.slots <- struct{}{}:
		case <-ctx.Done():
			return errs.From(ctx.Err())
		}
	}
	defer func() { <-b.slots }()
	return fn(ctx)
}

/* ================================================================== */
/* Rate limiter                                                        */
/* ================================================================== */

// Decision of a rate limiter.
type Decision struct {
	Allowed    bool
	Remaining  int
	Limit      int
	RetryAfter time.Duration
}

// RateLimiter decides per key. Implement it on Redis for multi-instance deployments.
type RateLimiter interface {
	Allow(ctx context.Context, key string, cost int) (Decision, error)
}

// TokenBucket is an in-memory per-key token bucket (per process).
type TokenBucket struct {
	Capacity int           // burst size
	Refill   int           // tokens added per Interval
	Interval time.Duration // refill period
	MaxKeys  int           // default 100k; least recently used keys are evicted
	Now      func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens  float64
	updated time.Time
}

// Allow consumes cost tokens for key.
func (t *TokenBucket) Allow(_ context.Context, key string, cost int) (Decision, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.Now != nil {
		now = t.Now()
	}
	if t.buckets == nil {
		t.buckets = map[string]*bucket{}
	}
	b, ok := t.buckets[key]
	if !ok {
		maxKeys := t.MaxKeys
		if maxKeys <= 0 {
			maxKeys = 100_000
		}
		if len(t.buckets) >= maxKeys {
			t.evict(now)
		}
		b = &bucket{tokens: float64(t.Capacity), updated: now}
		t.buckets[key] = b
	} else {
		elapsed := now.Sub(b.updated)
		b.tokens = math.Min(float64(t.Capacity), b.tokens+float64(t.Refill)*float64(elapsed)/float64(t.Interval))
		b.updated = now
	}
	if b.tokens >= float64(cost) {
		b.tokens -= float64(cost)
		return Decision{Allowed: true, Remaining: int(b.tokens), Limit: t.Capacity}, nil
	}
	deficit := float64(cost) - b.tokens
	wait := time.Duration(math.Ceil(deficit / float64(t.Refill) * float64(t.Interval)))
	return Decision{Allowed: false, Limit: t.Capacity, RetryAfter: wait}, nil
}

func (t *TokenBucket) evict(now time.Time) {
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(t.buckets))
	for k, b := range t.buckets {
		all = append(all, kv{k, b.updated})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, e := range all[:len(all)/10+1] {
		delete(t.buckets, e.k)
	}
}

// Enforce returns errs.TooManyRequests when the key is over its limit.
func Enforce(ctx context.Context, l RateLimiter, key string, cost int) (Decision, error) {
	d, err := l.Allow(ctx, key, cost)
	if err != nil {
		return d, err
	}
	if !d.Allowed {
		return d, errs.TooManyRequests(int(math.Ceil(d.RetryAfter.Seconds())))
	}
	return d, nil
}

/* ================================================================== */
/* Fallback                                                            */
/* ================================================================== */

// Strategy is one option of a fallback chain.
type Strategy[I, O any] struct {
	Name     string
	Priority int // lower runs first
	Run      func(ctx context.Context, in I) (O, error)
	Enabled  func(in I) bool
}

// StrategyFailure records why a strategy failed.
type StrategyFailure struct {
	Strategy string
	Err      error
}

// ErrAllStrategiesFailed is returned (with every failure joined as cause) when no strategy succeeds.
var ErrAllStrategiesFailed = errs.Define(errs.KindUnavailable, "ALL_STRATEGIES_FAILED", "all %d strategies failed")

// Fallback tries strategies by priority until one succeeds (primary SMS provider →
// secondary → email). Every failure is kept.
func Fallback[I, O any](strategies []Strategy[I, O], onFallback func(name string, err error)) func(ctx context.Context, in I) (O, error) {
	ordered := append([]Strategy[I, O](nil), strategies...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
	return func(ctx context.Context, in I) (O, error) {
		var zero O
		var failures []error
		for _, s := range ordered {
			if s.Enabled != nil && !s.Enabled(in) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return zero, err
			}
			out, err := s.Run(ctx, in)
			if err == nil {
				return out, nil
			}
			failures = append(failures, &strategyError{name: s.Name, err: err})
			if onFallback != nil {
				onFallback(s.Name, err)
			}
		}
		return zero, ErrAllStrategiesFailed.Newf(len(failures)).WithCause(errors.Join(failures...))
	}
}

type strategyError struct {
	name string
	err  error
}

func (e *strategyError) Error() string { return e.name + ": " + e.err.Error() }
func (e *strategyError) Unwrap() error { return e.err }

/* ================================================================== */
/* Concurrency helper                                                  */
/* ================================================================== */

// MapConcurrent applies fn to items with at most n goroutines, preserving order. It
// stops scheduling new work after the first error and returns it.
func MapConcurrent[T, R any](ctx context.Context, items []T, n int, fn func(ctx context.Context, item T) (R, error)) ([]R, error) {
	if n < 1 {
		n = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]R, len(items))
	sem := make(chan struct{}, n)
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i, it := range items {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, it T) {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := fn(ctx, it)
			if err != nil {
				once.Do(func() { firstErr = err; cancel() })
				return
			}
			out[i] = r
		}(i, it)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, ctx.Err()
}
