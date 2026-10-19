package ptyruntime

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Screen is the writer's minimal terminal model: enough VT100/xterm to render
// a redacted snapshot of what a CLI's TUI shows (spec §5 snapshot markers) and
// to answer tmuxproc.Host.CapturePane. It handles printable text with
// deferred autowrap, CR/LF/BS/TAB, cursor movement (CUU/CUD/CUF/CUB/CNL/CPL/
// CHA/VPA/CUP/HVP), erase in display/line (ED/EL), scroll region-less
// scrolling, save/restore cursor and the alternate screen. Every rune is one
// cell wide; colors and attributes are ignored. It is fed redacted bytes only,
// so a snapshot never holds a secret the journal does not.
type Screen struct {
	cols, rows int
	main, alt  [][]rune
	grid       *[][]rune // active buffer
	x, y       int
	wrapNext   bool
	savedX     int
	savedY     int
	inAlt      bool

	st     int // parser state
	params []byte
	carry  []byte // incomplete UTF-8 rune
}

const (
	scGround = iota
	scEsc
	scCSI
	scOSC
	scOSCEsc
	scStr
	scStrEsc
	scEscInter
)

// NewScreen returns a blank cols×rows screen.
func NewScreen(cols, rows int) *Screen {
	s := &Screen{}
	s.Resize(cols, rows)
	return s
}

func blank(cols, rows int) [][]rune {
	g := make([][]rune, rows)
	for i := range g {
		g[i] = blankLine(cols)
	}
	return g
}

func blankLine(cols int) []rune {
	l := make([]rune, cols)
	for i := range l {
		l[i] = ' '
	}
	return l
}

// Size returns the screen size.
func (s *Screen) Size() (cols, rows int) { return s.cols, s.rows }

// Resize changes the size, cropping or padding both buffers (no reflow).
func (s *Screen) Resize(cols, rows int) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	fit := func(g [][]rune) [][]rune {
		if g == nil {
			return blank(cols, rows)
		}
		if len(g) > rows {
			g = g[len(g)-rows:] // keep the bottom, where the cursor usually is
		}
		for len(g) < rows {
			g = append(g, blankLine(cols))
		}
		for i, l := range g {
			if len(l) >= cols {
				g[i] = l[:cols]
			} else {
				g[i] = append(l, blankLine(cols-len(l))...)
			}
		}
		return g
	}
	shift := 0
	if s.main != nil && s.rows > rows {
		shift = s.rows - rows
	}
	s.main, s.alt = fit(s.main), fit(s.alt)
	s.cols, s.rows = cols, rows
	if s.inAlt {
		s.grid = &s.alt
	} else {
		s.grid = &s.main
	}
	s.y -= shift
	s.clamp()
}

func (s *Screen) clamp() {
	s.x = min(max(s.x, 0), s.cols-1)
	s.y = min(max(s.y, 0), s.rows-1)
	s.wrapNext = false
}

// String renders the active buffer: one line per row, trailing blanks and
// trailing empty rows trimmed.
func (s *Screen) String() string {
	lines := make([]string, len(*s.grid))
	for i, l := range *s.grid {
		lines[i] = strings.TrimRight(string(l), " ")
	}
	n := len(lines)
	for n > 0 && lines[n-1] == "" {
		n--
	}
	return strings.Join(lines[:n], "\n")
}

// Write feeds output bytes to the model. It never fails.
func (s *Screen) Write(p []byte) (int, error) {
	buf := p
	if len(s.carry) > 0 {
		buf = append(s.carry, p...)
		s.carry = nil
	}
	for i := 0; i < len(buf); {
		b := buf[i]
		if s.st == scGround && b >= 0x80 {
			if !utf8.FullRune(buf[i:]) {
				s.carry = append([]byte(nil), buf[i:]...)
				break
			}
			r, n := utf8.DecodeRune(buf[i:])
			s.put(r)
			i += n
			continue
		}
		s.byte(b)
		i++
	}
	return len(p), nil
}

func (s *Screen) byte(b byte) {
	if s.st != scGround && (b == 0x18 || b == 0x1a) {
		s.st = scGround
		return
	}
	switch s.st {
	case scGround:
		switch b {
		case 0x1b:
			s.st = scEsc
		case '\r':
			s.x, s.wrapNext = 0, false
		case '\n', 0x0b, 0x0c:
			s.lineFeed()
		case '\b':
			if s.x > 0 {
				s.x--
			}
			s.wrapNext = false
		case '\t':
			s.x = min((s.x/8+1)*8, s.cols-1)
		default:
			if b >= 0x20 && b != 0x7f {
				s.put(rune(b))
			}
		}
	case scEsc:
		s.st = scGround
		switch b {
		case '[':
			s.st, s.params = scCSI, s.params[:0]
		case ']':
			s.st = scOSC
		case 'P', '_', '^', 'X':
			s.st = scStr
		case '7':
			s.savedX, s.savedY = s.x, s.y
		case '8':
			s.x, s.y = s.savedX, s.savedY
			s.clamp()
		case 'M': // reverse index
			if s.y == 0 {
				g := *s.grid
				copy(g[1:], g[:len(g)-1])
				g[0] = blankLine(s.cols)
			} else {
				s.y--
			}
		case 'D':
			s.lineFeed()
		case 'E':
			s.x = 0
			s.lineFeed()
		case 'c':
			s.main, s.alt, s.inAlt = blank(s.cols, s.rows), blank(s.cols, s.rows), false
			s.grid, s.x, s.y = &s.main, 0, 0
			s.clamp()
		default:
			if b >= 0x20 && b <= 0x2f {
				s.st = scEscInter
			}
		}
	case scEscInter:
		if b < 0x20 || b > 0x2f {
			s.st = scGround
		}
	case scCSI:
		switch {
		case b >= 0x40 && b <= 0x7e:
			s.st = scGround
			s.csi(b)
		case b == 0x1b:
			s.st = scEsc
		default:
			if len(s.params) < 64 {
				s.params = append(s.params, b)
			}
		}
	case scOSC:
		switch b {
		case 0x07:
			s.st = scGround
		case 0x1b:
			s.st = scOSCEsc
		}
	case scOSCEsc:
		if b == '\\' {
			s.st = scGround
		} else {
			s.st = scOSC
		}
	case scStr:
		if b == 0x1b {
			s.st = scStrEsc
		}
	case scStrEsc:
		if b == '\\' {
			s.st = scGround
		} else {
			s.st = scStr
		}
	}
}

func (s *Screen) put(r rune) {
	if s.wrapNext {
		s.x = 0
		s.lineFeed()
	}
	(*s.grid)[s.y][s.x] = r
	if s.x == s.cols-1 {
		s.wrapNext = true
	} else {
		s.x++
	}
}

func (s *Screen) lineFeed() {
	s.wrapNext = false
	if s.y < s.rows-1 {
		s.y++
		return
	}
	g := *s.grid
	copy(g, g[1:])
	g[len(g)-1] = blankLine(s.cols)
}

func (s *Screen) csi(final byte) {
	raw := string(s.params)
	private := strings.HasPrefix(raw, "?")
	raw = strings.TrimLeft(raw, "?<=>")
	var ps []int
	for _, f := range strings.Split(raw, ";") {
		n, _ := strconv.Atoi(f)
		ps = append(ps, n)
	}
	arg := func(i, def int) int {
		if i < len(ps) && ps[i] > 0 {
			return ps[i]
		}
		return def
	}
	if private {
		if final == 'h' || final == 'l' {
			for _, p := range ps {
				if p == 1049 || p == 1047 || p == 47 {
					s.altScreen(final == 'h', p == 1049)
				}
			}
		}
		return
	}
	switch final {
	case 'A':
		s.y -= arg(0, 1)
	case 'B', 'e':
		s.y += arg(0, 1)
	case 'C', 'a':
		s.x += arg(0, 1)
	case 'D':
		s.x -= arg(0, 1)
	case 'E':
		s.x, s.y = 0, s.y+arg(0, 1)
	case 'F':
		s.x, s.y = 0, s.y-arg(0, 1)
	case 'G', '`':
		s.x = arg(0, 1) - 1
	case 'd':
		s.y = arg(0, 1) - 1
	case 'H', 'f':
		s.y, s.x = arg(0, 1)-1, arg(1, 1)-1
	case 'J':
		s.eraseDisplay(arg(0, 0))
		return
	case 'K':
		s.eraseLine(arg(0, 0))
		return
	case 's':
		s.savedX, s.savedY = s.x, s.y
		return
	case 'u':
		s.x, s.y = s.savedX, s.savedY
	default:
		return
	}
	s.clamp()
}

func (s *Screen) eraseLine(mode int) {
	l := (*s.grid)[s.y]
	from, to := 0, s.cols
	switch mode {
	case 0:
		from = s.x
	case 1:
		to = s.x + 1
	}
	for i := from; i < to; i++ {
		l[i] = ' '
	}
}

func (s *Screen) eraseDisplay(mode int) {
	g := *s.grid
	switch mode {
	case 0:
		s.eraseLine(0)
		for i := s.y + 1; i < s.rows; i++ {
			g[i] = blankLine(s.cols)
		}
	case 1:
		s.eraseLine(1)
		for i := 0; i < s.y; i++ {
			g[i] = blankLine(s.cols)
		}
	default:
		for i := range g {
			g[i] = blankLine(s.cols)
		}
	}
}

func (s *Screen) altScreen(on, saveCursor bool) {
	if on == s.inAlt {
		return
	}
	if on {
		if saveCursor {
			s.savedX, s.savedY = s.x, s.y
		}
		s.alt = blank(s.cols, s.rows)
		s.grid, s.inAlt = &s.alt, true
		return
	}
	s.grid, s.inAlt = &s.main, false
	if saveCursor {
		s.x, s.y = s.savedX, s.savedY
	}
	s.clamp()
}
