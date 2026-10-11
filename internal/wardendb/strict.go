package wardendb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"

	"github.com/srjn45/warden/internal/audit"
)

// The strict open policy. wardendb applies it to the handle and to every
// registered collection; no Option and no Spec can change it.
const (
	// SyncMode fsyncs every write before it is acknowledged.
	SyncMode = engine.SyncModeAlways
	// IntegrityPolicy refuses corruption and conflicting history at open.
	IntegrityPolicy = engine.PolicyFail
)

// Bounds and defaults of the background compaction configuration.
const (
	DefaultCompactInterval = 15 * time.Minute
	MinCompactInterval     = time.Minute
	MaxCompactInterval     = 24 * time.Hour

	DefaultCompactDirtyRatio = 0.30
	MinCompactDirtyRatio     = 0.10
	MaxCompactDirtyRatio     = 0.90
)

// CompactionLimits bounds background compaction. A zero field selects its
// default; a value outside its Min/Max bound makes Open fail with
// ErrInvalidOption rather than being clamped.
type CompactionLimits struct {
	// Interval is the cadence of the background pass, for every collection.
	Interval time.Duration
	// DirtyRatio is the fraction of dead bytes in the sealed segments below
	// which a background pass does nothing.
	DirtyRatio float64
}

func (l CompactionLimits) resolve() (CompactionLimits, error) {
	if l.Interval == 0 {
		l.Interval = DefaultCompactInterval
	}
	if l.DirtyRatio == 0 {
		l.DirtyRatio = DefaultCompactDirtyRatio
	}
	if l.Interval < MinCompactInterval || l.Interval > MaxCompactInterval {
		return l, fmt.Errorf("%w: compaction interval %s outside [%s, %s]", ErrInvalidOption, l.Interval, MinCompactInterval, MaxCompactInterval)
	}
	if !(l.DirtyRatio >= MinCompactDirtyRatio && l.DirtyRatio <= MaxCompactDirtyRatio) {
		return l, fmt.Errorf("%w: compaction dirty ratio %v outside [%v, %v]", ErrInvalidOption, l.DirtyRatio, MinCompactDirtyRatio, MaxCompactDirtyRatio)
	}
	return l, nil
}

// WithCompaction sets the background compaction limits.
func WithCompaction(l CompactionLimits) Option {
	return func(c *config) { c.compaction = l }
}

// Auditor receives the structured lifecycle and integrity events. It must be
// safe for concurrent use and must not call back into the DB: events are
// emitted from Open, Register, Close and the engine's own goroutines.
// *audit.Writer satisfies it.
type Auditor interface {
	Log(audit.Event)
}

// Metrics receives one counter increment per event and the durations of opens
// and compactions. collection is empty for database-level events. The same
// concurrency rules as Auditor apply.
type Metrics interface {
	Inc(name, collection string)
	Observe(name, collection string, d time.Duration)
}

// WithAuditor routes events to a. The default discards them.
func WithAuditor(a Auditor) Option {
	return func(c *config) { c.auditor = a }
}

// WithMetrics routes counters and durations to m. The default discards them.
func WithMetrics(m Metrics) Option {
	return func(c *config) { c.metrics = m }
}

// Audit actions. Each is also the name of the counter incremented with it.
const (
	ActionOpen     = "wardendb.open"     // handle opened under the strict policy
	ActionClose    = "wardendb.close"    // handle closed
	ActionRegister = "wardendb.register" // collection registered
	// ActionLockRefused: Open refused because warden-db has another owner.
	ActionLockRefused = "wardendb.lock_refused"
	// ActionIntegrityRefused: Open or Register refused, or the open handle was
	// retired, because of corruption or conflicting history.
	ActionIntegrityRefused = "wardendb.integrity_refused"
	// ActionPolicyRefused: Register refused a collection that is not running
	// under the strict policy.
	ActionPolicyRefused = "wardendb.policy_refused"
	// ActionRecovery: Open found crash leftovers the engine reconciles on its
	// own from intact data (a torn unacknowledged tail, a stale derived index,
	// an undecided transaction). Reported so that recovery is never silent.
	ActionRecovery   = "wardendb.recovery"
	ActionCompaction = "wardendb.compaction" // background compaction pass finished
)

// Duration metrics.
const (
	MetricOpenDuration       = "wardendb.open.duration"
	MetricCompactionDuration = "wardendb.compaction.duration"
)

// Policy and integrity errors.
var (
	// ErrIntegrity is wrapped by every *IntegrityError.
	ErrIntegrity = errors.New("wardendb: integrity violation")
	// ErrPolicy is wrapped by every *PolicyError.
	ErrPolicy = errors.New("wardendb: strict policy violation")
	// ErrInvalidOption: an Option carries a value outside its bounds.
	ErrInvalidOption = errors.New("wardendb: invalid option")
	// ErrRepairNotImplemented was returned by OpenForRepair while it was a
	// reserved stub.
	//
	// Deprecated: OpenForRepair is implemented; nothing returns this error.
	ErrRepairNotImplemented = errors.New("wardendb: repair entry point is not implemented")
)

// Stages at which an integrity violation is detected.
const (
	StageOpen     = "open"     // before or while the handle was opened
	StageRegister = "register" // while a collection was being registered
	StageRuntime  = "runtime"  // on the open handle
)

// Finding is one blocking verification result behind an IntegrityError.
type Finding struct {
	Collection string
	Severity   string
	Code       string
	Segment    string
	Offset     int64
	Message    string
}

func (f Finding) String() string {
	var b strings.Builder
	b.WriteString(f.Code)
	if f.Collection != "" {
		b.WriteString(" in " + f.Collection)
	}
	if f.Segment != "" {
		fmt.Fprintf(&b, " %s@%d", f.Segment, f.Offset)
	}
	if f.Message != "" {
		b.WriteString(": " + f.Message)
	}
	return b.String()
}

// maxFindings caps the findings carried by one IntegrityError.
const maxFindings = 32

// IntegrityError reports that warden-db holds corrupt segment bytes or an
// ambiguous record history. The refused operation changed nothing on disk and
// nothing was salvaged: the data stays as found for verification and repair.
type IntegrityError struct {
	Dir string
	// Stage is one of the Stage* constants.
	Stage string
	// Collection is the affected collection, or empty when the violation is
	// database-level or spans several collections (see Findings).
	Collection string
	// Detail is a one-line summary of the first finding.
	Detail string
	// Findings are the blocking findings, capped; Total counts all of them.
	Findings []Finding
	Total    int
	cause    error
}

func (e *IntegrityError) Error() string {
	where := e.Dir
	if e.Collection != "" {
		where = fmt.Sprintf("collection %q in %s", e.Collection, e.Dir)
	}
	msg := fmt.Sprintf("wardendb: integrity violation at %s: %s", e.Stage, where)
	if e.Total > 0 {
		msg += fmt.Sprintf(": %d blocking finding(s)", e.Total)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg + " (refused; nothing was modified or salvaged)"
}

// Unwrap exposes ErrIntegrity and the underlying engine error.
func (e *IntegrityError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrIntegrity}
	}
	return []error{ErrIntegrity, e.cause}
}

// PolicyError reports a collection that is not running under the strict
// policy, for example one opened directly on Handle with weaker options
// before it was registered.
type PolicyError struct {
	Collection string
	Setting    string
	Got, Want  string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("wardendb: collection %q has %s %s, the strict policy requires %s", e.Collection, e.Setting, e.Got, e.Want)
}

// Unwrap exposes ErrPolicy.
func (e *PolicyError) Unwrap() error { return ErrPolicy }

// OpenForRepair is the only entry point that opens a warden-db it had to
// repair first — rebuilding derived structures and, when asked, salvaging
// around damaged segment bytes. It runs Repair with the RepairOptions given by
// WithRepair and then a strict Open, so the returned handle is under exactly
// the same policy as any other. Without WithRepair it fails with
// ErrRepairBackupRequired and touches nothing; whatever Repair refuses stays
// refused (ErrRepairRefused) and no handle is returned. Open and Register
// never salvage.
func OpenForRepair(dataDir string, opts ...Option) (*DB, error) {
	return openForRepair(dataDir, opts)
}

// monitor fans events out to the auditor and metrics sinks and holds the
// integrity fault that retires an open handle. It has its own lock because
// the engine invokes its callbacks while DB.mu may be held.
type monitor struct {
	dir     string
	auditor Auditor
	metrics Metrics

	mu       sync.Mutex
	fault    *IntegrityError
	observed map[string]*engine.CollectionReport // failed open-time scans
}

func (m *monitor) emit(action, collection string, detail map[string]string) {
	if m.metrics != nil {
		m.metrics.Inc(action, collection)
	}
	if m.auditor != nil {
		target := m.dir
		if collection != "" {
			if detail == nil {
				detail = map[string]string{}
			}
			detail["dir"] = m.dir
			target = collection
		}
		m.auditor.Log(audit.Event{Time: time.Now(), Action: action, Actor: "wardendb", Target: target, Detail: detail})
	}
}

func (m *monitor) observe(name, collection string, d time.Duration) {
	if m.metrics != nil {
		m.metrics.Observe(name, collection, d)
	}
}

// locked emits the lock refusal for e and returns it.
func (m *monitor) locked(e *LockedError) *LockedError {
	detail := map[string]string{}
	if o := e.Owner; o != nil {
		detail["owner_kind"] = string(o.Kind)
		detail["owner_pid"] = strconv.Itoa(o.PID)
	}
	m.emit(ActionLockRefused, "", detail)
	return e
}

// refuse emits the integrity refusal for e and returns it.
func (m *monitor) refuse(e *IntegrityError) *IntegrityError {
	m.emit(ActionIntegrityRefused, e.Collection, map[string]string{
		"stage":    e.Stage,
		"findings": strconv.Itoa(e.Total),
		"detail":   e.Detail,
	})
	return e
}

// fail retires the handle: every later use returns e. The first fault wins.
func (m *monitor) fail(e *IntegrityError) *IntegrityError {
	m.mu.Lock()
	if m.fault != nil {
		e = m.fault
		m.mu.Unlock()
		return e
	}
	m.fault = e
	m.mu.Unlock()
	return m.refuse(e)
}

func (m *monitor) faultErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault == nil {
		return nil
	}
	return m.fault
}

// onIntegrity is the engine's open-time scan callback.
func (m *monitor) onIntegrity(collection string, _ engine.IntegrityPolicy, outcome string, report *engine.CollectionReport) {
	if outcome == engine.IntegrityOutcomeClean {
		return
	}
	m.mu.Lock()
	if m.observed == nil {
		m.observed = map[string]*engine.CollectionReport{}
	}
	m.observed[collection] = report
	m.mu.Unlock()
}

func (m *monitor) takeObserved(collection string) *engine.CollectionReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.observed[collection]
	delete(m.observed, collection)
	return r
}

func (m *monitor) onCompaction(collection string, d time.Duration) {
	m.emit(ActionCompaction, collection, map[string]string{"duration_ms": strconv.FormatInt(d.Milliseconds(), 10)})
	m.observe(MetricCompactionDuration, collection, d)
}

// onSegmentPoisoned: a failed write could not be rolled back, so the segment
// may hold bytes the engine never acknowledged. The handle is retired.
func (m *monitor) onSegmentPoisoned(collection, segment string, cause error) {
	m.fail(&IntegrityError{
		Dir: m.dir, Stage: StageRuntime, Collection: collection,
		Detail: fmt.Sprintf("segment %s is poisoned by a failed write: %v", segment, cause),
		cause:  cause,
	})
}

// engineOptions is the strict policy as handle-wide engine options.
func (m *monitor) engineOptions(l CompactionLimits) []scriva.Option {
	return []scriva.Option{
		scriva.WithSyncMode(SyncMode),
		scriva.WithIntegrityPolicy(IntegrityPolicy),
		scriva.WithOnIntegrity(m.onIntegrity),
		scriva.WithCompactInterval(l.Interval),
		func(c *engine.CollectionConfig) {
			c.CompactDirtyPct = l.DirtyRatio
			c.OnCompaction = m.onCompaction
			c.OnSegmentPoisoned = m.onSegmentPoisoned
		},
	}
}

// collectionOptions appends the strict policy to a Spec's options. Options
// apply in order, so the policy overrides anything the Spec set.
func collectionOptions(spec []scriva.CollectionOption, l CompactionLimits) []scriva.CollectionOption {
	out := make([]scriva.CollectionOption, 0, len(spec)+2)
	for _, o := range spec {
		if o != nil {
			out = append(out, o)
		}
	}
	return append(out,
		scriva.WithCollectionSyncMode(SyncMode),
		scriva.WithCollectionCompactInterval(l.Interval),
	)
}

// checkPolicy confirms the configuration a collection is actually running
// under. It is what catches a handle that was opened behind Register's back.
func checkPolicy(name string, got engine.CollectionConfig, l CompactionLimits) error {
	bad := func(setting, g, w string) error {
		return &PolicyError{Collection: name, Setting: setting, Got: g, Want: w}
	}
	policy := got.IntegrityPolicy
	if policy == "" {
		policy = engine.PolicyFail
	}
	switch {
	case got.SyncMode != SyncMode:
		return bad("sync mode", syncModeName(got.SyncMode), syncModeName(SyncMode))
	case policy != IntegrityPolicy:
		return bad("integrity policy", string(policy), string(IntegrityPolicy))
	case got.OnIntegrity == nil:
		return bad("integrity callback", "none", "wardendb's")
	case got.CompactInterval != l.Interval:
		return bad("compaction interval", got.CompactInterval.String(), l.Interval.String())
	case got.CompactDirtyPct != l.DirtyRatio:
		return bad("compaction dirty ratio", fmt.Sprint(got.CompactDirtyPct), fmt.Sprint(l.DirtyRatio))
	}
	return nil
}

func syncModeName(m engine.SyncMode) string { return string(m) }

// blocking reports whether a finding must refuse the operation. Corrupt
// segment bytes and ambiguous history always do. What stays open is only what
// the engine reconciles deterministically from intact data: informational
// leftovers, derived indexes that lag the segments, and a transaction that
// crashed before its coordinator decision (aborted by recovery, since it was
// never acknowledged).
func blocking(f engine.Finding) bool {
	switch f.Severity {
	case engine.SeverityInfo, engine.SeverityRepairableIndex:
		return false
	}
	return f.Code != engine.CodeXTxUndecidedRun
}

// classify splits findings into blocking ones and the codes of tolerated
// crash leftovers. online is set for a check of an open collection, where
// recovery has already run: a derived index that still disagrees with the
// segments then blocks too.
func classify(all []engine.Finding, online bool) (block []Finding, total int, recovered []string) {
	seen := map[string]bool{}
	for _, f := range all {
		if blocking(f) || (online && f.Severity == engine.SeverityRepairableIndex) {
			total++
			if len(block) < maxFindings {
				block = append(block, Finding{
					Collection: f.Collection, Severity: string(f.Severity), Code: string(f.Code),
					Segment: f.Location.Segment, Offset: f.Location.Offset, Message: f.Message,
				})
			}
			continue
		}
		switch f.Code {
		case engine.CodeLockHeld, engine.CodeLockHeldByThisProc, engine.CodeLockProbeFailed, engine.CodeRecordExpiredLive, engine.CodeDuplicateIdentical:
			continue
		}
		code := string(f.Code)
		if f.Collection != "" {
			code = f.Collection + ":" + code
		}
		if !seen[code] {
			seen[code] = true
			recovered = append(recovered, code)
		}
	}
	return block, total, recovered
}

func collectionFindings(r *engine.CollectionReport) []engine.Finding {
	if r == nil {
		return nil
	}
	out := make([]engine.Finding, len(r.Findings))
	for i, f := range r.Findings {
		if f.Collection == "" {
			f.Collection = r.Name
		}
		out[i] = f
	}
	return out
}

func reportFindings(r *engine.IntegrityReport) []engine.Finding {
	out := append([]engine.Finding(nil), r.Findings...)
	for i := range r.Collections {
		out = append(out, collectionFindings(&r.Collections[i])...)
	}
	return out
}

// integrityError builds the typed error for blocking findings.
func (m *monitor) integrityError(stage, collection string, block []Finding, total int, cause error) *IntegrityError {
	e := &IntegrityError{Dir: m.dir, Stage: stage, Collection: collection, Findings: block, Total: total, cause: cause}
	if len(block) > 0 {
		e.Detail = block[0].String()
		if collection == "" {
			e.Collection = block[0].Collection
			for _, f := range block {
				if f.Collection != e.Collection {
					e.Collection = ""
					break
				}
			}
		}
	} else if cause != nil {
		e.Detail = cause.Error()
	}
	return e
}

// preflight verifies the directory read-only, before the engine opens it, so
// a refusal leaves every byte as found. It returns the codes of tolerated
// crash leftovers.
func (m *monitor) preflight() ([]string, error) {
	rep, err := engine.VerifyDir(context.Background(), m.dir, engine.VerifyOptions{Mode: engine.VerifyFull})
	if err != nil {
		return nil, m.refuse(m.integrityError(StageOpen, "", nil, 0, err))
	}
	block, total, recovered := classify(reportFindings(rep), false)
	if total > 0 {
		return nil, m.refuse(m.integrityError(StageOpen, "", block, total, nil))
	}
	return recovered, nil
}

// engineIntegrity converts an engine open failure into the typed error, or
// returns nil when err is not an integrity failure.
func (m *monitor) engineIntegrity(stage, collection string, err error) *IntegrityError {
	if !errors.Is(err, engine.ErrIntegrity) {
		return nil
	}
	var rep *engine.CollectionReport
	var oe *engine.OpenIntegrityError
	if errors.As(err, &oe) {
		rep = oe.Report
		if collection == "" {
			collection = oe.Collection
		}
	}
	if rep == nil {
		rep = m.takeObserved(collection)
	}
	block, total, _ := classify(collectionFindings(rep), false)
	return m.integrityError(stage, collection, block, total, err)
}

// verifyCollection checks an opened collection against its segment bytes
// before Register hands it out.
func (m *monitor) verifyCollection(col *engine.Collection) *IntegrityError {
	name := col.Name()
	rep, err := col.Verify(context.Background(), engine.VerifyOptions{Mode: engine.VerifyFull})
	if err != nil {
		return m.integrityError(StageRegister, name, nil, 0, err)
	}
	if block, total, _ := classify(reportFindings(rep), true); total > 0 {
		return m.integrityError(StageRegister, name, block, total, nil)
	}
	return nil
}
