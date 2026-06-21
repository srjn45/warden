package ctxstore

import (
	"errors"
	"strconv"
	"sync"
	"testing"
)

func TestSetThenGet(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Set("global.greeting", "hello", "agent-A"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e, err := s.Get("global.greeting")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Value != "hello" || e.UpdatedBy != "agent-A" || e.Key != "global.greeting" {
		t.Fatalf("got %+v", e)
	}
	if e.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt not set")
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSetEmptyKeyRejected(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Set("", "v", "by"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("want ErrBadKey, got %v", err)
	}
}

func TestSetKeyWithSlashRejected(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Set("a/b", "v", "by"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("want ErrBadKey for slash key, got %v", err)
	}
}

func TestConcurrentSetDistinctKeys(t *testing.T) {
	s, _ := New(t.TempDir())
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Set("k"+strconv.Itoa(i), "v", "w"); err != nil {
				t.Errorf("Set: %v", err)
			}
		}(i)
	}
	wg.Wait()
	all, err := s.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != n {
		t.Fatalf("want %d entries after concurrent writes, got %d", n, len(all))
	}
}

func TestSetOverwrites(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("k", "v1", "a")
	s.Set("k", "v2", "b")
	e, _ := s.Get("k")
	if e.Value != "v2" || e.UpdatedBy != "b" {
		t.Fatalf("overwrite failed: %+v", e)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir)
	s1.Set("k", "v", "a")
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	e, err := s2.Get("k")
	if err != nil || e.Value != "v" {
		t.Fatalf("not persisted: %+v err=%v", e, err)
	}
}

func TestListByPrefixSorted(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("pipeline.p1.b.output", "B", "b")
	s.Set("pipeline.p1.a.output", "A", "a")
	s.Set("global.x", "X", "x")

	all, err := s.List("")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3, got %d", len(all))
	}
	// sorted by key: global.x, pipeline.p1.a.output, pipeline.p1.b.output
	if all[0].Key != "global.x" || all[1].Key != "pipeline.p1.a.output" {
		t.Fatalf("not sorted: %+v", all)
	}

	pref, _ := s.List("pipeline.p1.")
	if len(pref) != 2 {
		t.Fatalf("prefix want 2, got %d", len(pref))
	}
}

func TestListEmptyStoreReturnsEmptySlice(t *testing.T) {
	s, _ := New(t.TempDir())
	got, err := s.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", got)
	}
}

func TestDel(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("k", "v", "a")
	if err := s.Del("k"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("still present after Del")
	}
	if err := s.Del("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Del missing want ErrNotFound, got %v", err)
	}
}

func TestDelPrefixRemovesOnlyMatchingKeys(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("global.a", "1", "by")
	s.Set("pipeline.p1.j1.output", "x", "by")
	s.Set("pipeline.p1.j2.output", "y", "by")
	s.Set("pipeline.p10.j1.output", "z", "by") // must NOT match prefix "pipeline.p1."

	n, err := s.DelPrefix("pipeline.p1.")
	if err != nil {
		t.Fatalf("DelPrefix: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 deleted, got %d", n)
	}
	if _, err := s.Get("pipeline.p1.j1.output"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("p1.j1 should be gone, got %v", err)
	}
	if _, err := s.Get("pipeline.p10.j1.output"); err != nil {
		t.Fatalf("p10 must survive prefix p1., got %v", err)
	}
	if _, err := s.Get("global.a"); err != nil {
		t.Fatalf("unrelated key must survive, got %v", err)
	}
}

func TestCompareAndSetCreatesWhenAbsent(t *testing.T) {
	s, _ := New(t.TempDir())
	e, err := s.CompareAndSet("global.lock", "", "agent-A", "agent-A")
	if err != nil {
		t.Fatalf("CAS on absent key with empty expected must succeed, got %v", err)
	}
	if e.Value != "agent-A" {
		t.Fatalf("want value agent-A, got %q", e.Value)
	}
}

func TestCompareAndSetConflictWhenPresentButExpectedAbsent(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("global.lock", "agent-A", "agent-A")
	if _, err := s.CompareAndSet("global.lock", "", "agent-B", "agent-B"); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS with empty expected on an existing key must conflict, got %v", err)
	}
	e, _ := s.Get("global.lock")
	if e.Value != "agent-A" {
		t.Fatalf("conflicting CAS must not overwrite, got %q", e.Value)
	}
}

func TestCompareAndSetMatchAndMismatch(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("k", "v1", "a")
	if _, err := s.CompareAndSet("k", "v1", "v2", "b"); err != nil {
		t.Fatalf("matching expected must swap, got %v", err)
	}
	if e, _ := s.Get("k"); e.Value != "v2" {
		t.Fatalf("want v2 after successful CAS, got %q", e.Value)
	}
	if _, err := s.CompareAndSet("k", "v1", "v3", "c"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale expected must conflict, got %v", err)
	}
}

// TestCompareAndSetSerializesConcurrentWriters is the point of the primitive:
// N racing read-modify-write loops must produce exactly N increments with no
// lost updates (which a Get-then-Set would not guarantee).
func TestCompareAndSetSerializesConcurrentWriters(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("counter", "0", "init")
	const writers = 20
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				e, _ := s.Get("counter")
				n, _ := strconv.Atoi(e.Value)
				_, err := s.CompareAndSet("counter", e.Value, strconv.Itoa(n+1), "w")
				if err == nil {
					return
				}
				if !errors.Is(err, ErrConflict) {
					t.Errorf("unexpected CAS error: %v", err)
					return
				}
				// lost the race — re-read and retry
			}
		}()
	}
	wg.Wait()
	e, _ := s.Get("counter")
	if e.Value != strconv.Itoa(writers) {
		t.Fatalf("want %d increments with no lost updates, got %q", writers, e.Value)
	}
}

func TestAppendCreatesThenConcatenates(t *testing.T) {
	s, _ := New(t.TempDir())
	if e, err := s.Append("log", "first", "\n", "a"); err != nil || e.Value != "first" {
		t.Fatalf("append to absent key must create with no leading sep, got %q err=%v", e.Value, err)
	}
	if e, err := s.Append("log", "second", "\n", "b"); err != nil || e.Value != "first\nsecond" {
		t.Fatalf("append to existing key must insert sep, got %q err=%v", e.Value, err)
	}
}

func TestDelPrefixNoMatchReturnsZero(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("global.a", "1", "by")
	n, err := s.DelPrefix("pipeline.ghost.")
	if err != nil {
		t.Fatalf("DelPrefix: %v", err)
	}
	if n != 0 {
		t.Fatalf("want 0 deleted, got %d", n)
	}
}

func TestDelPrefixEmptyRejected(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("global.a", "1", "by")
	if _, err := s.DelPrefix(""); !errors.Is(err, ErrBadKey) {
		t.Fatalf("empty prefix must be rejected with ErrBadKey, got %v", err)
	}
	if _, err := s.Get("global.a"); err != nil {
		t.Fatalf("rejected DelPrefix must not delete anything, got %v", err)
	}
}
