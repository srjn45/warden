package ptyevent

import (
	"bytes"
	"unicode/utf8"
)

// SplitUTF8 joins a held-back carry with a freshly read chunk and splits the
// result at the last complete UTF-8 boundary: emit is safe to publish as one
// Output event, rest is the incomplete trailing sequence to prepend to the next
// read. Invalid bytes are emitted as-is (they can never complete, so holding
// them would stall the stream). rest is always a fresh copy, so the caller may
// reuse its read buffer. At most utf8.UTFMax-1 bytes are ever held back; when
// the hold-back times out or the process exits the runtime flushes rest as an
// Output with Partial=true.
func SplitUTF8(carry, chunk []byte) (emit, rest []byte) {
	buf := chunk
	if len(carry) > 0 {
		buf = append(append(make([]byte, 0, len(carry)+len(chunk)), carry...), chunk...)
	}
	for i := len(buf) - 1; i >= 0 && i > len(buf)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(buf[i]) {
			continue
		}
		if !utf8.FullRune(buf[i:]) {
			return buf[:i], bytes.Clone(buf[i:])
		}
		break
	}
	return buf, nil
}

// ControlKind names a terminal control sequence class the contract cares about.
// Generic kinds (csi/osc/esc/string) are reported for every sequence; the
// semantic kinds are reported INSTEAD of the generic one when recognized.
type ControlKind string

const (
	ControlCSI            ControlKind = "csi"    // unrecognized CSI (cursor moves, SGR, …)
	ControlOSC            ControlKind = "osc"    // unrecognized OSC
	ControlESC            ControlKind = "esc"    // two-/three-byte escape (charset, keypad, …)
	ControlString         ControlKind = "string" // DCS/APC/PM/SOS string
	ControlBell           ControlKind = "bell"
	ControlAltScreenEnter ControlKind = "alt_screen_enter"
	ControlAltScreenExit  ControlKind = "alt_screen_exit"
	ControlClearScreen    ControlKind = "clear_screen"
	ControlTitle          ControlKind = "title"
	ControlBracketPasteOn ControlKind = "bracketed_paste_on"
	ControlBracketPasteOf ControlKind = "bracketed_paste_off"
	ControlCursorHide     ControlKind = "cursor_hide"
	ControlCursorShow     ControlKind = "cursor_show"
	ControlAborted        ControlKind = "aborted" // sequence cancelled (CAN/SUB/ESC) or over MaxControlLen
)

// MaxControlLen caps one control sequence. A longer one (adversarial or
// corrupt output) is reported as ControlAborted and parsing returns to text.
const MaxControlLen = 4096

// Control is one control sequence found in output. End is the byte offset in
// the chunk just past the sequence's final byte (a sequence may have started in
// an earlier chunk; Raw then holds all of it).
type Control struct {
	Kind ControlKind `json:"kind"`
	End  int         `json:"end"`
	Raw  string      `json:"raw,omitempty"`
	// Param carries the semantic payload: the title text for ControlTitle.
	Param string `json:"param,omitempty"`
}

type scanState int

const (
	stGround scanState = iota
	stEsc
	stEscInter
	stCSI
	stOSC
	stOSCEsc
	stStr
	stStrEsc
)

// ControlScanner is an incremental VT500-style tokenizer: it is fed raw output
// chunk by chunk, carries an unfinished sequence across chunk boundaries, and
// returns the printable text (controls removed; \n and \t kept, \r dropped)
// plus the controls seen. It does NOT model the screen — that is the runtime's
// terminal emulator — it only gives the journal and the classifiers a stable
// tokenization. The zero value is ready to use.
type ControlScanner struct {
	st  scanState
	raw []byte
}

// Feed scans one chunk.
func (s *ControlScanner) Feed(chunk []byte) (text []byte, controls []Control) {
	emit := func(k ControlKind, end int) {
		c := Control{Kind: k, End: end, Raw: string(s.raw)}
		if k == ControlOSC || k == ControlTitle {
			c.Kind, c.Param = classifyOSC(s.raw)
		}
		if k == ControlCSI {
			c.Kind = classifyCSI(s.raw)
		}
		controls = append(controls, c)
		s.raw = s.raw[:0]
		s.st = stGround
	}
	for i, b := range chunk {
		end := i + 1
		if s.st != stGround {
			if b == 0x18 || b == 0x1a { // CAN / SUB abort any sequence
				s.raw = append(s.raw, b)
				emit(ControlAborted, end)
				continue
			}
			s.raw = append(s.raw, b)
			if len(s.raw) > MaxControlLen {
				emit(ControlAborted, end)
				continue
			}
		}
		switch s.st {
		case stGround:
			switch {
			case b == 0x1b:
				s.raw = append(s.raw[:0], b)
				s.st = stEsc
			case b == 0x07:
				controls = append(controls, Control{Kind: ControlBell, End: end, Raw: "\a"})
			case b == '\n' || b == '\t':
				text = append(text, b)
			case b < 0x20 || b == 0x7f:
				// other C0 / DEL: no printable effect for the text stream
			default:
				text = append(text, b)
			}
		case stEsc:
			switch {
			case b == '[':
				s.st = stCSI
			case b == ']':
				s.st = stOSC
			case b == 'P' || b == '_' || b == '^' || b == 'X':
				s.st = stStr
			case b == 0x1b:
				s.raw = s.raw[:len(s.raw)-1]
				emit(ControlAborted, i)
				s.raw = append(s.raw, b)
				s.st = stEsc
			case b >= 0x20 && b <= 0x2f:
				s.st = stEscInter
			default:
				emit(ControlESC, end)
			}
		case stEscInter:
			if b < 0x20 || b > 0x2f {
				emit(ControlESC, end)
			}
		case stCSI:
			switch {
			case b == 0x1b:
				s.raw = s.raw[:len(s.raw)-1]
				emit(ControlAborted, i)
				s.raw = append(s.raw, b)
				s.st = stEsc
			case b >= 0x40 && b <= 0x7e:
				emit(ControlCSI, end)
			}
		case stOSC:
			switch b {
			case 0x07:
				emit(ControlOSC, end)
			case 0x1b:
				s.st = stOSCEsc
			}
		case stOSCEsc:
			if b == '\\' {
				emit(ControlOSC, end)
			} else {
				s.st = stOSC
			}
		case stStr:
			if b == 0x1b {
				s.st = stStrEsc
			}
		case stStrEsc:
			if b == '\\' {
				emit(ControlString, end)
			} else {
				s.st = stStr
			}
		}
	}
	return text, controls
}

// Pending reports whether a control sequence is open across the chunk boundary.
func (s *ControlScanner) Pending() bool { return s.st != stGround }

func classifyCSI(raw []byte) ControlKind {
	body := string(raw[2:]) // after ESC [
	switch body {
	case "?1049h", "?1047h", "?47h":
		return ControlAltScreenEnter
	case "?1049l", "?1047l", "?47l":
		return ControlAltScreenExit
	case "2J", "3J":
		return ControlClearScreen
	case "?2004h":
		return ControlBracketPasteOn
	case "?2004l":
		return ControlBracketPasteOf
	case "?25l":
		return ControlCursorHide
	case "?25h":
		return ControlCursorShow
	}
	return ControlCSI
}

func classifyOSC(raw []byte) (ControlKind, string) {
	body := raw[2:] // after ESC ]
	body = bytes.TrimSuffix(body, []byte{0x07})
	body = bytes.TrimSuffix(body, []byte{0x1b, '\\'})
	if bytes.HasPrefix(body, []byte("0;")) || bytes.HasPrefix(body, []byte("2;")) {
		return ControlTitle, string(body[2:])
	}
	return ControlOSC, ""
}
