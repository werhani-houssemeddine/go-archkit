// Package cache implements app.Cache in memory (LRU + TTL) and on any Redis client,
// plus typed JSON helpers with single-flight loading to prevent cache stampedes.
package cache

import (
	"container/list"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/errs"
)

/* ================================================================== */
/* In-memory LRU                                                       */
/* ================================================================== */

// Memory is a per-process LRU cache with TTL. Use Redis when running several instances.
type Memory struct {
	MaxEntries int // default 10,000
	Now        func() time.Time

	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
}

type entry struct {
	key     string
	value   []byte
	expires time.Time
}

var _ app.Cache = (*Memory)(nil)

// NewMemory creates an in-memory cache.
func NewMemory(maxEntries int) *Memory {
	return &Memory{MaxEntries: maxEntries, ll: list.New(), items: map[string]*list.Element{}}
}

func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Memory) init() {
	if m.items == nil {
		m.ll, m.items = list.New(), map[string]*list.Element{}
	}
}

// Get returns the value if present and not expired.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	el, ok := m.items[key]
	if !ok {
		return nil, false, nil
	}
	e := el.Value.(*entry)
	if !e.expires.IsZero() && !m.now().Before(e.expires) {
		m.ll.Remove(el)
		delete(m.items, key)
		return nil, false, nil
	}
	m.ll.MoveToFront(el)
	return e.value, true, nil
}

// Set stores value; ttl <= 0 means no expiry.
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	var exp time.Time
	if ttl > 0 {
		exp = m.now().Add(ttl)
	}
	if el, ok := m.items[key]; ok {
		el.Value = &entry{key, value, exp}
		m.ll.MoveToFront(el)
		return nil
	}
	m.items[key] = m.ll.PushFront(&entry{key, value, exp})
	maxE := m.MaxEntries
	if maxE <= 0 {
		maxE = 10_000
	}
	for m.ll.Len() > maxE {
		last := m.ll.Back()
		m.ll.Remove(last)
		delete(m.items, last.Value.(*entry).key)
	}
	return nil
}

// Delete removes keys.
func (m *Memory) Delete(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	for _, k := range keys {
		if el, ok := m.items[k]; ok {
			m.ll.Remove(el)
			delete(m.items, k)
		}
	}
	return nil
}

// DeletePrefix removes every key starting with prefix.
func (m *Memory) DeletePrefix(_ context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	n := 0
	for k, el := range m.items {
		if strings.HasPrefix(k, prefix) {
			m.ll.Remove(el)
			delete(m.items, k)
			n++
		}
	}
	return n, nil
}

// Len returns the number of entries (including not-yet-evicted expired ones).
func (m *Memory) Len() int { m.mu.Lock(); defer m.mu.Unlock(); m.init(); return m.ll.Len() }

/* ================================================================== */
/* Redis                                                               */
/* ================================================================== */

// RedisClient is the small command surface needed; wrap go-redis or rueidis in a few lines.
type RedisClient interface {
	Get(ctx context.Context, key string) (value []byte, found bool, err error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) (int, error)
	// Scan iterates keys matching a glob pattern (use SCAN, never KEYS).
	Scan(ctx context.Context, cursor uint64, match string, count int) (keys []string, next uint64, err error)
}

// Redis implements app.Cache on a RedisClient with a key prefix.
type Redis struct {
	Client RedisClient
	Prefix string // e.g. "myapp:cache:"
}

var _ app.Cache = Redis{}

func (r Redis) wrap(err error) error {
	if err == nil {
		return nil
	}
	return errs.Wrap(err, errs.KindInfrastructure, "CACHE_ERROR", "cache operation failed")
}

func (r Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, ok, err := r.Client.Get(ctx, r.Prefix+key)
	return v, ok, r.wrap(err)
}

func (r Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.wrap(r.Client.Set(ctx, r.Prefix+key, value, ttl))
}

func (r Redis) Delete(ctx context.Context, keys ...string) error {
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = r.Prefix + k
	}
	_, err := r.Client.Del(ctx, full...)
	return r.wrap(err)
}

func (r Redis) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	pattern := escapeGlob(r.Prefix+prefix) + "*"
	var cursor uint64
	total := 0
	for {
		keys, next, err := r.Client.Scan(ctx, cursor, pattern, 500)
		if err != nil {
			return total, r.wrap(err)
		}
		if len(keys) > 0 {
			n, err := r.Client.Del(ctx, keys...)
			if err != nil {
				return total, r.wrap(err)
			}
			total += n
		}
		if next == 0 {
			return total, nil
		}
		cursor = next
	}
}

func escapeGlob(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`).Replace(s)
}

/* ================================================================== */
/* Typed helpers                                                       */
/* ================================================================== */

// Namespaced prefixes every key (per module or tenant).
func Namespaced(c app.Cache, prefix string) app.Cache { return namespaced{c, prefix} }

type namespaced struct {
	c      app.Cache
	prefix string
}

func (n namespaced) Get(ctx context.Context, k string) ([]byte, bool, error) {
	return n.c.Get(ctx, n.prefix+k)
}
func (n namespaced) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	return n.c.Set(ctx, n.prefix+k, v, ttl)
}
func (n namespaced) Delete(ctx context.Context, keys ...string) error {
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = n.prefix + k
	}
	return n.c.Delete(ctx, full...)
}
func (n namespaced) DeletePrefix(ctx context.Context, p string) (int, error) {
	return n.c.DeletePrefix(ctx, n.prefix+p)
}

// Typed is a JSON-encoding view of a cache with single-flight GetOrLoad.
type Typed[T any] struct {
	Cache app.Cache
	TTL   time.Duration

	mu       sync.Mutex
	inflight map[string]*call[T]
}

type call[T any] struct {
	wg  sync.WaitGroup
	val T
	err error
}

// NewTyped creates a typed view.
func NewTyped[T any](c app.Cache, ttl time.Duration) *Typed[T] { return &Typed[T]{Cache: c, TTL: ttl} }

// Get decodes the cached value.
func (t *Typed[T]) Get(ctx context.Context, key string) (T, bool, error) {
	var v T
	raw, ok, err := t.Cache.Get(ctx, key)
	if err != nil || !ok {
		return v, false, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, false, nil // treat corrupt entries as misses
	}
	return v, true, nil
}

// Set encodes and stores v.
func (t *Typed[T]) Set(ctx context.Context, key string, v T) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return t.Cache.Set(ctx, key, raw, t.TTL)
}

// GetOrLoad returns the cached value or calls load once for all concurrent callers of
// the same key (per process), then stores the result. Cache read/write failures fall
// back to load, so an outage of the cache never breaks the caller.
func (t *Typed[T]) GetOrLoad(ctx context.Context, key string, load func(ctx context.Context) (T, error)) (T, error) {
	if v, ok, err := t.Get(ctx, key); err == nil && ok {
		return v, nil
	}
	t.mu.Lock()
	if t.inflight == nil {
		t.inflight = map[string]*call[T]{}
	}
	if c, ok := t.inflight[key]; ok {
		t.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &call[T]{}
	c.wg.Add(1)
	t.inflight[key] = c
	t.mu.Unlock()

	c.val, c.err = load(ctx)
	if c.err == nil {
		_ = t.Set(ctx, key, c.val)
	}
	c.wg.Done()
	t.mu.Lock()
	delete(t.inflight, key)
	t.mu.Unlock()
	return c.val, c.err
}
