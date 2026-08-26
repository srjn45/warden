package groupstore

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewStore(filepath.Join(t.TempDir(), "groups"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func group(name string, members ...Member) *Group {
	return &Group{Name: name, Members: members}
}

func member(id, projectKey string) Member {
	return Member{
		AgentID:    id,
		ProjectKey: projectKey,
		Summary:    "one-line project summary",
		JoinedAt:   time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
	}
}

func TestStoreCreateGetList(t *testing.T) {
	st := newTestStore(t)
	if err := st.Create(group("beta", member("a1", "github.com/o/repo"))); err != nil {
		t.Fatalf("Create beta: %v", err)
	}
	if err := st.Create(group("alpha")); err != nil {
		t.Fatalf("Create alpha: %v", err)
	}

	got, err := st.Get("beta")
	if err != nil {
		t.Fatalf("Get beta: %v", err)
	}
	if got.Name != "beta" || len(got.Members) != 1 {
		t.Fatalf("Get beta = %+v", got)
	}
	if got.Members[0].AgentID != "a1" || got.Members[0].ProjectKey != "github.com/o/repo" {
		t.Fatalf("member not round-tripped: %+v", got.Members[0])
	}
	if !got.Members[0].JoinedAt.Equal(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("JoinedAt not round-tripped: %v", got.Members[0].JoinedAt)
	}

	list, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("List not sorted by name: %+v", list)
	}
}

func TestStoreCreateDuplicate(t *testing.T) {
	st := newTestStore(t)
	if err := st.Create(group("dup")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.Create(group("dup")); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create err = %v, want ErrExists", err)
	}
}

func TestStoreGetMissing(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.Get("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}
}

func TestStoreUpdateSeatsMembers(t *testing.T) {
	st := newTestStore(t)
	if err := st.Create(group("g")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Join: add a seat.
	if err := st.Update("g", func(g *Group) {
		g.Members = append(g.Members, member("a1", "p1"))
	}); err != nil {
		t.Fatalf("Update add: %v", err)
	}
	got, _ := st.Get("g")
	if len(got.Members) != 1 {
		t.Fatalf("after add, members = %+v", got.Members)
	}

	// Leave: remove the seat.
	if err := st.Update("g", func(g *Group) {
		g.Members = nil
	}); err != nil {
		t.Fatalf("Update remove: %v", err)
	}
	got, _ = st.Get("g")
	if len(got.Members) != 0 {
		t.Fatalf("after remove, members = %+v", got.Members)
	}

	if err := st.Update("ghost", func(*Group) {}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing err = %v, want ErrNotFound", err)
	}
}

func TestStoreDelete(t *testing.T) {
	st := newTestStore(t)
	if err := st.Create(group("g")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.Delete("g"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get("g"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete, Get err = %v, want ErrNotFound", err)
	}
	if err := st.Delete("g"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing err = %v, want ErrNotFound", err)
	}
}

// Persistence: a second Store over the same dir (after the first is closed,
// mirroring a daemon restart) sees what the first wrote.
func TestStorePersistsAcrossInstances(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "groups")
	st1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("open st1: %v", err)
	}
	if err := st1.Create(group("keep", member("a1", "p1"), member("a2", "p2"))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("close st1: %v", err)
	}

	st2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	got, err := st2.Get("keep")
	if err != nil {
		t.Fatalf("Get keep after reopen: %v", err)
	}
	if len(got.Members) != 2 {
		t.Fatalf("after reopen, members = %+v", got.Members)
	}
}

// The group record must stay lean (§4.3): only the roster, never transcripts or
// logs, so it never approaches the oversized-record (>64 KB) ReadAt / index-
// corruption regime. A generously-sized roster still serialises well under it.
func TestRecordStaysSmall(t *testing.T) {
	g := &Group{Name: "big"}
	for range 100 {
		g.Members = append(g.Members, Member{
			AgentID:    "agent-00000000-0000-0000-0000-000000000000",
			ProjectKey: "github.com/some-org/some-reasonably-named-repository",
			Summary:    "A one-line project summary describing what this member's project does.",
			JoinedAt:   time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		})
	}
	rec, err := toRecord(g)
	if err != nil {
		t.Fatalf("toRecord: %v", err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const maxBytes = 64 * 1024
	if len(b) >= maxBytes {
		t.Fatalf("record for 100 members = %d bytes, want < %d (roster must stay lean)", len(b), maxBytes)
	}
}
