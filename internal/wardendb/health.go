package wardendb

// Health is the one-word classification of a warden-db directory, derived
// from a verification (see VerifyResult).
type Health string

const (
	// HealthHealthy: the directory exists, nothing else has it open and
	// verification found nothing to reconcile.
	HealthHealthy Health = "healthy"
	// HealthDegraded: the data is intact but verification found crash
	// leftovers or lagging derived structures (a torn unacknowledged tail, a
	// missing or stale index, an undecided transaction, leftover temp files).
	// A strict Open accepts the directory and the engine reconciles them from
	// the intact segments, reporting ActionRecovery.
	HealthDegraded Health = "degraded"
	// HealthCorrupt: at least one blocking finding — damaged segment bytes,
	// ambiguous record history or an irreconcilable transaction. Open and
	// Register refuse the directory; only Repair may touch it.
	HealthCorrupt Health = "corrupt"
	// HealthMissing: the warden-db directory does not exist.
	HealthMissing Health = "missing"
	// HealthLocked: the directory is open elsewhere and a read-only
	// verification found no blocking finding. Non-blocking findings may then
	// only reflect the owner's in-flight writes, so the directory is neither
	// called healthy nor degraded; ask the owner (DB.Verify) for the
	// authoritative answer.
	HealthLocked Health = "locked"
)

// Openable reports whether a strict Open would accept a directory in this
// state: it is intact (healthy or degraded) and nothing else has it open.
func (h Health) Openable() bool { return h == HealthHealthy || h == HealthDegraded }

// healthOf applies the classification rules, in order of precedence:
//
//  1. no directory                          -> missing
//  2. any blocking finding                  -> corrupt (damaged bytes are damaged
//     whoever holds the lock)
//  3. open in another handle or process     -> locked
//  4. any recoverable (non-blocking) finding -> degraded
//  5. otherwise                             -> healthy
//
// A handle verifying itself (LockOwnedBySelf) is not "locked".
func healthOf(exists bool, lock LockState, blocking, recoverable int) Health {
	switch {
	case !exists:
		return HealthMissing
	case blocking > 0:
		return HealthCorrupt
	case lock == LockOwnedByOther || lock == LockOpenElsewhere:
		return HealthLocked
	case recoverable > 0:
		return HealthDegraded
	}
	return HealthHealthy
}

// FindingClass groups findings by what may be done about them. It is the unit
// Repair's policy is stated in.
type FindingClass string

const (
	// ClassCrashLeftover: what a crash legitimately leaves over intact data (a
	// torn tail that was never acknowledged, leftover temp or orphan files, a
	// missing meta file, a transaction with no coordinator decision). A strict
	// Open reconciles these itself; Repair leaves them to it.
	ClassCrashLeftover FindingClass = "crash-leftover"
	// ClassDerived: a derived structure (primary or secondary index, id
	// counter, interrupted compaction swap) disagrees with the segments.
	// Repair rebuilds it from the segments, which are never edited.
	ClassDerived FindingClass = "derived-structure"
	// ClassSegmentDamage: segment bytes are damaged. Repair refuses unless
	// RepairOptions.Salvage is set, in which case the valid records are moved
	// to a new segment and the damaged original is quarantined, never deleted.
	ClassSegmentDamage FindingClass = "segment-damage"
	// ClassConflict: ambiguous record history (duplicate ids, revision
	// regressions, unique violations). Never repaired: resolving it would mean
	// guessing which record is the truth.
	ClassConflict FindingClass = "history-conflict"
	// ClassTransaction: the cross-collection transaction journal and its
	// participants disagree. Never repaired.
	ClassTransaction FindingClass = "transaction"
)

// ClassOf returns the class of a finding reported by Verify.
func ClassOf(f Finding) FindingClass {
	if len(f.Code) >= 4 && f.Code[:4] == "xtx-" {
		if f.Code == codeXTxUndecided {
			return ClassCrashLeftover
		}
		return ClassTransaction
	}
	switch f.Severity {
	case severityRepairableIndex:
		return ClassDerived
	case severityDataCorruption:
		return ClassSegmentDamage
	case severityConflict:
		return ClassConflict
	}
	return ClassCrashLeftover
}

// RepairPolicy states what Repair does with findings of class c.
func (c FindingClass) RepairPolicy() string {
	switch c {
	case ClassCrashLeftover:
		return "left alone: a strict Open reconciles it from intact data"
	case ClassDerived:
		return "rebuilt from the segments; no segment is edited"
	case ClassSegmentDamage:
		return "refused unless Salvage is set; with Salvage the valid records move to a new segment and the damaged original is quarantined, never deleted"
	case ClassConflict:
		return "refused: conflicting record history is never resolved automatically"
	case ClassTransaction:
		return "refused: transaction state is never rewritten"
	}
	return "refused: unknown finding class"
}
