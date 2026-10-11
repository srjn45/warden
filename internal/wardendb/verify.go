package wardendb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/srjn45/scriva/engine"

	"github.com/srjn45/warden/internal/ownerlock"
)

// ActionVerify is emitted by DB.Verify with the resulting health.
const ActionVerify = "wardendb.verify"

// StageVerify marks an IntegrityError raised by DB.Verify on an open handle.
const StageVerify = "verify"

// ErrNotFound: the warden-db directory does not exist.
var ErrNotFound = errors.New("wardendb: warden-db directory does not exist")

const (
	severityRepairableIndex = string(engine.SeverityRepairableIndex)
	severityDataCorruption  = string(engine.SeverityDataCorruption)
	severityConflict        = string(engine.SeverityConflict)
	codeXTxUndecided        = string(engine.CodeXTxUndecidedRun)
)

// LockState says who has a warden-db directory open.
type LockState string

const (
	// LockUnowned: nothing has the directory open.
	LockUnowned LockState = "unowned"
	// LockOwnedBySelf: the verification ran on the handle that owns it.
	LockOwnedBySelf LockState = "owned-by-this-handle"
	// LockOwnedByOther: another process holds the ownership lock.
	LockOwnedByOther LockState = "owned-by-other"
	// LockOpenElsewhere: the engine's directory lock is held although no other
	// process holds the ownership lock — typically a handle in this very
	// process, which should be asked through DB.Verify instead.
	LockOpenElsewhere LockState = "open-elsewhere"
)

// LockStatus is the ownership of the directory at verification time.
type LockStatus struct {
	State LockState
	// Owner is the holder's advisory metadata; nil unless State is
	// LockOwnedByOther and the metadata was readable.
	Owner *ownerlock.Owner
}

// FindingSet holds the findings of one scope, split by whether they block a
// strict Open. Each list is capped; the totals count everything.
type FindingSet struct {
	// Blocking findings make the directory corrupt.
	Blocking []Finding
	// Recoverable findings are what the engine reconciles from intact data.
	Recoverable      []Finding
	BlockingTotal    int
	RecoverableTotal int
}

func (s *FindingSet) add(f Finding, block bool) {
	if block {
		s.BlockingTotal++
		if len(s.Blocking) < maxFindings {
			s.Blocking = append(s.Blocking, f)
		}
		return
	}
	s.RecoverableTotal++
	if len(s.Recoverable) < maxFindings {
		s.Recoverable = append(s.Recoverable, f)
	}
}

// CollectionStatus is the verification result of one collection directory.
type CollectionStatus struct {
	Name string
	// Online: checked against the open collection's in-memory state rather
	// than the files at rest.
	Online bool
	// Registered: declared with Register on the verifying handle. Always false
	// for a read-only verification.
	Registered bool
	Health     Health

	Segments     int
	SegmentBytes int64
	LiveRecords  int

	FindingSet
	// Truncated counts findings the engine dropped past its per-code cap.
	Truncated int
}

// VerifyResult is the structured result of a database-wide verification: every
// collection directory plus the cross-collection transaction state. It is an
// observation only; nothing in it was repaired. Finding messages come from the
// engine and may quote indexed field values — use Report for anything that is
// written out or shared.
type VerifyResult struct {
	Dir    string
	Exists bool
	// Online: run on the owning handle (DB.Verify).
	Online bool
	Lock   LockStatus
	Health Health

	StartedAt  time.Time
	FinishedAt time.Time

	// Collections are sorted by name.
	Collections []CollectionStatus
	// Database holds the findings that belong to no single collection: the
	// cross-collection transaction journal and its participants.
	Database FindingSet

	// Blocking and Recoverable total the findings of every scope.
	Blocking    int
	Recoverable int
}

// Collection returns the status of the named collection, or nil.
func (r *VerifyResult) Collection(name string) *CollectionStatus {
	for i := range r.Collections {
		if r.Collections[i].Name == name {
			return &r.Collections[i]
		}
	}
	return nil
}

// blockingFindings returns every retained blocking finding.
func (r *VerifyResult) blockingFindings() []Finding {
	out := append([]Finding(nil), r.Database.Blocking...)
	for _, c := range r.Collections {
		out = append(out, c.Blocking...)
	}
	return out
}

// note reports the informational codes that say nothing about the data: lock
// state, an expired record awaiting compaction, a byte-identical duplicate.
func note(f engine.Finding) bool {
	switch f.Code {
	case engine.CodeLockHeld, engine.CodeLockHeldByThisProc, engine.CodeLockProbeFailed,
		engine.CodeRecordExpiredLive, engine.CodeDuplicateIdentical:
		return true
	}
	return false
}

func convert(f engine.Finding, collection string) Finding {
	if f.Collection == "" {
		f.Collection = collection
	}
	return Finding{
		Collection: f.Collection, Severity: string(f.Severity), Code: string(f.Code),
		Segment: f.Location.Segment, Offset: f.Location.Offset, Message: f.Message,
	}
}

// absorb folds an engine report into r. online selects the stricter rule for
// an open collection, where recovery has already run: a derived structure that
// still disagrees with the segments then blocks too. database says whether the
// report's database-level findings are taken (the transaction graph is checked
// once per verification).
func (r *VerifyResult) absorb(rep *engine.IntegrityReport, online, database bool) {
	blocks := func(f engine.Finding) bool {
		return blocking(f) || (online && f.Severity == engine.SeverityRepairableIndex)
	}
	if database {
		for _, f := range rep.Findings {
			if !note(f) {
				r.Database.add(convert(f, ""), blocks(f))
			}
		}
	}
	for i := range rep.Collections {
		cr := &rep.Collections[i]
		cs := CollectionStatus{
			Name: cr.Name, Online: online,
			Segments: cr.Stats.Segments, SegmentBytes: cr.Stats.SegmentBytes, LiveRecords: cr.Stats.LiveRecords,
		}
		for _, f := range cr.Findings {
			if !note(f) {
				cs.add(convert(f, cr.Name), blocks(f))
			}
		}
		for _, n := range cr.Truncated {
			cs.Truncated += n
		}
		r.Collections = append(r.Collections, cs)
	}
}

// finish derives the totals and the health of every scope.
func (r *VerifyResult) finish() {
	sort.Slice(r.Collections, func(i, j int) bool { return r.Collections[i].Name < r.Collections[j].Name })
	r.Blocking, r.Recoverable = r.Database.BlockingTotal, r.Database.RecoverableTotal
	for i := range r.Collections {
		c := &r.Collections[i]
		c.Health = healthOf(true, LockUnowned, c.BlockingTotal, c.RecoverableTotal)
		r.Blocking += c.BlockingTotal
		r.Recoverable += c.RecoverableTotal
	}
	r.Health = healthOf(r.Exists, r.Lock.State, r.Blocking, r.Recoverable)
	r.FinishedAt = time.Now()
}

func hasCode(rep *engine.IntegrityReport, code engine.FindingCode) bool {
	for _, f := range rep.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// Verify is the read-only, database-wide verification of the warden-db under
// dataDir. It checks every collection directory against its segment bytes and
// the cross-collection transaction state exactly as they sit on disk. It never
// creates, opens, repairs or modifies anything and never takes ownership, so
// it is safe to run against a directory a daemon has open; in that case the
// result is HealthLocked unless it found damage (see Health). A missing
// directory is reported as HealthMissing, not as an error.
func Verify(ctx context.Context, dataDir string) (*VerifyResult, error) {
	if dataDir == "" {
		return nil, errors.New("wardendb: empty data directory")
	}
	st, err := Probe(dataDir)
	if err != nil {
		return nil, err
	}
	lock := LockStatus{State: LockUnowned}
	if st.Owned {
		lock = LockStatus{State: LockOwnedByOther, Owner: st.Owner}
	}
	return verifyDir(ctx, st.Dir, st.Exists, lock)
}

// verifyDir is the offline verification of dir. It is read-only.
func verifyDir(ctx context.Context, dir string, exists bool, lock LockStatus) (*VerifyResult, error) {
	res := &VerifyResult{Dir: dir, Exists: exists, Lock: lock, StartedAt: time.Now()}
	if !exists {
		res.finish()
		return res, nil
	}
	rep, err := engine.VerifyDir(ctx, dir, engine.VerifyOptions{Mode: engine.VerifyFull})
	if err != nil {
		return nil, fmt.Errorf("wardendb: verify %s: %w", dir, err)
	}
	if res.Lock.State == LockUnowned && hasCode(rep, engine.CodeLockHeld) {
		res.Lock.State = LockOpenElsewhere
	}
	res.absorb(rep, false, true)
	res.finish()
	return res, nil
}

// Verify is the database-wide verification run by the owning handle. Every
// collection the engine has open is checked online, against a consistent
// snapshot of its in-memory state, without blocking writers; a collection
// directory the engine has not opened is checked at rest; the cross-collection
// transaction state is checked once for the whole database. It modifies
// nothing on disk.
//
// A blocking finding retires the handle, exactly as one found by Register
// does: Verify still returns the result (Health is HealthCorrupt) and every
// later use of the handle returns the *IntegrityError.
func (db *DB) Verify(ctx context.Context) (*VerifyResult, error) {
	handle, err := db.Handle()
	if err != nil {
		return nil, err
	}
	res := &VerifyResult{Dir: db.dir, Exists: true, Online: true, Lock: LockStatus{State: LockOwnedBySelf}, StartedAt: time.Now()}
	opts := engine.VerifyOptions{Mode: engine.VerifyFull}
	rep, err := handle.Engine().Verify(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("wardendb: verify %s: %w", db.dir, err)
	}
	res.absorb(rep, true, true)

	open := map[string]bool{}
	for _, c := range rep.Collections {
		open[c.Name] = true
	}
	ents, err := os.ReadDir(db.dir)
	if err != nil {
		return nil, fmt.Errorf("wardendb: verify %s: %w", db.dir, err)
	}
	var atRest []string
	for _, e := range ents {
		if e.IsDir() && !open[e.Name()] {
			atRest = append(atRest, e.Name())
		}
	}
	if len(atRest) > 0 {
		opts.Collections = atRest
		off, err := engine.VerifyDir(ctx, db.dir, opts)
		if err != nil {
			return nil, fmt.Errorf("wardendb: verify %s: %w", db.dir, err)
		}
		res.absorb(off, false, false)
	}

	db.mu.RLock()
	for i := range res.Collections {
		_, res.Collections[i].Registered = db.entries[res.Collections[i].Name]
	}
	db.mu.RUnlock()
	res.finish()

	db.mon.emit(ActionVerify, "", map[string]string{
		"health":      string(res.Health),
		"collections": strconv.Itoa(len(res.Collections)),
		"blocking":    strconv.Itoa(res.Blocking),
		"recoverable": strconv.Itoa(res.Recoverable),
	})
	if res.Blocking > 0 {
		db.mon.fail(db.mon.integrityError(StageVerify, "", res.blockingFindings(), res.Blocking, nil))
	}
	return res, nil
}
