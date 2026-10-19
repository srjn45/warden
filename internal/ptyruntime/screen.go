package ptyruntime

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Screen is the writer's terminal model: enough VT100/xterm to render a
// snapshot of what a CLI's TUI shows (spec §5 snapshot markers) and to answer
// tmuxproc.Host.CapturePane. It handles printable text with deferred autowrap,
// wide (East Asian / emoji, two cells) and combining (zero-width, attached to
// the previous cell) characters, CR/LF/BS/TAB, cursor movement
// (CUU/CUD/CUF/CUB/CNL/CPL/CHA/VPA/CUP/HVP), erase in display/line (ED/EL),
// insert/delete of lines and characters (IL/DL/ICH/DCH/ECH), scroll regions
// (DECSTBM) and scrolling (SU/SD, IND/RI), save/restore cursor, the alternate
// screen and SGR attributes (kept per cell). String is the plain, redacted-safe
// text used for snapshots and journals; StyledString re-emits SGR for the local
// authorized view only. It is fed redacted bytes, so neither ever holds a
// secret the journal does not.
type Screen struct {
	cols, rows int
	main, alt  [][]cell
	grid       *[][]cell // active buffer
	x, y       int
	wrapNext   bool
	savedX     int
	savedY     int
	inAlt      bool
	top, bot   int // scroll region, inclusive rows
	pen        pen

	st     int // parser state
	params []byte
	carry  []byte // incomplete UTF-8 rune
}

// pen is the SGR state a cell was written with. Colors: 0 = default,
// 1..256 = palette index+1, colorRGB|rgb = truecolor.
type pen struct {
	fg, bg uint32
	attr   uint16
}

const colorRGB uint32 = 1 << 24

const (
	attrBold uint16 = 1 << iota
	attrDim
	attrItalic
	attrUnderline
	attrBlink
	attrReverse
	attrStrike
)

// cell is one screen position. r == 0 marks the right half of a wide
// character; ext holds combining marks that follow r.
type cell struct {
	r   rune
	ext string
	pen pen
}

var blankCell = cell{r: ' '}

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

func blank(cols, rows int) [][]cell {
	g := make([][]cell, rows)
	for i := range g {
		g[i] = blankLine(cols)
	}
	return g
}

func blankLine(cols int) []cell {
	l := make([]cell, cols)
	for i := range l {
		l[i] = blankCell
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
	fit := func(g [][]cell) [][]cell {
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
	s.top, s.bot = 0, rows-1
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

// String renders the active buffer as plain text: one line per row, trailing
// blanks and trailing empty rows trimmed. Attributes are dropped (this is the
// form the journal and every redacted view carry).
func (s *Screen) String() string { return s.render(false) }

// StyledString is String with each run's SGR attributes re-emitted, for the
// local authorized view (a snapshot a person looks at on their own terminal).
func (s *Screen) StyledString() string { return s.render(true) }

func (s *Screen) render(styled bool) string {
	lines := make([]string, len(*s.grid))
	for i, row := range *s.grid {
		end := len(row)
		for end > 0 && row[end-1].r == ' ' && row[end-1].ext == "" && (!styled || row[end-1].pen == (pen{})) {
			end--
		}
		var b strings.Builder
		cur := pen{}
		for _, c := range row[:end] {
			if c.r == 0 {
				continue // right half of a wide character
			}
			if styled && c.pen != cur {
				b.WriteString(sgr(c.pen))
				cur = c.pen
			}
			b.WriteRune(c.r)
			b.WriteString(c.ext)
		}
		if styled && cur != (pen{}) {
			b.WriteString("\x1b[0m")
		}
		lines[i] = b.String()
	}
	n := len(lines)
	for n > 0 && lines[n-1] == "" {
		n--
	}
	return strings.Join(lines[:n], "\n")
}

// sgr renders p as one SGR sequence that fully sets it (reset first).
func sgr(p pen) string {
	ps := []string{"0"}
	for _, a := range []struct {
		bit uint16
		on  string
	}{{attrBold, "1"}, {attrDim, "2"}, {attrItalic, "3"}, {attrUnderline, "4"}, {attrBlink, "5"}, {attrReverse, "7"}, {attrStrike, "9"}} {
		if p.attr&a.bit != 0 {
			ps = append(ps, a.on)
		}
	}
	color := func(c uint32, base string) {
		switch {
		case c == 0:
		case c&colorRGB != 0:
			ps = append(ps, base+"8;2;"+strconv.Itoa(int(c>>16&0xff))+";"+strconv.Itoa(int(c>>8&0xff))+";"+strconv.Itoa(int(c&0xff)))
		case c-1 < 8:
			ps = append(ps, base+strconv.Itoa(int(c-1)))
		case c-1 < 16:
			ps = append(ps, map[string]string{"3": "9", "4": "10"}[base]+strconv.Itoa(int(c-1-8)))
		default:
			ps = append(ps, base+"8;5;"+strconv.Itoa(int(c-1)))
		}
	}
	color(p.fg, "3")
	color(p.bg, "4")
	return "\x1b[" + strings.Join(ps, ";") + "m"
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
			if s.y == s.top {
				s.scrollDown(1)
			} else if s.y > 0 {
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
			s.top, s.bot, s.pen = 0, s.rows-1, pen{}
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
	w := runeWidth(r)
	if w == 0 {
		s.combine(r)
		return
	}
	if s.wrapNext || (w == 2 && s.x == s.cols-1) {
		s.x = 0
		s.lineFeed()
	}
	row := (*s.grid)[s.y]
	s.clearWide(row, s.x)
	if w == 2 {
		s.clearWide(row, s.x+1)
		row[s.x] = cell{r: r, pen: s.pen}
		row[s.x+1] = cell{r: 0, pen: s.pen}
	} else {
		row[s.x] = cell{r: r, pen: s.pen}
	}
	if s.x+w >= s.cols {
		s.x, s.wrapNext = s.cols-1, true
	} else {
		s.x += w
	}
}

// clearWide blanks the other half of a wide character that a write at x is
// about to split.
func (s *Screen) clearWide(row []cell, x int) {
	if x < 0 || x >= len(row) {
		return
	}
	if row[x].r == 0 && x > 0 {
		row[x-1] = blankCell
	}
	if x+1 < len(row) && row[x+1].r == 0 && runeWidth(row[x].r) == 2 {
		row[x+1] = blankCell
	}
}

// combine attaches a zero-width mark to the previously written cell.
func (s *Screen) combine(r rune) {
	x := s.x
	if !s.wrapNext {
		x--
	}
	row := (*s.grid)[s.y]
	for x > 0 && row[x].r == 0 {
		x--
	}
	if x < 0 || x >= len(row) {
		return // nothing to attach to
	}
	row[x].ext += string(r)
}

func (s *Screen) lineFeed() {
	s.wrapNext = false
	if s.y == s.bot {
		s.scrollUp(1)
		return
	}
	if s.y < s.rows-1 {
		s.y++
	}
}

// scrollUp scrolls the region up n lines (content moves up, blanks enter at
// the bottom).
func (s *Screen) scrollUp(n int) {
	g := *s.grid
	n = min(max(n, 0), s.bot-s.top+1)
	copy(g[s.top:], g[s.top+n:s.bot+1])
	for i := s.bot - n + 1; i <= s.bot; i++ {
		g[i] = blankLine(s.cols)
	}
}

// scrollDown scrolls the region down n lines.
func (s *Screen) scrollDown(n int) {
	g := *s.grid
	n = min(max(n, 0), s.bot-s.top+1)
	copy(g[s.top+n:s.bot+1], g[s.top:s.bot+1-n])
	for i := s.top; i < s.top+n; i++ {
		g[i] = blankLine(s.cols)
	}
}

// runeWidth is the number of terminal cells r occupies: 0 for combining and
// format characters, 2 for East Asian wide/fullwidth and emoji, else 1.
func runeWidth(r rune) int {
	switch {
	case r < 0x300:
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf), r >= 0xFE00 && r <= 0xFE0F, r == 0x200B:
		return 0
	}
	switch {
	case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0x303E, r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF, r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3, r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, r >= 0x1F680 && r <= 0x1F6FF, r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x1FA70 && r <= 0x1FAFF, r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

func (s *Screen) csi(final byte) {
	raw := string(s.params)
	private := strings.HasPrefix(raw, "?")
	raw = strings.TrimLeft(raw, "?<=>")
	var ps []int
	for _, f := range strings.Split(raw, ";") {
		n, _ := strconv.Atoi(strings.SplitN(f, ":", 2)[0])
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
	case 'm':
		s.setPen(ps)
		return
	case 'r': // DECSTBM: scroll region, cursor homes
		top, bot := arg(0, 1)-1, arg(1, s.rows)-1
		if top < bot && bot < s.rows {
			s.top, s.bot = top, bot
		} else {
			s.top, s.bot = 0, s.rows-1
		}
		s.x, s.y = 0, 0
	case 'L': // IL: only inside the region
		if s.y >= s.top && s.y <= s.bot {
			save := s.top
			s.top = s.y
			s.scrollDown(arg(0, 1))
			s.top = save
		}
		s.x = 0
	case 'M': // DL
		if s.y >= s.top && s.y <= s.bot {
			save := s.top
			s.top = s.y
			s.scrollUp(arg(0, 1))
			s.top = save
		}
		s.x = 0
	case 'S':
		s.scrollUp(arg(0, 1))
		return
	case 'T':
		s.scrollDown(arg(0, 1))
		return
	case '@': // ICH
		row := (*s.grid)[s.y]
		n := min(arg(0, 1), s.cols-s.x)
		copy(row[s.x+n:], row[s.x:s.cols-n])
		for i := s.x; i < s.x+n; i++ {
			row[i] = blankCell
		}
		return
	case 'P': // DCH
		row := (*s.grid)[s.y]
		n := min(arg(0, 1), s.cols-s.x)
		copy(row[s.x:], row[s.x+n:])
		for i := s.cols - n; i < s.cols; i++ {
			row[i] = blankCell
		}
		return
	case 'X': // ECH
		row := (*s.grid)[s.y]
		for i := s.x; i < min(s.x+arg(0, 1), s.cols); i++ {
			row[i] = blankCell
		}
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

// setPen applies one SGR sequence.
func (s *Screen) setPen(ps []int) {
	if len(ps) == 0 {
		ps = []int{0}
	}
	for i := 0; i < len(ps); i++ {
		p := ps[i]
		switch {
		case p == 0:
			s.pen = pen{}
		case p == 1:
			s.pen.attr |= attrBold
		case p == 2:
			s.pen.attr |= attrDim
		case p == 3:
			s.pen.attr |= attrItalic
		case p == 4:
			s.pen.attr |= attrUnderline
		case p == 5 || p == 6:
			s.pen.attr |= attrBlink
		case p == 7:
			s.pen.attr |= attrReverse
		case p == 9:
			s.pen.attr |= attrStrike
		case p == 22:
			s.pen.attr &^= attrBold | attrDim
		case p == 23:
			s.pen.attr &^= attrItalic
		case p == 24:
			s.pen.attr &^= attrUnderline
		case p == 25:
			s.pen.attr &^= attrBlink
		case p == 27:
			s.pen.attr &^= attrReverse
		case p == 29:
			s.pen.attr &^= attrStrike
		case p >= 30 && p <= 37:
			s.pen.fg = uint32(p-30) + 1
		case p >= 90 && p <= 97:
			s.pen.fg = uint32(p-90+8) + 1
		case p == 39:
			s.pen.fg = 0
		case p >= 40 && p <= 47:
			s.pen.bg = uint32(p-40) + 1
		case p >= 100 && p <= 107:
			s.pen.bg = uint32(p-100+8) + 1
		case p == 49:
			s.pen.bg = 0
		case p == 38 || p == 48:
			var c uint32
			switch {
			case i+2 < len(ps) && ps[i+1] == 5:
				c = uint32(ps[i+2]&0xff) + 1
				i += 2
			case i+4 < len(ps) && ps[i+1] == 2:
				c = colorRGB | uint32(ps[i+2]&0xff)<<16 | uint32(ps[i+3]&0xff)<<8 | uint32(ps[i+4]&0xff)
				i += 4
			default:
				i = len(ps)
			}
			if p == 38 {
				s.pen.fg = c
			} else {
				s.pen.bg = c
			}
		}
	}
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
		l[i] = blankCell
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
