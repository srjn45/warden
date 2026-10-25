package ptyruntime

import (
	"errors"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/terminalcontract"
)

// 02b-05 adversarial bounds: the modelled screen, combining marks and the
// rendered snapshot are bounded no matter what an attach client or untrusted
// program output asks for.
func TestScreenModelBoundsAndSnapshotCap(t *testing.T) {
	if MaxSnapshotBytes != terminalcontract.Default.SnapshotMaxBytes {
		t.Fatalf("MaxSnapshotBytes %d != budget %d", MaxSnapshotBytes, terminalcontract.Default.SnapshotMaxBytes)
	}
	// An absurd size is clamped in the model (never a 65535x65535 grid).
	s := NewScreen(65535, 65535)
	if c, r := s.Size(); !FitsScreen(c, r) {
		t.Fatalf("model not clamped: %dx%d", c, r)
	}
	s.Resize(100000, 3)
	if c, _ := s.Size(); c != MaxCols {
		t.Fatalf("cols not clamped: %d", c)
	}

	// A combining-mark flood on one cell is bounded.
	s = NewScreen(80, 24)
	feed(s, "e"+strings.Repeat("́", 100000))
	if got := s.String(); len(got) > 1+maxCombiningBytes {
		t.Fatalf("combining flood kept %d bytes", len(got))
	}

	// The largest legal screen full of 4-byte runes + max combining marks still
	// renders a snapshot within MaxSnapshotBytes, keeping the bottom rows.
	s = NewScreen(512, 512)
	line := strings.Repeat("😀"+strings.Repeat("́", 8), 256) // wide: 2 cells each
	for i := range 511 {
		feed(s, line)
		if i < 510 {
			feed(s, "\r\n")
		}
	}
	feed(s, "\r\nBOTTOM")
	got := s.String()
	if len(got) > MaxSnapshotBytes {
		t.Fatalf("snapshot %d bytes > %d", len(got), MaxSnapshotBytes)
	}
	if !strings.HasSuffix(got, "BOTTOM") {
		t.Fatal("snapshot cap must keep the bottom (cursor) rows")
	}
	if styled := s.StyledString(); len(styled) > MaxSnapshotBytes {
		t.Fatalf("styled snapshot %d bytes > %d", len(styled), MaxSnapshotBytes)
	}
}

// Session.Resize refuses an out-of-bounds size with an actionable error and
// keeps the PTY's current size.
func TestResizeBeyondBoundsRefused(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	for _, sz := range [][2]uint16{{65535, 65535}, {MaxCols + 1, 1}, {1, MaxRows + 1}, {600, 600}} {
		err := r.s.Resize(sz[0], sz[1], ptyevent.OriginHuman, "c")
		if !errors.Is(err, ErrResizeTooLarge) || !strings.Contains(err.Error(), "shrink the window") {
			t.Fatalf("resize %v: want actionable ErrResizeTooLarge, got %v", sz, err)
		}
	}
	if err := r.s.Resize(MaxCols, 256, ptyevent.OriginHuman, "c"); err != nil {
		t.Fatalf("largest legal size refused: %v", err)
	}
}
