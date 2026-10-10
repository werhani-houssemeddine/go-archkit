package cache_test

import (
	"context"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/cache"
)

func TestMemoryTTLAndLRU(t *testing.T) {
	now := time.Unix(0, 0)
	m := cache.NewMemory(2)
	m.Now = func() time.Time { return now }
	ctx := context.Background()
	_ = m.Set(ctx, "a", []byte("1"), 100*time.Millisecond)
	now = now.Add(101 * time.Millisecond)
	if _, ok, _ := m.Get(ctx, "a"); ok {
		t.Fatal("expired")
	}
	_ = m.Set(ctx, "b", []byte("2"), 0)
	_ = m.Set(ctx, "c", []byte("3"), 0)
	_, _, _ = m.Get(ctx, "b")
	_ = m.Set(ctx, "d", []byte("4"), 0) // evicts c
	if _, ok, _ := m.Get(ctx, "c"); ok {
		t.Fatal("LRU eviction")
	}
	if n, _ := m.DeletePrefix(ctx, "b"); n != 1 {
		t.Fatal("prefix delete")
	}
}

func TestTypedSingleFlight(t *testing.T) {
	type User struct{ Name string }
	c := cache.NewTyped[User](cache.Namespaced(cache.NewMemory(100), "t1:"), time.Minute)
	var calls atomic.Int32
	var wg sync.WaitGroup
	results := make([]User, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := c.GetOrLoad(context.Background(), "user:1", func(context.Context) (User, error) {
				calls.Add(1)
				time.Sleep(20 * time.Millisecond)
				return User{Name: "Ali"}, nil
			})
			if err != nil {
				t.Error(err)
			}
			results[i] = u
		}(i)
	}
	wg.Wait()
	if calls.Load() != 1 || results[9].Name != "Ali" {
		t.Fatalf("calls=%d", calls.Load())
	}
}

type fakeRedis struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (f *fakeRedis) Get(_ context.Context, k string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[k]
	return v, ok, nil
}
func (f *fakeRedis) Set(_ context.Context, k string, v []byte, _ time.Duration) error {
	f.mu.Lock()
	f.m[k] = v
	f.mu.Unlock()
	return nil
}
func (f *fakeRedis) Del(_ context.Context, keys ...string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, k := range keys {
		if _, ok := f.m[k]; ok {
			delete(f.m, k)
			n++
		}
	}
	return n, nil
}
func (f *fakeRedis) Scan(_ context.Context, _ uint64, match string, _ int) ([]string, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	pattern := strings.ReplaceAll(match, `\`, "")
	for k := range f.m {
		if ok, _ := path.Match(pattern, k); ok {
			out = append(out, k)
		}
	}
	return out, 0, nil
}

func TestRedisAdapter(t *testing.T) {
	r := cache.Redis{Client: &fakeRedis{m: map[string][]byte{}}, Prefix: "app:"}
	ctx := context.Background()
	_ = r.Set(ctx, "user:1", []byte("a"), time.Minute)
	_ = r.Set(ctx, "user:2", []byte("b"), 0)
	if v, ok, _ := r.Get(ctx, "user:1"); !ok || string(v) != "a" {
		t.Fatal("get")
	}
	if n, err := r.DeletePrefix(ctx, "user:"); err != nil || n != 2 {
		t.Fatalf("delete prefix %d %v", n, err)
	}
}
