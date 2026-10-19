package ptyruntime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
)

// Retention bounds one session's journal (spec §5). The defaults are generous:
// they only bite on pathological sessions.
type Retention struct {
	// MaxBytes caps the journal on disk. Past it, whole segments (each starts
	// at a snapshot) are dropped from the front and a trim marker appended.
	MaxBytes int64
	// KeepSnapshots caps how many snapshot-led segments are retained.
	KeepSnapshots int
	// MaxAge is how long an ended journal is kept (see Journal.Expired); the
	// caller deletes it with the session's archive.
	MaxAge time.Duration
}

// DefaultRetention is the contract default.
var DefaultRetention = Retention{MaxBytes: 64 << 20, KeepSnapshots: 8, MaxAge: 14 * 24 * time.Hour}

func (r Retention) withDefaults() Retention {
	if r.MaxBytes <= 0 {
		r.MaxBytes = DefaultRetention.MaxBytes
	}
	if r.KeepSnapshots <= 0 {
		r.KeepSnapshots = DefaultRetention.KeepSnapshots
	}
	if r.MaxAge <= 0 {
		r.MaxAge = DefaultRetention.MaxAge
	}
	return r
}

// Journal errors.
var (
	// ErrTrimmed: the requested seq is no longer retained.
	ErrTrimmed = errors.New("ptyruntime: seq no longer retained")
	// ErrOrder: the event breaks the stream ordering contract.
	ErrOrder = errors.New("ptyruntime: event breaks stream order")
	// ErrJournalClosed: the journal was closed.
	ErrJournalClosed = errors.New("ptyruntime: journal closed")
)

// Recovery reports what Open had to repair.
type Recovery struct {
	// TruncatedBytes is how many bytes of a torn/invalid tail were cut.
	TruncatedBytes int64
	// DroppedSegments counts segments after a corrupt one that were removed.
	DroppedSegments int
}

type segment struct {
	path     string
	first    uint64 // seq of the first event
	last     uint64 // seq of the last event (0 = empty)
	bytes    int64
	snapshot bool // first event is a snapshot marker
}

// streamTail is the minimum ordering state needed to check the next event.
type streamTail struct {
	last  ptyevent.Event // zero = empty journal
	gapTo uint64         // pending gap marker's ToSeq
	ended bool           // a terminal lifecycle event closed the current epoch
}

// check applies ValidateStream's rules to ev as the successor of t.last.
func (t streamTail) check(ev ptyevent.Event) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	if t.last.Seq == 0 {
		return nil
	}
	if vs := ptyevent.ValidateStream([]ptyevent.Event{t.last, ev}); len(vs) > 0 {
		return fmt.Errorf("%w: %s", ErrOrder, vs[0])
	}
	if t.ended && ev.Epoch == t.last.Epoch && (ev.Kind == ptyevent.KindOutput || ev.Kind == ptyevent.KindInput) {
		return fmt.Errorf("%w: %s after the process ended", ErrOrder, ev.Kind)
	}
	return nil
}

func (t *streamTail) advance(ev ptyevent.Event) {
	if ev.Epoch > t.last.Epoch {
		t.ended = false
	}
	t.gapTo = 0
	if ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerGap {
		t.gapTo = ev.Marker.ToSeq
	}
	if ev.Kind == ptyevent.KindLifecycle && ev.Lifecycle.Phase.Terminal() {
		t.ended = true
	}
	t.last = ev
}

// Journal is one session's append-only event log. It is safe for one writer
// and any number of concurrent readers.
type Journal struct {
	dir       string
	sessionID string
	ret       Retention

	mu       sync.Mutex
	segs     []segment
	f        *os.File // active (last) segment, opened for append
	tail     streamTail
	snapSeq  uint64 // newest snapshot marker's ToSeq
	resetSeq uint64 // newest reset marker's seq
	total    int64
	recovery Recovery
	closed   bool
}

const segPrefix, segSuffix = "seg-", ".jsonl"

func segName(first uint64) string { return fmt.Sprintf("%s%020d%s", segPrefix, first, segSuffix) }

// OpenJournal opens (creating if needed) the journal for sessionID in dir and
// recovers from a crash: a torn or invalid tail is truncated, and segments
// after a corrupt one are removed, so the retained stream always passes
// ValidateStream.
func OpenJournal(dir, sessionID string, ret Retention) (*Journal, error) {
	if sessionID == "" {
		return nil, ptyevent.ErrSession
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, sessionID: sessionID, ret: ret.withDefaults()}
	names, err := filepath.Glob(filepath.Join(dir, segPrefix+"*"+segSuffix))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	for i, name := range names {
		seg, ok, err := j.scan(name)
		if err != nil {
			return nil, err
		}
		if seg.last != 0 {
			j.segs = append(j.segs, seg)
			j.total += seg.bytes
		} else if err := os.Remove(name); err != nil {
			return nil, err
		}
		if !ok {
			for _, rest := range names[i+1:] {
				if err := os.Remove(rest); err != nil {
					return nil, err
				}
				j.recovery.DroppedSegments++
			}
			break
		}
	}
	if len(j.segs) > 0 {
		last := j.segs[len(j.segs)-1]
		if j.f, err = os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
			return nil, err
		}
	}
	return j, nil
}

// scan reads one segment, advancing j's tail. It truncates the file at the
// first undecodable, invalid or out-of-order record; ok=false reports that.
func (j *Journal) scan(path string) (seg segment, ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return seg, false, err
	}
	defer f.Close()
	seg.path = path
	r := bufio.NewReader(f)
	var off int64
	for {
		line, rerr := r.ReadBytes('\n')
		if rerr == io.EOF && len(line) == 0 {
			return seg, true, nil
		}
		var ev ptyevent.Event
		bad := rerr != nil // a record without its newline is torn
		if !bad {
			bad = json.Unmarshal(line, &ev) != nil || ev.SessionID != j.sessionID || j.tail.check(ev) != nil
		}
		if bad {
			st, serr := f.Stat()
			if serr != nil {
				return seg, false, serr
			}
			j.recovery.TruncatedBytes += st.Size() - off
			if err := f.Truncate(off); err != nil {
				return seg, false, err
			}
			return seg, false, f.Sync()
		}
		if seg.first == 0 {
			seg.first = ev.Seq
			seg.snapshot = ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerSnapshot
		}
		seg.last = ev.Seq
		seg.bytes += int64(len(line))
		off += int64(len(line))
		j.note(ev)
	}
}

// note updates the index state for an accepted event.
func (j *Journal) note(ev ptyevent.Event) {
	j.tail.advance(ev)
	if ev.Kind == ptyevent.KindMarker {
		switch ev.Marker.Type {
		case ptyevent.MarkerSnapshot:
			j.snapSeq = ev.Marker.ToSeq
		case ptyevent.MarkerReset:
			j.resetSeq = ev.Seq
		}
	}
}

// Recovery reports what OpenJournal repaired.
func (j *Journal) Recovery() Recovery { return j.recovery }

// SessionID is the session this journal belongs to.
func (j *Journal) SessionID() string { return j.sessionID }

// NextSeq is the seq the next appended event must carry (it skips the hole a
// pending gap marker names).
func (j *Journal) NextSeq() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.nextSeqLocked()
}

func (j *Journal) nextSeqLocked() uint64 {
	if j.tail.gapTo != 0 {
		return j.tail.gapTo + 1
	}
	return j.tail.last.Seq + 1
}

// Last returns the newest event (zero when empty).
func (j *Journal) Last() ptyevent.Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.tail.last
}

// Ended reports whether the newest epoch ended with a terminal lifecycle event.
func (j *Journal) Ended() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.tail.ended
}

// Window describes what the journal retains (for ptyevent.PlanResume).
func (j *Journal) Window() ptyevent.Window {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.windowLocked()
}

func (j *Journal) windowLocked() ptyevent.Window {
	w := ptyevent.Window{SessionID: j.sessionID, Epoch: j.tail.last.Epoch, NewestSeq: j.tail.last.Seq,
		SnapshotSeq: j.snapSeq, ResetSeq: j.resetSeq}
	if len(j.segs) > 0 {
		w.OldestSeq = j.segs[0].first
	}
	return w
}

// Bytes is the journal's size on disk.
func (j *Journal) Bytes() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.total
}

// Append durably appends ev. It rejects any event that fails ptyevent.Validate
// or the stream ordering rules (so unredacted events can never be persisted).
// A snapshot marker starts a new segment and may trigger retention, in which
// case a trim marker is appended after it; Append returns every event it
// wrote, in order, for the caller to fan out.
func (j *Journal) Append(ev ptyevent.Event) ([]ptyevent.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrJournalClosed
	}
	if ev.SessionID != j.sessionID {
		return nil, fmt.Errorf("%w: session %q in journal %q", ErrOrder, ev.SessionID, j.sessionID)
	}
	if err := j.tail.check(ev); err != nil {
		return nil, err
	}
	snapshot := ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerSnapshot
	if err := j.write(ev, snapshot); err != nil {
		return nil, err
	}
	out := []ptyevent.Event{ev}
	if snapshot {
		trim, err := j.retain()
		if err != nil {
			return out, err
		}
		if trim != nil {
			out = append(out, *trim)
		}
	}
	return out, nil
}

func (j *Journal) write(ev ptyevent.Event, rotate bool) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if j.f == nil || rotate {
		if j.f != nil {
			if err := j.f.Sync(); err != nil {
				return err
			}
			if err := j.f.Close(); err != nil {
				return err
			}
			j.f = nil
		}
		path := filepath.Join(j.dir, segName(ev.Seq))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		j.f = f
		j.segs = append(j.segs, segment{path: path, first: ev.Seq, snapshot: rotate})
	}
	if _, err := j.f.Write(line); err != nil {
		return err
	}
	seg := &j.segs[len(j.segs)-1]
	seg.last = ev.Seq
	seg.bytes += int64(len(line))
	j.total += int64(len(line))
	j.note(ev)
	return nil
}

// retain drops whole segments from the front while the journal is over its
// bounds, keeping at least the active segment, and appends a trim marker.
func (j *Journal) retain() (*ptyevent.Event, error) {
	snaps := 0
	for _, s := range j.segs {
		if s.snapshot {
			snaps++
		}
	}
	from := j.segs[0].first
	dropped := false
	for len(j.segs) > 1 && (j.total > j.ret.MaxBytes || snaps > j.ret.KeepSnapshots) {
		s := j.segs[0]
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		j.total -= s.bytes
		if s.snapshot {
			snaps--
		}
		j.segs = j.segs[1:]
		dropped = true
	}
	if !dropped {
		return nil, nil
	}
	last := j.tail.last
	ev := ptyevent.Event{V: ptyevent.SchemaVersion, SessionID: j.sessionID, Epoch: last.Epoch,
		Seq: j.nextSeqLocked(), MonoNS: last.MonoNS, Wall: time.Now(), Kind: ptyevent.KindMarker,
		Marker: &ptyevent.Marker{Type: ptyevent.MarkerTrim, FromSeq: from, ToSeq: j.segs[0].first - 1}, Redacted: true}
	if err := j.write(ev, false); err != nil {
		return nil, err
	}
	return &ev, nil
}

// Read calls fn for every retained event with seq >= from, in order, up to
// the newest event at the time of the call. It returns ErrTrimmed when from
// is older than the retention window (or retention removed a segment while it
// was being read).
func (j *Journal) Read(from uint64, fn func(ptyevent.Event) error) error {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return ErrJournalClosed
	}
	segs := append([]segment(nil), j.segs...)
	upTo := j.tail.last.Seq
	j.mu.Unlock()
	if len(segs) == 0 || from > upTo {
		return nil
	}
	if from < segs[0].first {
		return fmt.Errorf("%w: seq %d < oldest %d", ErrTrimmed, from, segs[0].first)
	}
	i := sort.Search(len(segs), func(i int) bool { return segs[i].first > from }) - 1
	for _, s := range segs[i:] {
		if err := readSegment(s.path, from, upTo, fn); err != nil {
			return err
		}
	}
	return nil
}

func readSegment(path string, from, upTo uint64, fn func(ptyevent.Event) error) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: segment %s removed", ErrTrimmed, filepath.Base(path))
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return nil // a concurrent append past upTo may still be in flight
		}
		if err != nil {
			return err
		}
		var ev ptyevent.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return err
		}
		if ev.Seq > upTo {
			return nil
		}
		if ev.Seq < from {
			continue
		}
		if err := ev.Validate(); err != nil {
			return err
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}

// Expired reports whether an ended journal is past Retention.MaxAge.
func (j *Journal) Expired(now time.Time) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.tail.ended && !j.tail.last.Wall.IsZero() && now.Sub(j.tail.last.Wall) > j.ret.MaxAge
}

// Close syncs and closes the active segment.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	if j.f == nil {
		return nil
	}
	err := j.f.Sync()
	if cerr := j.f.Close(); err == nil {
		err = cerr
	}
	j.f = nil
	return err
}

// Remove closes the journal and deletes it from disk (session archive/expiry).
func (j *Journal) Remove() error {
	if err := j.Close(); err != nil {
		return err
	}
	return os.RemoveAll(j.dir)
}
