package agentstore

import (
	"errors"
	"fmt"
	"strings"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"

	"github.com/srjn45/warden/internal/store"
)

// ErrUnhealthy matches (errors.Is) every *UnhealthyError.
var ErrUnhealthy = errors.New("agent store unhealthy")

// unhealthyHint is the operator-facing next step appended to every message.
const unhealthyHint = SafeNextStep + "; automated repair is not yet available (run `warden repair agents` for status)"

// UnhealthyError reports that the engine's index/segments disagree with what a
// read needs (stale or corrupt index, an offset that no longer decodes to the
// requested record, identity mismatch, duplicate keys, or a scan that silently
// omitted records). Reads are complete-or-error: this is returned instead of a
// short list or a wrong record. It also unwraps to *store.DegradedScanError so
// the existing 503 / last-known-good / store-health handling applies.
type UnhealthyError struct {
	Failures []store.ScanFailure
	degraded *store.DegradedScanError
}

func newUnhealthy(failures ...store.ScanFailure) *UnhealthyError {
	return &UnhealthyError{Failures: failures, degraded: &store.DegradedScanError{Failures: failures}}
}

func (e *UnhealthyError) Error() string {
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		if f.Key != "" {
			parts = append(parts, fmt.Sprintf("%s/%s: %s (%s)", f.Collection, f.Key, f.Detail, f.Class))
		} else {
			parts = append(parts, fmt.Sprintf("%s: %s (%s)", f.Collection, f.Detail, f.Class))
		}
	}
	return fmt.Sprintf("agent store degraded: %s; %s", strings.Join(parts, "; "), unhealthyHint)
}

func (e *UnhealthyError) Unwrap() []error { return []error{ErrUnhealthy, e.degraded} }

// IsUnhealthy returns the typed error when err is or wraps one.
func IsUnhealthy(err error) (*UnhealthyError, bool) { return errors.AsType[*UnhealthyError](err) }

func readFailure(col, key string, err error) *UnhealthyError {
	return newUnhealthy(store.ScanFailure{Collection: col, Key: key, Class: store.DegradeRead, Detail: err.Error()})
}

func integrityFailure(col, key, detail string) store.ScanFailure {
	return store.ScanFailure{Collection: col, Key: key, Class: store.DegradeIntegrity, Detail: detail}
}

// verifyRecord checks that a record fetched for logical key carries that key
// and the same agent id, i.e. the index offset decoded to the right record.
func verifyRecord(col, key string, rec engine.Record) *UnhealthyError {
	if rec.Key != key {
		return newUnhealthy(integrityFailure(col, key, fmt.Sprintf("index offset decodes to record with key %q (numeric id %d)", rec.Key, rec.ID)))
	}
	if id, _ := rec.Data["id"].(string); id != key {
		return newUnhealthy(integrityFailure(col, key, fmt.Sprintf("record body id %q does not match its logical key", id)))
	}
	return nil
}

// scanVerified scans col and verifies the result is complete and consistent:
// the row count equals the primary-index count (a nil-error scan can silently
// omit entries whose index location is invalid), every row has a logical key
// equal to its body id, and neither keys nor numeric ids repeat. Undecodable
// bodies are skipped and counted when tolerateDecode, else reported as failures.
// The caller holds s.mu so Count and Scan see one state.
func scanVerified(col *engine.Collection, name string, tolerateDecode bool) (out []*Agent, skipped int, err error) {
	// Count(nil) is the primary index length. Count(query.MatchAll) would stream
	// the same (possibly short) scan and so could never expose an omission.
	want, err := col.Count(nil)
	if err != nil {
		return nil, 0, readFailure(name, "", err)
	}
	rows, err := col.Scan(query.MatchAll)
	if err != nil {
		return nil, 0, readFailure(name, "", err)
	}
	return verifyRows(name, rows, want, tolerateDecode)
}

// verifyRows is the pure verification half of scanVerified (see there).
func verifyRows(name string, rows []engine.ScanResult, want uint64, tolerateDecode bool) (out []*Agent, skipped int, err error) {
	var failures []store.ScanFailure
	if uint64(len(rows)) != want {
		failures = append(failures, integrityFailure(name, "",
			fmt.Sprintf("scan returned %d of %d indexed records (silent omission)", len(rows), want)))
	}
	keys := make(map[string]struct{}, len(rows))
	ids := make(map[uint64]struct{}, len(rows))
	out = make([]*Agent, 0, len(rows))
	for _, r := range rows {
		key, _ := r.Data[engine.KeyField].(string)
		if key == "" {
			failures = append(failures, integrityFailure(name, "", fmt.Sprintf("record with numeric id %d has no logical key", r.ID)))
			continue
		}
		if _, dup := keys[key]; dup {
			failures = append(failures, integrityFailure(name, key, "duplicate logical key"))
			continue
		}
		keys[key] = struct{}{}
		if _, dup := ids[r.ID]; dup {
			failures = append(failures, integrityFailure(name, key, fmt.Sprintf("duplicate numeric id %d", r.ID)))
		}
		ids[r.ID] = struct{}{}
		if id, _ := r.Data["id"].(string); id != key {
			failures = append(failures, integrityFailure(name, key, fmt.Sprintf("record body id %q does not match its logical key", id)))
			continue
		}
		a, derr := fromRecord(r.Data)
		if derr != nil {
			if tolerateDecode {
				skipped++
				continue
			}
			failures = append(failures, store.ScanFailure{Collection: name, Key: key, Class: store.DegradeDecode, Detail: derr.Error()})
			continue
		}
		out = append(out, a)
	}
	if len(failures) > 0 {
		return nil, 0, newUnhealthy(failures...)
	}
	return out, skipped, nil
}
