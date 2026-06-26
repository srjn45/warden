package savings

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store is the append-only savings ledger: one JSON object per line in
// <dir>/ledger.jsonl. Append-only fits the access pattern — every emit is a
// single write, every report is a full scan over a window — and keeps the
// daemon the single writer without the read-modify-write a JSON-array file would
// need. A mutex serializes the daemon's concurrent emit callers; a corrupt line
// (partial write after a crash) is skipped on read rather than failing the whole
// report. Mirrors the audit writer's discipline (internal/audit).
type Store struct {
	mu   sync.Mutex
	path string
	// samples gates persistence of the opt-in RawSample/KeptSample provenance
	// pairs (config `savings_samples`, default off). When off, Record strips any
	// sample an emit site attached before writing — the ledger never stores raw or
	// kept output unless the operator opted in. sampleCount drives the 1-in-N
	// retention that bounds growth even when the gate is on. Both guarded by mu.
	samples     bool
	sampleCount int
}

// SetSampling toggles whether Record persists the opt-in provenance samples
// (config `savings_samples`). The daemon calls it when wiring the gate. When off,
// any sample an emit site attached is dropped before the event is written.
func (s *Store) SetSampling(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = on
}

// NewStore creates the savings dir (0700) and returns a ledger writer/reader
// rooted at <dir>/ledger.jsonl.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "ledger.jsonl")}, nil
}

// Record appends one event. It is the daemon-side sink the lifecycle emit points
// call through; the caller has already converted bytes→tokens via EstimateTokens.
// A zero-saving event is still recorded (it documents that the feature ran), but
// the caller may skip emitting one if it prefers. Errors are returned so the
// caller can log-and-continue — recording must never break the action it measures.
func (s *Store) Record(ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateSampleLocked(&ev)
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// Events returns every recorded event with TS >= since (since.IsZero ⇒ all),
// oldest-first. A missing ledger is not an error — it yields an empty slice, so
// `wd savings` on a fresh install reads as "nothing saved yet" rather than failing.
// Malformed lines are skipped (best-effort, mirrors the gauge's transcript scan).
func (s *Store) Events(since time.Time) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	// Events are tiny, but a line cap guards against a wedged write; match the
	// transcript scanner's generous buffer rather than the 64 KiB default.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // skip a partial/corrupt line rather than abort the report
		}
		if !since.IsZero() && ev.TS.Before(since) {
			continue
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// gateSampleLocked enforces the privacy + volume policy on an event's retained
// provenance samples before it is written: when sampling is off it strips both
// sides (the ledger never stores raw/kept output unconditionally); when on it
// keeps them only on ~1 in sampleEvery sample-eligible events so growth stays
// bounded. Events that carry no sample are left untouched (and don't advance the
// counter). Caller holds s.mu.
func (s *Store) gateSampleLocked(ev *Event) {
	if ev.RawSample == "" && ev.KeptSample == "" {
		return
	}
	if !s.samples {
		ev.RawSample, ev.KeptSample = "", ""
		return
	}
	s.sampleCount++
	if s.sampleCount%sampleEvery != 1 { // keep the 1st, then every Nth eligible event
		ev.RawSample, ev.KeptSample = "", ""
	}
}

// CalibrationSamples returns the retained provenance sample strings (both the raw
// and kept side of every event that carries them) since `since`, newest-first,
// for `wd savings --calibrate` to count against count_tokens. Only the truncated
// samples persisted under savings_samples are available — events whose samples
// were not retained contribute nothing, which is why calibration measures the
// workload from whatever real bytes the ledger kept. Best-effort like Events.
func (s *Store) CalibrationSamples(since time.Time) ([]string, error) {
	evs, err := s.Events(since)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(evs))
	for i := len(evs) - 1; i >= 0; i-- { // newest first
		e := evs[i]
		if e.RawSample != "" {
			out = append(out, e.RawSample)
		}
		if e.KeptSample != "" {
			out = append(out, e.KeptSample)
		}
	}
	return out, nil
}

// Calibration reads the persisted calibration sidecar next to the ledger (written
// by `wd savings --calibrate`). The bool is false (no error) when no calibration
// exists yet. The daemon calls this at report time to stamp the summary's basis
// and to refresh the live estimation factor.
func (s *Store) Calibration() (Calibration, bool, error) {
	return LoadCalibration(filepath.Dir(s.path))
}

// Summary reads the events since `since` and aggregates them. Convenience wrapper
// the daemon's GET /savings handler uses so the HTTP layer stays a thin shell.
func (s *Store) Summary(since time.Time) (Summary, error) {
	return s.Report(since, "", false)
}

// Report reads the events since `since` and aggregates them, optionally also
// attaching the zero-filled trend buckets (bucket = "hour"|"day"; "" ⇒ none)
// and/or the retained provenance samples (the audit view). It reads the ledger
// once and fans the events into each projection, so the GET /savings handler
// stays a thin shell over a single scan. The trend is built up to now so the line
// extends to the present even when the last saving was a while ago.
func (s *Store) Report(since time.Time, bucket string, samples bool) (Summary, error) {
	evs, err := s.Events(since)
	if err != nil {
		return Summary{}, err
	}
	sum := Summarize(evs, since)
	if bucket != "" {
		gran := normalizeGranularity(bucket)
		sum.BucketGranularity = gran
		sum.Buckets = BucketBy(evs, gran, since, time.Now())
	}
	if samples {
		sum.Samples = collectSamples(evs)
	}
	return sum, nil
}
