package mailbox

import (
	"errors"
	"testing"
)

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
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, _ := s2.Messages("agent-1")
	if len(got) != 1 || got[0].Body != "kept" {
		t.Fatalf("not persisted: %+v", got)
	}
}
