package ptyruntime

import (
	"strings"
	"testing"
)

func feed(s *Screen, parts ...string) {
	for _, p := range parts {
		_, _ = s.Write([]byte(p))
	}
}

// roundTrip redraws a screen's plain text into a fresh screen of the same size
// (what a client does with a snapshot marker) and returns the result.
func roundTrip(s *Screen) string {
	cols, rows := s.Size()
	n := NewScreen(cols, rows)
	_, _ = n.Write(ScreenBytes([]byte(s.String())))
	return n.String()
}

// F-VT-01: alternate-screen, scroll-region and cursor-addressing traces of
// full-screen programs (an editor, a pager, a monitor) leave the cells a
// terminal would show, and a snapshot of them round-trips.
func TestF_VT_01_FullscreenTracesRoundTrip(t *testing.T) {
	t.Run("editor: alt screen, status line, scroll region", func(t *testing.T) {
		s := NewScreen(20, 6)
		feed(s, "shell$ vim\r\n")
		feed(s, "\x1b[?1049h\x1b[2J\x1b[H") // enter alt screen
		feed(s, "\x1b[1;5r")                // text area = rows 1..5, row 6 = status
		feed(s, "\x1b[6;1H-- INSERT --")    // status line outside the region
		feed(s, "\x1b[1;1Hone\r\ntwo\r\nthree\r\nfour\r\nfive\r\nsix")
		// Typing past the bottom of the region scrolls only the region.
		want := "two\nthree\nfour\nfive\nsix\n-- INSERT --"
		if got := s.String(); got != want {
			t.Fatalf("alt screen:\n%q\nwant\n%q", got, want)
		}
		if rt := roundTrip(s); rt != want {
			t.Fatalf("snapshot round trip:\n%q", rt)
		}
		feed(s, "\x1b[r\x1b[?1049l") // quit: the shell screen is back, untouched
		if got := s.String(); got != "shell$ vim" {
			t.Fatalf("after leaving the alt screen: %q", got)
		}
	})
	t.Run("pager: insert/delete lines and reverse index", func(t *testing.T) {
		s := NewScreen(10, 4)
		feed(s, "\x1b[?1049h", "a\r\nb\r\nc\r\nd", "\x1b[2;1H\x1b[L", "X") // IL at row 2
		if got, want := s.String(), "a\nX\nb\nc"; got != want {
			t.Fatalf("IL: %q want %q", got, want)
		}
		feed(s, "\x1b[1;1H\x1b[M") // DL at row 1
		if got, want := s.String(), "X\nb\nc"; got != want {
			t.Fatalf("DL: %q want %q", got, want)
		}
		feed(s, "\x1b[1;1H\x1bM", "top") // reverse index at the top scrolls down
		if got, want := s.String(), "top\nX\nb\nc"; got != want {
			t.Fatalf("RI: %q want %q", got, want)
		}
	})
	t.Run("monitor: cursor addressing and erase", func(t *testing.T) {
		s := NewScreen(12, 3)
		feed(s, "\x1b[2J\x1b[1;1HCPU 91%\x1b[2;1HMEM 40%\x1b[1;5H\x1b[K", "__") // overwrite then erase to EOL
		if got, want := s.String(), "CPU __\nMEM 40%"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
		feed(s, "\x1b[2;1H\x1b[3P") // delete 3 chars
		if got, want := s.String(), "CPU __\n 40%"; got != want {
			t.Fatalf("DCH: got %q want %q", got, want)
		}
	})
}

// F-VT-02: wide (CJK), emoji and combining sequences occupy the cells a
// terminal gives them, so columns after them line up and a snapshot keeps them.
func TestF_VT_02_WideCombiningEmojiCells(t *testing.T) {
	s := NewScreen(8, 3)
	feed(s, "ab你好c\r\n")    // 2+4+1 = 7 cells
	feed(s, "e\u0301x\r\n") // e + combining acute is ONE cell
	feed(s, "😀!")           // emoji = 2 cells, then '!'
	if got, want := s.String(), "ab你好c\ne\u0301x\n😀!"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Column positions: the 'c' sits in cell 6, so CHA 7 (1-based) overwrites it.
	feed(s, "\x1b[1;7HZ")
	if got := strings.Split(s.String(), "\n")[0]; got != "ab你好Z" {
		t.Fatalf("columns after wide chars misaligned: %q", got)
	}
	// A wide character that does not fit in the last column wraps whole.
	w := NewScreen(4, 2)
	feed(w, "abc你")
	if got, want := w.String(), "abc\n你"; got != want {
		t.Fatalf("wide wrap: %q want %q", got, want)
	}
	// Overwriting half of a wide character blanks the other half.
	h := NewScreen(6, 1)
	feed(h, "你好", "\x1b[1;2HX")
	if got := h.String(); got != " X好" {
		t.Fatalf("split wide char: %q", got)
	}
	// Round trip.
	if rt, want := roundTrip(s), s.String(); rt != want {
		t.Fatalf("round trip %q != %q", rt, want)
	}
}

// F-VT-03: SGR attributes survive for the local authorized view and are absent
// from the plain (journal / redacted) rendering.
func TestF_VT_03_SGRKeptLocallyDroppedInRedactedViews(t *testing.T) {
	s := NewScreen(40, 2)
	feed(s, "plain \x1b[1;31mbold-red\x1b[0m \x1b[38;5;82mgreen\x1b[0m \x1b[48;2;1;2;3mtrue\x1b[0m")
	if strings.Contains(s.String(), "\x1b") {
		t.Fatalf("plain rendering leaked attributes: %q", s.String())
	}
	if got, want := s.String(), "plain bold-red green true"; got != want {
		t.Fatalf("plain text %q want %q", got, want)
	}
	styled := s.StyledString()
	for _, want := range []string{"\x1b[0;1;31mbold-red", "\x1b[0;38;5;82mgreen", "\x1b[0;48;2;1;2;3mtrue"} {
		if !strings.Contains(styled, want) {
			t.Fatalf("styled view lost %q:\n%q", want, styled)
		}
	}
	// Replaying the styled view reproduces the same attributes (idempotent).
	n := NewScreen(40, 2)
	_, _ = n.Write(ScreenBytes([]byte(styled)))
	if n.StyledString() != styled {
		t.Fatalf("styled round trip changed:\n%q\n%q", n.StyledString(), styled)
	}
	// The journal snapshot is built from String(): plain.
	if strings.Contains(s.String(), "[") {
		t.Fatal("snapshot would carry escape bytes")
	}
}
