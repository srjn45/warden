package metrics

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Recorder appends Samples to per-day JSONL files under dir and reads them back.
// Append-only; one Sample per line. Safe for concurrent Record calls.
type Recorder struct {
	dir string
	mu  sync.Mutex
}

// NewRecorder ensures dir exists (0o700 — samples carry agent ids/host memory
// state, same sensitivity class as session files).
func NewRecorder(dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Recorder{dir: dir}, nil
}

func (r *Recorder) fileFor(t time.Time) string {
	return filepath.Join(r.dir, t.UTC().Format("2006-01-02")+".jsonl")
}

// Record appends one sample to its day-file (chosen from sample.TakenAt).
func (r *Recorder) Record(s Sample) error {
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.OpenFile(r.fileFor(s.TakenAt), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// History returns samples with TakenAt >= since, newest-first, capped at limit
// (<=0 ⇒ no cap). Missing/unreadable files and malformed lines are skipped, not
// errored.
func (r *Recorder) History(since time.Time, limit int) ([]Sample, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	files, err := filepath.Glob(filepath.Join(r.dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Sample
	for _, fp := range files {
		f, err := os.Open(fp)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var s Sample
			if json.Unmarshal(sc.Bytes(), &s) != nil {
				continue
			}
			if !since.IsZero() && s.TakenAt.Before(since) {
				continue
			}
			out = append(out, s)
		}
		f.Close()
	}
	// newest-first
	sort.Slice(out, func(i, j int) bool { return out[i].TakenAt.After(out[j].TakenAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Prune deletes day-files older than keepDays relative to now (by filename date).
func (r *Recorder) Prune(now time.Time, keepDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.UTC().AddDate(0, 0, -keepDays).Truncate(24 * time.Hour)
	files, err := filepath.Glob(filepath.Join(r.dir, "*.jsonl"))
	if err != nil {
		return err
	}
	for _, fp := range files {
		base := strings.TrimSuffix(filepath.Base(fp), ".jsonl")
		day, err := time.Parse("2006-01-02", base)
		if err != nil {
			continue
		}
		if day.Before(cutoff) {
			_ = os.Remove(fp)
		}
	}
	return nil
}
