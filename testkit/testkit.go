// Package testkit provides fakes for fast, deterministic unit tests of use cases and
// handlers: an in-memory repository with optimistic locking, a unit of work, a
// recording publisher, a mailer, a password hasher and a token service.
package testkit

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/domain"
	"github.com/werhani-houssemeddine/go-archkit/errs"
)

// MemoryRepository implements domain.Repository in memory with the same optimistic
// concurrency semantics as pg.Repository, and records dispatched events.
type MemoryRepository[A any, ID comparable] struct {
	Resource  string
	IDOf      func(A) ID
	Publisher app.EventPublisher

	mu       sync.Mutex
	items    map[ID]A
	versions map[ID]int
	Events   []domain.Event
}

// NewMemoryRepository creates a repository; idOf extracts the aggregate id.
func NewMemoryRepository[A any, ID comparable](resource string, idOf func(A) ID) *MemoryRepository[A, ID] {
	return &MemoryRepository[A, ID]{Resource: resource, IDOf: idOf, items: map[ID]A{}, versions: map[ID]int{}}
}

var _ domain.Repository[any, string] = (*MemoryRepository[any, string])(nil)

func (r *MemoryRepository[A, ID]) Get(_ context.Context, id ID) (A, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.items[id]
	if !ok {
		var zero A
		return zero, errs.ResourceNotFound(r.Resource, id)
	}
	return a, nil
}

func (r *MemoryRepository[A, ID]) Exists(_ context.Context, id ID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.items[id]
	return ok, nil
}

func (r *MemoryRepository[A, ID]) Save(ctx context.Context, a A) error {
	r.mu.Lock()
	id := r.IDOf(a)
	var events []domain.Event
	if agg, ok := any(a).(domain.Aggregate); ok {
		stored, exists := r.versions[id]
		if (exists && stored != agg.Version()) || (!exists && agg.Version() != 0) {
			r.mu.Unlock()
			return errs.ConcurrentModification(r.Resource, id)
		}
		agg.MarkPersisted(agg.Version() + 1)
		r.versions[id] = agg.Version()
		events = agg.PullEvents()
		r.Events = append(r.Events, events...)
	}
	r.items[id] = a
	r.mu.Unlock()
	if r.Publisher != nil && len(events) > 0 {
		return r.Publisher.Publish(ctx, events...)
	}
	return nil
}

func (r *MemoryRepository[A, ID]) Delete(_ context.Context, a A) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.IDOf(a)
	delete(r.items, id)
	delete(r.versions, id)
	return nil
}

// All returns every stored aggregate.
func (r *MemoryRepository[A, ID]) All() []A {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]A, 0, len(r.items))
	for _, a := range r.items {
		out = append(out, a)
	}
	return out
}

// UnitOfWork runs work directly and counts transactions.
type UnitOfWork struct {
	mu           sync.Mutex
	Transactions int
}

func (u *UnitOfWork) Transact(ctx context.Context, fn func(ctx context.Context) error, _ ...app.TxOptions) error {
	u.mu.Lock()
	u.Transactions++
	u.mu.Unlock()
	return fn(ctx)
}

// Publisher records published events.
type Publisher struct {
	mu     sync.Mutex
	Events []domain.Event
	Err    error // returned by Publish when set
}

func (p *Publisher) Publish(_ context.Context, evs ...domain.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return p.Err
	}
	p.Events = append(p.Events, evs...)
	return nil
}

// OfType returns recorded events of a type.
func (p *Publisher) OfType(t string) []domain.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []domain.Event
	for _, e := range p.Events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// Mailer records sent messages.
type Mailer struct {
	mu   sync.Mutex
	Sent []app.MailMessage
}

func (m *Mailer) Send(_ context.Context, msg app.MailMessage) error {
	m.mu.Lock()
	m.Sent = append(m.Sent, msg)
	m.mu.Unlock()
	return nil
}

// Hasher is a fast, NON-secure password hasher for tests.
type Hasher struct{}

func (Hasher) Hash(p string) (string, error) {
	s := sha256.Sum256([]byte(p))
	return "test$" + hex.EncodeToString(s[:]), nil
}
func (h Hasher) Verify(p, hash string) (bool, error) { x, _ := h.Hash(p); return x == hash, nil }
func (Hasher) NeedsRehash(hash string) bool          { return !strings.HasPrefix(hash, "test$") }

// Tokens is an unsigned token service for tests ("test.<base64 json>").
type Tokens struct{}

func (Tokens) Issue(_ context.Context, c app.Claims, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	c.ExpiresAt = time.Now().Add(ttl)
	b, _ := json.Marshal(struct {
		app.Claims
		Exp int64 `json:"exp"`
	}{c, c.ExpiresAt.Unix()})
	return "test." + base64.RawURLEncoding.EncodeToString(b), nil
}

func (Tokens) Verify(_ context.Context, token string) (app.Claims, error) {
	raw, ok := strings.CutPrefix(token, "test.")
	if !ok {
		return app.Claims{}, errs.ErrInvalidToken
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return app.Claims{}, errs.ErrInvalidToken
	}
	var v struct {
		app.Claims
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &v) != nil || v.Subject == "" {
		return app.Claims{}, errs.ErrInvalidToken
	}
	if time.Now().Unix() > v.Exp {
		return app.Claims{}, errs.ErrExpiredToken
	}
	v.Claims.ExpiresAt = time.Unix(v.Exp, 0)
	return v.Claims, nil
}
