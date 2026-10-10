// Package lifecycle coordinates graceful shutdown and health checks.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/logx"
)

/* ================================================================== */
/* Shutdown                                                            */
/* ================================================================== */

// Hook is a shutdown step. Lower Priority runs first: e.g. 10 stop accepting traffic,
// 50 drain workers/relays, 90 close pools.
type Hook struct {
	Name     string
	Priority int
	Timeout  time.Duration
	Fn       func(ctx context.Context) error
}

// Manager runs hooks once, in order, within an overall timeout. Components register
// hooks; nothing deep in the code calls os.Exit.
type Manager struct {
	Logger  *slog.Logger
	Timeout time.Duration // overall budget, default 25s (under the usual 30s k8s grace period)

	mu       sync.Mutex
	hooks    []Hook
	once     sync.Once
	err      error
	stopping chan struct{}
}

// NewManager returns a manager.
func NewManager(log *slog.Logger) *Manager {
	return &Manager{Logger: logx.OrDiscard(log), stopping: make(chan struct{})}
}

// OnShutdown registers a hook.
func (m *Manager) OnShutdown(name string, priority int, fn func(ctx context.Context) error) *Manager {
	m.mu.Lock()
	m.hooks = append(m.hooks, Hook{Name: name, Priority: priority, Fn: fn})
	m.mu.Unlock()
	return m
}

// Add registers a fully specified hook.
func (m *Manager) Add(h Hook) *Manager {
	m.mu.Lock()
	m.hooks = append(m.hooks, h)
	m.mu.Unlock()
	return m
}

// ShuttingDown is closed when shutdown starts (readiness probes should fail from then on).
func (m *Manager) ShuttingDown() <-chan struct{} { return m.stopping }

// IsShuttingDown reports whether shutdown has started.
func (m *Manager) IsShuttingDown() bool {
	select {
	case <-m.stopping:
		return true
	default:
		return false
	}
}

// Shutdown runs every hook (once). Failures are logged and joined; they don't stop other hooks.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.once.Do(func() {
		close(m.stopping)
		total := m.Timeout
		if total <= 0 {
			total = 25 * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, total)
		defer cancel()
		m.mu.Lock()
		hooks := append([]Hook(nil), m.hooks...)
		m.mu.Unlock()
		sort.SliceStable(hooks, func(i, j int) bool { return hooks[i].Priority < hooks[j].Priority })
		var failures []error
		start := time.Now()
		for _, h := range hooks {
			hctx := ctx
			var hcancel context.CancelFunc = func() {}
			if h.Timeout > 0 {
				hctx, hcancel = context.WithTimeout(ctx, h.Timeout)
			}
			err := runHook(hctx, h)
			hcancel()
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", h.Name, err))
				m.Logger.Error("shutdown hook failed", slog.String("hook", h.Name), logx.Err(err))
			} else {
				m.Logger.Debug("shutdown hook done", slog.String("hook", h.Name))
			}
		}
		m.err = errors.Join(failures...)
		m.Logger.Info("shutdown complete", slog.Duration("duration", time.Since(start)), slog.Int("failures", len(failures)))
	})
	return m.err
}

func runHook(ctx context.Context, h Hook) (err error) {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- h.Fn(ctx)
	}()
	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait blocks until SIGINT/SIGTERM (or ctx is done), then runs Shutdown. Typical end of main:
//
//	if err := lc.Wait(context.Background()); err != nil { os.Exit(1) }
func (m *Manager) Wait(ctx context.Context, signals ...os.Signal) error {
	if len(signals) == 0 {
		signals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	}
	sctx, stop := signal.NotifyContext(ctx, signals...)
	defer stop()
	<-sctx.Done()
	m.Logger.Info("shutdown signal received")
	return m.Shutdown(context.WithoutCancel(ctx))
}

/* ================================================================== */
/* Health                                                              */
/* ================================================================== */

// Status of a health check.
type Status string

const (
	Up       Status = "up"
	Degraded Status = "degraded"
	Down     Status = "down"
)

// Check is a dependency health check. Non-critical failures only degrade the report.
type Check struct {
	Name     string
	Critical bool
	Timeout  time.Duration // default 3s
	Fn       func(ctx context.Context) error
}

// CheckResult is one check's outcome.
type CheckResult struct {
	Status     Status `json:"status"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
}

// Report aggregates checks.
type Report struct {
	Status Status                 `json:"status"`
	Time   time.Time              `json:"time"`
	Checks map[string]CheckResult `json:"checks"`
}

// Health runs registered checks concurrently.
type Health struct {
	mu     sync.RWMutex
	checks []Check
}

// Register adds a check.
func (h *Health) Register(c Check) *Health {
	h.mu.Lock()
	h.checks = append(h.checks, c)
	h.mu.Unlock()
	return h
}

// Run executes every check with its timeout.
func (h *Health) Run(ctx context.Context) Report {
	h.mu.RLock()
	checks := append([]Check(nil), h.checks...)
	h.mu.RUnlock()
	results := make([]CheckResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c Check) {
			defer wg.Done()
			to := c.Timeout
			if to <= 0 {
				to = 3 * time.Second
			}
			cctx, cancel := context.WithTimeout(ctx, to)
			defer cancel()
			start := time.Now()
			err := runHook(cctx, Hook{Name: c.Name, Fn: c.Fn})
			r := CheckResult{Status: Up, DurationMs: time.Since(start).Milliseconds()}
			if err != nil {
				r.Status, r.Error = Down, err.Error()
			}
			results[i] = r
		}(i, c)
	}
	wg.Wait()
	rep := Report{Status: Up, Time: time.Now().UTC(), Checks: map[string]CheckResult{}}
	for i, c := range checks {
		r := results[i]
		rep.Checks[c.Name] = r
		if r.Status == Down {
			if c.Critical {
				rep.Status = Down
			} else if rep.Status == Up {
				rep.Status = Degraded
			}
		}
	}
	return rep
}
