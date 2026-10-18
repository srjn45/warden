package ptyevent

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitUTF8(t *testing.T) {
	euro := []byte("€")  // e2 82 ac
	emoji := []byte("🙂") // f0 9f 99 82
	cases := []struct {
		name       string
		carry, in  []byte
		emit, rest []byte
	}{
		{"ascii", nil, []byte("abc"), []byte("abc"), nil},
		{"complete multibyte", nil, euro, euro, nil},
		{"split after lead byte", nil, append([]byte("a"), euro[0]), []byte("a"), euro[:1]},
		{"split mid sequence", nil, append([]byte("a"), euro[:2]...), []byte("a"), euro[:2]},
		{"carry completes", euro[:2], append(euro[2:], 'b'), append(append([]byte{}, euro...), 'b'), nil},
		{"4-byte split 3+1", nil, emoji[:3], []byte{}, emoji[:3]},
		{"4-byte carry completes", emoji[:3], emoji[3:], emoji, nil},
		{"invalid byte is not held", nil, []byte{'a', 0xff}, []byte{'a', 0xff}, nil},
		{"lone continuation byte is not held", nil, []byte{'a', 0x82}, []byte{'a', 0x82}, nil},
		{"empty", nil, nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			emit, rest := SplitUTF8(c.carry, c.in)
			if !bytes.Equal(emit, c.emit) || !bytes.Equal(rest, c.rest) {
				t.Fatalf("SplitUTF8 = %q,%q want %q,%q", emit, rest, c.emit, c.rest)
			}
		})
	}
}

// TestSplitUTF8EveryCut feeds a multilingual string in every possible
// two-chunk split and in 1-byte chunks: every emitted event must be valid
// UTF-8 and the concatenation must reproduce the input exactly.
func TestSplitUTF8EveryCut(t *testing.T) {
	src := []byte("ok ✓ → 日本語 🙂 ü done\x1b[0m")
	check := func(chunks [][]byte) {
		t.Helper()
		var carry, got []byte
		for _, ch := range chunks {
			var emit []byte
			emit, carry = SplitUTF8(carry, ch)
			if !utf8.Valid(emit) {
				t.Fatalf("emitted invalid UTF-8 %q (chunks %q)", emit, chunks)
			}
			if len(carry) >= utf8.UTFMax {
				t.Fatalf("held %d bytes", len(carry))
			}
			got = append(got, emit...)
		}
		got = append(got, carry...)
		if !bytes.Equal(got, src) {
			t.Fatalf("reassembled %q, want %q", got, src)
		}
	}
	for i := 0; i <= len(src); i++ {
		check([][]byte{src[:i], src[i:]})
	}
	var ones [][]byte
	for i := range src {
		ones = append(ones, src[i:i+1])
	}
	check(ones)
	// The caller reuses its read buffer: rest must not alias it.
	buf := []byte("a\xe2\x82")
	_, rest := SplitUTF8(nil, buf)
	buf[1] = 'X'
	if rest[0] != 0xe2 {
		t.Fatal("rest aliases the caller's buffer")
	}
}

func kinds(cs []Control) []ControlKind {
	out := make([]ControlKind, len(cs))
	for i, c := range cs {
		out[i] = c.Kind
	}
	return out
}

func TestControlScanner(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		text   string
		kinds  []ControlKind
	}{
		{"plain text keeps newline and tab, drops CR", []string{"a\tb\r\nc"}, "a\tb\nc", nil},
		{"SGR color", []string{"\x1b[1;31mred\x1b[0m"}, "red", []ControlKind{ControlCSI, ControlCSI}},
		{"alt screen enter/exit", []string{"\x1b[?1049h", "x", "\x1b[?1049l"}, "x", []ControlKind{ControlAltScreenEnter, ControlAltScreenExit}},
		{"legacy alt screen", []string{"\x1b[?47h\x1b[?47l"}, "", []ControlKind{ControlAltScreenEnter, ControlAltScreenExit}},
		{"clear screen", []string{"\x1b[2J\x1b[H"}, "", []ControlKind{ControlClearScreen, ControlCSI}},
		{"title BEL", []string{"\x1b]0;claude: fix tests\a"}, "", []ControlKind{ControlTitle}},
		{"title ST", []string{"\x1b]2;agy\x1b\\"}, "", []ControlKind{ControlTitle}},
		{"hyperlink OSC 8 is generic", []string{"\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\"}, "link", []ControlKind{ControlOSC, ControlOSC}},
		{"bell", []string{"done\a"}, "done", []ControlKind{ControlBell}},
		{"bracketed paste + cursor", []string{"\x1b[?2004h\x1b[?25l\x1b[?25h\x1b[?2004l"}, "", []ControlKind{ControlBracketPasteOn, ControlCursorHide, ControlCursorShow, ControlBracketPasteOf}},
		{"charset designation ESC ( B", []string{"\x1b(Bx"}, "x", []ControlKind{ControlESC}},
		{"keypad mode ESC =", []string{"\x1b=y"}, "y", []ControlKind{ControlESC}},
		{"DCS string", []string{"\x1bPq#0;2;0;0;0\x1b\\z"}, "z", []ControlKind{ControlString}},
		{"CSI split across chunks", []string{"a\x1b[", "?104", "9hb"}, "ab", []ControlKind{ControlAltScreenEnter}},
		{"OSC split inside ST", []string{"\x1b]0;t\x1b", "\\after"}, "after", []ControlKind{ControlTitle}},
		{"ESC split from its final", []string{"\x1b", "[2J"}, "", []ControlKind{ControlClearScreen}},
		{"CAN aborts CSI", []string{"\x1b[12\x18ok"}, "ok", []ControlKind{ControlAborted}},
		{"ESC inside CSI restarts", []string{"\x1b[1\x1b[2Jz"}, "z", []ControlKind{ControlAborted, ControlClearScreen}},
		{"utf8 passes through", []string{"→ 日本"}, "→ 日本", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s ControlScanner
			var text []byte
			var all []Control
			for _, ch := range c.chunks {
				tx, cs := s.Feed([]byte(ch))
				text = append(text, tx...)
				all = append(all, cs...)
			}
			if string(text) != c.text {
				t.Errorf("text = %q, want %q", text, c.text)
			}
			if !reflect.DeepEqual(kinds(all), c.kinds) && !(len(all) == 0 && len(c.kinds) == 0) {
				t.Errorf("controls = %v, want %v", kinds(all), c.kinds)
			}
			if s.Pending() {
				t.Errorf("scanner left a sequence open")
			}
		})
	}
}

func TestControlScannerDetails(t *testing.T) {
	var s ControlScanner
	_, cs := s.Feed([]byte("\x1b]0;my title\a"))
	if len(cs) != 1 || cs[0].Param != "my title" || cs[0].End != len("\x1b]0;my title\a") {
		t.Fatalf("title control = %+v", cs)
	}
	// A sequence that started in an earlier chunk reports End in THIS chunk and
	// the whole sequence in Raw.
	s = ControlScanner{}
	s.Feed([]byte("\x1b[?10"))
	if !s.Pending() {
		t.Fatal("want pending sequence across chunk")
	}
	_, cs = s.Feed([]byte("49hX"))
	if len(cs) != 1 || cs[0].End != 3 || cs[0].Raw != "\x1b[?1049h" {
		t.Fatalf("split control = %+v", cs)
	}
	// Adversarial: an unterminated OSC must not swallow the stream forever.
	s = ControlScanner{}
	_, cs = s.Feed([]byte("\x1b]0;" + strings.Repeat("A", MaxControlLen+10)))
	if len(cs) != 1 || cs[0].Kind != ControlAborted {
		t.Fatalf("overlong OSC = %v", kinds(cs))
	}
	tx, _ := s.Feed([]byte("visible"))
	if string(tx) != "visible" {
		t.Fatalf("after abort text = %q", tx)
	}
}
