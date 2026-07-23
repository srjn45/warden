package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// benchSession returns a representative, fully-populated session whose id is
// derived from i, so a benchmark can seed a store with N distinct records.
func benchSession(i int) *Session {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	id := fmt.Sprintf("agent-%06d", i)
	return &Session{
		ID:          id,
		Name:        id,
		Type:        TypeDevelopment,
		TmuxSession: id,
		Repo:        "/repo",
		Worktree:    ".worktrees/" + id,
		Branch:      id,
		Status:      StatusWorking,
		CreatedAt:   now,
		UpdatedAt:   now,
		Events: []Event{
			{TS: now, Type: "spawn", Detail: "started"},
			{TS: now, Type: "status", Detail: "working"},
		},
		LastPaneExcerpt: "doing work on the thing",
	}
}

// seedStore inserts n distinct sessions and returns the ready store.
func seedStore(b *testing.B, n int) *FileStore {
	b.Helper()
	ctx := context.Background()
	st, err := NewFileStore(b.TempDir())
	if err != nil {
		b.Fatalf("NewFileStore: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := st.Insert(ctx, benchSession(i)); err != nil {
			b.Fatalf("seed Insert %d: %v", i, err)
		}
	}
	return st
}

// BenchmarkInsert measures the cost of a single Insert (a ScrivaDB record append
// after the active-collection name-uniqueness scan), the per-spawn store cost on
// the hot path.
func BenchmarkInsert(b *testing.B) {
	ctx := context.Background()
	st, err := NewFileStore(b.TempDir())
	if err != nil {
		b.Fatalf("NewFileStore: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.Insert(ctx, benchSession(i)); err != nil {
			b.Fatalf("Insert: %v", err)
		}
	}
}

// BenchmarkGet measures a single-record read+decode from a populated store.
func BenchmarkGet(b *testing.B) {
	ctx := context.Background()
	const n = 1000
	st := seedStore(b, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("agent-%06d", i%n)
		if _, err := st.Get(ctx, id); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

// BenchmarkList measures a full active-listing (read+decode+sort of every
// record) at a few fleet sizes — the cost behind `warden ls` and the dashboard
// poll, which is what the roadmap flags as the scaling concern.
func BenchmarkList(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{10, 100, 1000} {
		st := seedStore(b, n)
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := st.List(ctx)
				if err != nil {
					b.Fatalf("List: %v", err)
				}
				if len(out) != n {
					b.Fatalf("List returned %d, want %d", len(out), n)
				}
			}
		})
	}
}
