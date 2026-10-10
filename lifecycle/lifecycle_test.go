package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/lifecycle"
)

func TestShutdownOrderAndIsolation(t *testing.T) {
	var order []string
	m := lifecycle.NewManager(nil)
	m.OnShutdown("db", 90, func(context.Context) error { order = append(order, "db"); return nil })
	m.OnShutdown("http", 10, func(context.Context) error { order = append(order, "http"); return nil })
	m.OnShutdown("bad", 50, func(context.Context) error { return errors.New("x") })
	m.Add(lifecycle.Hook{Name: "slow", Priority: 60, Timeout: 20 * time.Millisecond, Fn: func(ctx context.Context) error {
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
		return ctx.Err()
	}})
	err := m.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "slow") {
		t.Fatalf("err %v", err)
	}
	if strings.Join(order, ",") != "http,db" || !m.IsShuttingDown() {
		t.Fatalf("order %v", order)
	}
	if m.Shutdown(context.Background()) != err {
		t.Fatal("idempotent")
	}
}

func TestHealth(t *testing.T) {
	h := (&lifecycle.Health{}).
		Register(lifecycle.Check{Name: "db", Critical: true, Fn: func(context.Context) error { return nil }}).
		Register(lifecycle.Check{Name: "cache", Fn: func(context.Context) error { return errors.New("redis down") }})
	r := h.Run(context.Background())
	if r.Status != lifecycle.Degraded || r.Checks["cache"].Error != "redis down" {
		t.Fatalf("%+v", r)
	}
	h.Register(lifecycle.Check{Name: "slow", Critical: true, Timeout: 10 * time.Millisecond, Fn: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	if h.Run(context.Background()).Status != lifecycle.Down {
		t.Fatal("critical timeout must be down")
	}
}
