package mailbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNextIDUsesHighWaterMark(t *testing.T) {
	// len+1 would collide after a drop; nextID must be max(existing ids)+1.
	if got := nextID(nil); got != "1" {
		t.Fatalf("empty inbox nextID want 1, got %s", got)
	}
	ms := []Message{{ID: "1"}, {ID: "5"}, {ID: "2"}} // 3 messages but high id is 5
	if got := nextID(ms); got != "6" {
		t.Fatalf("nextID want 6 (max+1, not len+1), got %s", got)
	}
}

func TestCompactNeverDropsUnreadButDropsAgedRead(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	ms := []Message{
		{ID: "1", Read: true, TS: old},        // aged read → dropped
		{ID: "2", Read: false, TS: old},       // unread, even though old → kept
		{ID: "3", Read: true, TS: time.Now()}, // recent read → kept
	}
	got := compact(ms, maxInboxMessages, readRetention)
	if len(got) != 2 || got[0].ID != "2" || got[1].ID != "3" {
		t.Fatalf("aged read dropped, unread+recent kept: got %+v", got)
	}
}

func TestCompactCapShedsOldestReadFirstNeverUnread(t *testing.T) {
	now := time.Now()
	// 4 messages, all recent: 2 read (oldest), then unread, then read.
	ms := []Message{
		{ID: "1", Read: true, TS: now},
		{ID: "2", Read: true, TS: now},
		{ID: "3", Read: false, TS: now},
		{ID: "4", Read: true, TS: now},
	}
	// Cap at 2: must shed the two oldest READ (ids 1,2), keep unread 3 and read 4.
	got := compact(ms, 2, readRetention)
	if len(got) != 2 || got[0].ID != "3" || got[1].ID != "4" {
		t.Fatalf("cap must shed oldest read first, never unread: got %+v", got)
	}
}

func TestCompactKeepsUnreadEvenPastCap(t *testing.T) {
	now := time.Now()
	ms := []Message{
		{ID: "1", Read: false, TS: now},
		{ID: "2", Read: false, TS: now},
		{ID: "3", Read: false, TS: now},
	}
	// Cap of 1 but all unread: undelivered work is never dropped.
	got := compact(ms, 1, readRetention)
	if len(got) != 3 {
		t.Fatalf("unread work must survive the cap, got %d", len(got))
	}
}

func TestAppendAssignsIDAndTS(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, err := s.Append(Message{To: "agent-1", From: "agent-2", Body: "hello"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if m.ID != "1" || m.Read || m.TS.IsZero() {
		t.Fatalf("got %+v", m)
	}
	m2, _ := s.Append(Message{To: "agent-1", From: "agent-2", Body: "again"})
	if m2.ID != "2" {
		t.Fatalf("want sequential id 2, got %q", m2.ID)
	}
}

func TestMessagesChronologicalPerRecipient(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Append(Message{To: "agent-1", From: "x", Body: "a"})
	s.Append(Message{To: "agent-1", From: "y", Body: "b"})
	s.Append(Message{To: "agent-2", From: "z", Body: "c"})

	one, err := s.Messages("agent-1")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(one) != 2 || one[0].Body != "a" || one[1].Body != "b" {
		t.Fatalf("agent-1 inbox wrong: %+v", one)
	}
	two, _ := s.Messages("agent-2")
	if len(two) != 1 || two[0].Body != "c" {
		t.Fatalf("agent-2 inbox wrong: %+v", two)
	}
}

func TestMessagesEmptyReturnsEmptySlice(t *testing.T) {
	s, _ := New(t.TempDir())
	got, err := s.Messages("nobody")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", got)
	}
}

func TestAppendBadRecipientRejected(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Append(Message{To: "a/b", From: "x", Body: "v"}); !errors.Is(err, ErrBadRecipient) {
		t.Fatalf("want ErrBadRecipient, got %v", err)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir)
	s1.Append(Message{To: "agent-1", From: "x", Body: "kept"})
	// The store is now embedded (FileDB): a reopen models a daemon restart, so
	// close the first handle (flushing its index) before opening a new one.
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, _ := s2.Messages("agent-1")
	if len(got) != 1 || got[0].Body != "kept" {
		t.Fatalf("not persisted: %+v", got)
	}
}

func TestMarkRead(t *testing.T) {
	s, _ := New(t.TempDir())
	m1, _ := s.Append(Message{To: "a", From: "x", Body: "1"})
	s.Append(Message{To: "a", From: "x", Body: "2"})

	if err := s.MarkRead("a", []string{m1.ID}); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	got, _ := s.Messages("a")
	if !got[0].Read || got[1].Read {
		t.Fatalf("read flags wrong: %+v", got)
	}
}

func TestTakeFirstUnreadOrderAndMarks(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Append(Message{To: "a", From: "x", Body: "first"})
	s.Append(Message{To: "a", From: "x", Body: "second"})

	m, ok, err := s.TakeFirstUnread("a", "")
	if err != nil || !ok || m.Body != "first" {
		t.Fatalf("first take: ok=%v m=%+v err=%v", ok, m, err)
	}
	// taken message is now read; next take returns the second
	m2, ok, _ := s.TakeFirstUnread("a", "")
	if !ok || m2.Body != "second" {
		t.Fatalf("second take: ok=%v m=%+v", ok, m2)
	}
	// nothing unread left
	if _, ok, _ := s.TakeFirstUnread("a", ""); ok {
		t.Fatalf("expected no unread left")
	}
}

func TestTakeFirstUnreadFromFilter(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Append(Message{To: "a", From: "x", Body: "from-x"})
	s.Append(Message{To: "a", From: "y", Body: "from-y"})

	m, ok, _ := s.TakeFirstUnread("a", "y")
	if !ok || m.Body != "from-y" {
		t.Fatalf("from filter: ok=%v m=%+v", ok, m)
	}
	// x's message is still unread (filter skipped it without consuming)
	m2, ok, _ := s.TakeFirstUnread("a", "x")
	if !ok || m2.Body != "from-x" {
		t.Fatalf("x still unread expected: ok=%v m=%+v", ok, m2)
	}
}

func TestTakeFirstUnreadEmpty(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, ok, err := s.TakeFirstUnread("nobody", ""); ok || err != nil {
		t.Fatalf("want (false,nil), got ok=%v err=%v", ok, err)
	}
}

func TestAllGathersEveryInbox(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Append(Message{To: "agent-1", From: "x", Body: "a"})
	s.Append(Message{To: "agent-1", From: "y", Body: "b"})
	s.Append(Message{To: "agent-2", From: "z", Body: "c"})

	all, err := s.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 messages across inboxes, got %d: %+v", len(all), all)
	}
	bodies := map[string]bool{}
	for _, m := range all {
		bodies[m.Body] = true
	}
	if !bodies["a"] || !bodies["b"] || !bodies["c"] {
		t.Fatalf("missing a message: %+v", all)
	}
}

func TestAllEmptyDir(t *testing.T) {
	s, _ := New(t.TempDir())
	all, err := s.All()
	if err != nil || len(all) != 0 {
		t.Fatalf("want ([],nil), got len=%d err=%v", len(all), err)
	}
}

func TestAllIgnoresTempFiles(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Append(Message{To: "agent-1", From: "x", Body: "a"})
	// A leftover atomic-write temp file must not be parsed as an inbox.
	if err := os.WriteFile(filepath.Join(s.dir, ".tmp-garbage"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	all, err := s.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 (temp ignored), got %d: %+v", len(all), all)
	}
}

func TestDeleteInboxRemovesMessages(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	s.Append(Message{To: "agent-x", From: "y", Body: "hi"})
	if msgs, _ := s.Messages("agent-x"); len(msgs) != 1 {
		t.Fatalf("inbox should have 1 message before delete, got %d", len(msgs))
	}

	if err := s.DeleteInbox("agent-x"); err != nil {
		t.Fatalf("DeleteInbox: %v", err)
	}
	msgs, err := s.Messages("agent-x")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("want empty inbox after delete, got %d", len(msgs))
	}
	// The deleted inbox must not resurface in the global view either.
	if all, _ := s.All(); len(all) != 0 {
		t.Fatalf("want no messages in All() after delete, got %d", len(all))
	}
}

func TestDeleteInboxMissingIsNoError(t *testing.T) {
	s, _ := New(t.TempDir())
	if err := s.DeleteInbox("never-used"); err != nil {
		t.Fatalf("deleting a missing inbox must be a no-op, got %v", err)
	}
}

func TestDeleteInboxBadRecipient(t *testing.T) {
	s, _ := New(t.TempDir())
	if err := s.DeleteInbox("../escape"); !errors.Is(err, ErrBadRecipient) {
		t.Fatalf("want ErrBadRecipient, got %v", err)
	}
}
