package ids_test

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/clock"
	"github.com/werhani-houssemeddine/go-archkit/ids"
)

func TestV7SortableUniqueValid(t *testing.T) {
	const n = 5000
	out := make([]string, n)
	for i := range out {
		out[i] = ids.NewV7().String()
	}
	if !sort.StringsAreSorted(out) {
		t.Fatal("v7 ids must be monotonic within a process")
	}
	seen := map[string]bool{}
	for _, s := range out {
		if seen[s] || !ids.IsValid(s) {
			t.Fatalf("duplicate or invalid %s", s)
		}
		seen[s] = true
	}
	u, _ := ids.Parse(out[0])
	if u.Version() != 7 || time.Since(u.Time()) > time.Minute {
		t.Fatal("version/time")
	}
	if !ids.IsValid(ids.NewV4().String()) || ids.IsValid("not-a-uuid") {
		t.Fatal("validation")
	}
}

func TestV7Concurrent(t *testing.T) {
	var mu sync.Mutex
	seen := map[ids.UUID]bool{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				u := ids.NewV7()
				mu.Lock()
				if seen[u] {
					t.Error("duplicate")
				}
				seen[u] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

func TestClockHelpers(t *testing.T) {
	if clock.Age(time.Date(2000, 6, 15, 0, 0, 0, 0, time.UTC), time.Date(2025, 6, 14, 0, 0, 0, 0, time.UTC)) != 24 {
		t.Fatal("age")
	}
	if got := clock.AddMonths(time.Date(2025, 1, 31, 10, 0, 0, 0, time.UTC), 1); !got.Equal(time.Date(2025, 2, 28, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("AddMonths: %v", got)
	}
	f := clock.NewFixed(time.Unix(0, 0))
	f.Advance(time.Hour)
	if f.Now().Unix() != 3600 {
		t.Fatal("fixed clock")
	}
}
