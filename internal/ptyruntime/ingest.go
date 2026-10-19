package ptyruntime

import (
	"bytes"

	"github.com/srjn45/warden/internal/ptyevent"
)

// ingest is the writer-side output pipeline in contract order:
// SplitUTF8 → Redactor → ControlScanner (§4). It turns raw PTY reads into
// redacted Output payloads. It is owned by the session writer and is not safe
// for concurrent use.
type ingest struct {
	carry []byte // incomplete trailing UTF-8 sequence (≤ 3 bytes)
	red   ptyevent.Redactor
	scan  ptyevent.ControlScanner
}

// feed ingests one raw read. ok is false when everything is still held back.
func (in *ingest) feed(raw []byte) (out ptyevent.Output, reds []ptyevent.Redaction, ok bool) {
	emit, rest := ptyevent.SplitUTF8(in.carry, raw)
	in.carry = rest
	data, reds := in.red.Feed(emit)
	return in.finish(data, reds, false)
}

// flush force-releases everything held (idle timer, gap, process exit). An
// incomplete UTF-8 tail is released too and the event is marked Partial.
func (in *ingest) flush() (out ptyevent.Output, reds []ptyevent.Redaction, ok bool) {
	if len(in.carry) > 0 {
		data, r := in.red.Feed(in.carry)
		in.carry = nil
		tail, r2 := in.red.Flush()
		data, r = join(data, r, tail, r2)
		return in.finish(data, r, true)
	}
	data, r := in.red.Flush()
	return in.finish(data, r, false)
}

// pending reports whether flush would release anything.
func (in *ingest) pending() bool { return len(in.carry) > 0 || in.red.Held() > 0 }

// reset drops parser state after a discontinuity (dropped bytes): a control
// sequence or a held line can no longer be completed by what follows.
func (in *ingest) reset() {
	in.scan = ptyevent.ControlScanner{}
}

func (in *ingest) finish(data []byte, reds []ptyevent.Redaction, partial bool) (ptyevent.Output, []ptyevent.Redaction, bool) {
	if len(data) == 0 {
		return ptyevent.Output{}, nil, false
	}
	_, controls := in.scan.Feed(data)
	return ptyevent.Output{Data: bytes.Clone(data), Controls: controls, Partial: partial}, reds, true
}

// join concatenates two redacted chunks, shifting the second's spans.
func join(a []byte, ra []ptyevent.Redaction, b []byte, rb []ptyevent.Redaction) ([]byte, []ptyevent.Redaction) {
	out := append(append(make([]byte, 0, len(a)+len(b)), a...), b...)
	reds := append([]ptyevent.Redaction(nil), ra...)
	for _, r := range rb {
		r.Offset += len(a)
		reds = append(reds, r)
	}
	return out, reds
}
