package agentstore

import "sync/atomic"

// Diagnostic seams (contract: docs/specs/2026-10-09-agent-store-lock-contention-contract.md §9).
//
// Both seams are nil in production, so they are a single atomic load on the hot
// path and change no behavior. They exist so the lock-contention baseline can
// make "a slow scan" and "a slow write" deterministic instead of relying on a
// huge store or sleeps in the engine. They are process-global: tests that set
// them must not run in parallel and must call the returned restore func.

// ScanSeam is invoked inside scanVerified while the caller still holds the
// store mutex, immediately before the engine scan. collection is "agents" or
// "closed".
type ScanSeam func(collection string)

// WriteSeam is invoked by every mutating method after it has acquired the store
// mutex and before it touches the engine. op names the method ("Insert",
// "Update", "UpdateStatusIf", "FinalizeExit", "Archive", "Delete").
type WriteSeam func(op string)

// ReadSeam is invoked by snapshot read methods before loading the snapshot.
// op names the method ("List", "Get", "GetByNameOrID", "ListClosed", "ListClosedDegraded").
type ReadSeam func(op string)

// AuditSeam is invoked inside Auditor during verification passes.
type AuditSeam func(phase string)

var (
	scanSeam  atomic.Pointer[ScanSeam]
	writeSeam atomic.Pointer[WriteSeam]
	readSeam  atomic.Pointer[ReadSeam]
	auditSeam atomic.Pointer[AuditSeam]
)

// SetScanSeam installs fn (nil clears) and returns a restore func.
func SetScanSeam(fn ScanSeam) (restore func()) {
	prev := scanSeam.Load()
	if fn == nil {
		scanSeam.Store(nil)
	} else {
		scanSeam.Store(&fn)
	}
	return func() { scanSeam.Store(prev) }
}

// SetWriteSeam installs fn (nil clears) and returns a restore func.
func SetWriteSeam(fn WriteSeam) (restore func()) {
	prev := writeSeam.Load()
	if fn == nil {
		writeSeam.Store(nil)
	} else {
		writeSeam.Store(&fn)
	}
	return func() { writeSeam.Store(prev) }
}

// SetReadSeam installs fn (nil clears) and returns a restore func.
func SetReadSeam(fn ReadSeam) (restore func()) {
	prev := readSeam.Load()
	if fn == nil {
		readSeam.Store(nil)
	} else {
		readSeam.Store(&fn)
	}
	return func() { readSeam.Store(prev) }
}

// SetAuditSeam installs fn (nil clears) and returns a restore func.
func SetAuditSeam(fn AuditSeam) (restore func()) {
	prev := auditSeam.Load()
	if fn == nil {
		auditSeam.Store(nil)
	} else {
		auditSeam.Store(&fn)
	}
	return func() { auditSeam.Store(prev) }
}

func fireScanSeam(collection string) {
	if p := scanSeam.Load(); p != nil {
		(*p)(collection)
	}
}

func fireWriteSeam(op string) {
	if p := writeSeam.Load(); p != nil {
		(*p)(op)
	}
}

func fireReadSeam(op string) {
	if p := readSeam.Load(); p != nil {
		(*p)(op)
	}
}

func fireAuditSeam(phase string) {
	if p := auditSeam.Load(); p != nil {
		(*p)(phase)
	}
}
