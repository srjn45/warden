package agentstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/store"
)

// State is the overall operational state of the agent store (docs/specs/2026-10-09-agent-store-lock-contention-contract.md §4.2).
type State string

const (
	StateOK          State = "ok"
	StateSuspect     State = "suspect"
	StateDegraded    State = "degraded"
	StateUnavailable State = "unavailable"
)

// AuditResult indicates the outcome of an integrity audit pass.
type AuditResult string

const (
	AuditResultClean    AuditResult = "clean"
	AuditResultFindings AuditResult = "findings"
	AuditResultError    AuditResult = "error"
	AuditResultTimeout  AuditResult = "timeout"
	AuditResultCanceled AuditResult = "canceled"
)

// AuditFinding is a structured finding reported by the auditor.
type AuditFinding struct {
	Collection string `json:"collection"`
	Key        string `json:"key,omitempty"`
	Class      string `json:"class"`
	Detail     string `json:"detail"`
}

// AuditReport records the result and metadata of the latest audit.
type AuditReport struct {
	LastRunAt     time.Time      `json:"last_run_at"`
	LastSuccessAt time.Time      `json:"last_success_at"`
	LastResult    AuditResult    `json:"last_result"`
	Duration      time.Duration  `json:"duration_ms"`
	Findings      []AuditFinding `json:"findings"`
}

// AuditorOptions configures the cadence, timeout, and bounds for the integrity auditor.
type AuditorOptions struct {
	Interval   time.Duration // periodic audit interval (default 10m)
	Debounce   time.Duration // debounce window for suspect/unforced audits (default 5s)
	Timeout    time.Duration // execution duration cap per audit pass (default 5m)
	MaxRetries int           // retry attempts when observing concurrent generation changes (default 3)
}

func (o *AuditorOptions) withDefaults() AuditorOptions {
	res := *o
	if res.Interval <= 0 {
		res.Interval = 10 * time.Minute
	}
	if res.Debounce <= 0 {
		res.Debounce = 5 * time.Second
	}
	if res.Timeout <= 0 {
		res.Timeout = 5 * time.Minute
	}
	if res.MaxRetries <= 0 {
		res.MaxRetries = 3
	}
	return res
}

// Auditor is the bounded, rate-limited, single-flight integrity auditor
// (§4.3 of the lock-contention contract). It is the sole authority to transition
// suspect->ok and ok|suspect->degraded for runtime integrity findings.
type Auditor struct {
	store      *Store
	opts       AuditorOptions
	flight     singleflight.Group
	mu         sync.Mutex
	lastReport AuditReport
	lastRun    time.Time
	running    bool
	rootCtx    context.Context
	rootCancel context.CancelFunc
	done       chan struct{}
	triggerCh  chan bool
}

// NewAuditor creates an Auditor for store s.
func NewAuditor(s *Store, opts AuditorOptions) *Auditor {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &Auditor{
		store:      s,
		opts:       opts.withDefaults(),
		rootCtx:    rootCtx,
		rootCancel: rootCancel,
		done:       make(chan struct{}),
		triggerCh:  make(chan bool, 8),
	}
}

// Start launches the background periodic audit loop.
func (a *Auditor) Start() {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return
	}
	a.running = true
	a.mu.Unlock()

	go a.loop()
}

// Close cancels and stops the background auditor, waiting for any in-flight
// audit to finish.
func (a *Auditor) Close() error {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return nil
	}
	a.running = false
	a.rootCancel()
	a.mu.Unlock()

	<-a.done
	return nil
}

// Trigger queues an audit. If force is true, it bypasses debouncing.
func (a *Auditor) Trigger(force bool) {
	select {
	case a.triggerCh <- force:
	default:
	}
}

// Report returns a copy of the latest audit report.
func (a *Auditor) Report() AuditReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastReport
}

func (a *Auditor) loop() {
	defer close(a.done)

	// Boot audit: run once on boot if healthy.
	if a.store != nil && a.store.State() != StateDegraded && a.store.preflight == nil {
		select {
		case <-a.rootCtx.Done():
			return
		default:
			_, _ = a.Audit(a.rootCtx, false)
		}
	}

	ticker := time.NewTicker(a.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-a.rootCtx.Done():
			return
		case <-ticker.C:
			_, _ = a.Audit(a.rootCtx, false)
		case force := <-a.triggerCh:
			_, _ = a.Audit(a.rootCtx, force)
		}
	}
}

// Audit runs or joins a single-flight integrity audit. If force is false, it returns
// the cached report if called within the debounce interval.
func (a *Auditor) Audit(ctx context.Context, force bool) (*AuditReport, error) {
	if !force {
		a.mu.Lock()
		if !a.lastRun.IsZero() && time.Since(a.lastRun) < a.opts.Debounce {
			rep := a.lastReport
			a.mu.Unlock()
			return &rep, nil
		}
		a.mu.Unlock()
	}

	resCh := a.flight.DoChan("audit", func() (any, error) {
		rep, err := a.runAuditPass()
		return rep, err
	})

	select {
	case res := <-resCh:
		if res.Err != nil {
			return nil, res.Err
		}
		if res.Val != nil {
			return res.Val.(*AuditReport), nil
		}
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *Auditor) runAuditPass() (*AuditReport, error) {
	start := time.Now()
	timeoutCtx, cancel := context.WithTimeout(a.rootCtx, a.opts.Timeout)
	defer cancel()

	var failures []store.ScanFailure
	var auditErr error

	for attempt := 0; attempt < a.opts.MaxRetries; attempt++ {
		fireAuditSeam("start_attempt")
		if err := timeoutCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return a.recordResult(AuditResultTimeout, time.Since(start), nil, nil), nil
			}
			return a.recordResult(AuditResultCanceled, time.Since(start), nil, err), nil
		}

		genBefore := a.store.Generation()
		epochBefore := a.store.writeEpoch()

		curFailures, err := a.verifyStore(timeoutCtx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
				return a.recordResult(AuditResultTimeout, time.Since(start), nil, nil), nil
			}
			if errors.Is(err, context.Canceled) || errors.Is(timeoutCtx.Err(), context.Canceled) {
				return a.recordResult(AuditResultCanceled, time.Since(start), nil, err), nil
			}
			auditErr = err
		}

		genAfter := a.store.Generation()

		// Stable only if no writer held the slot at any point during the pass:
		// the epoch was even (quiescent) before and unchanged after. The
		// generation alone is bumped after the engine commit, so it can miss a
		// write that landed between Count and Scan.
		if genBefore == genAfter && epochBefore%2 == 0 && epochBefore == a.store.writeEpoch() {
			// Observed a stable generation: count and scan saw consistent state.
			failures = curFailures
			break
		}

		// A concurrent write occurred while verifying (genBefore != genAfter).
		// A count or index mismatch observed during in-flight writes is not a confirmed failure.
		// If curFailures contained preflight/VerifyAgentStore findings or structural corruption,
		// keep retrying to observe a quiet window.
		time.Sleep(15 * time.Millisecond)
	}

	if len(failures) > 0 {
		u := newUnhealthy(failures...)
		a.store.TransitionDegraded(u)
		return a.recordResult(AuditResultFindings, time.Since(start), failures, nil), nil
	}

	if auditErr != nil {
		return a.recordResult(AuditResultError, time.Since(start), nil, auditErr), nil
	}

	// Clean audit: transition suspect -> ok if applicable (preserves latched degraded).
	a.store.TransitionOK()
	return a.recordResult(AuditResultClean, time.Since(start), nil, nil), nil
}

func (a *Auditor) verifyStore(ctx context.Context) ([]store.ScanFailure, error) {
	var allFailures []store.ScanFailure

	// If boot preflight failed, the persisted store was confirmed corrupt at boot.
	if a.store.preflight != nil {
		allFailures = append(allFailures, a.store.preflight.Failures...)
	}

	// Verify "agents" collection online:
	if a.store.col != nil {
		f, err := a.verifyCollection(ctx, a.store.col, "agents", false)
		if err != nil {
			return nil, err
		}
		allFailures = append(allFailures, f...)
	}

	// Verify "closed" collection:
	if a.store.closed != nil {
		f, err := a.verifyCollection(ctx, a.store.closed, "closed", true)
		if err != nil {
			return nil, err
		}
		allFailures = append(allFailures, f...)
	}

	return allFailures, nil
}

func (a *Auditor) verifyCollection(ctx context.Context, col *engine.Collection, name string, tolerateDecode bool) ([]store.ScanFailure, error) {
	var failures []store.ScanFailure

	// 1. Online engine-level integrity check (segment extents, checksums, secondary indexes).
	if rep, err := col.Verify(ctx, engine.VerifyOptions{Mode: engine.VerifyFull}); err != nil {
		failures = append(failures, store.ScanFailure{
			Collection: name,
			Class:      store.DegradeIntegrity,
			Detail:     fmt.Sprintf("engine verify: %v", err),
		})
	} else if errs := ReportFailures(rep); len(errs) > 0 {
		failures = append(failures, errs...)
	}

	// 2. Count + Scan primary index vs records:
	want, err := col.Count(nil)
	if err != nil {
		failures = append(failures, store.ScanFailure{
			Collection: name,
			Class:      store.DegradeRead,
			Detail:     fmt.Sprintf("count failed: %v", err),
		})
		return failures, nil
	}

	rows, err := col.Scan(query.MatchAll)
	if err != nil {
		class := store.DegradeRead
		if strings.Contains(err.Error(), "decode") {
			class = store.DegradeDecode
		}
		failures = append(failures, store.ScanFailure{
			Collection: name,
			Class:      class,
			Detail:     fmt.Sprintf("scan failed: %v", err),
		})
		return failures, nil
	}

	// 3. Verify rows: Count vs len(rows), missing key, duplicate key, duplicate numeric id,
	// body id vs logical key, and decode failures.
	_, _, vErr := verifyRows(name, rows, want, tolerateDecode)
	if vErr != nil {
		if u, ok := IsUnhealthy(vErr); ok {
			failures = append(failures, u.Failures...)
		} else {
			failures = append(failures, store.ScanFailure{
				Collection: name,
				Class:      store.DegradeIntegrity,
				Detail:     vErr.Error(),
			})
		}
	}

	// 4. Point-lookup verification: probe GetByKey for indexed keys to ensure offsets decode correctly.
	if len(failures) == 0 {
		for _, r := range rows {
			key, _ := r.Data[engine.KeyField].(string)
			if key == "" {
				continue
			}
			rec, gerr := col.GetByKey(key)
			if gerr != nil {
				failures = append(failures, store.ScanFailure{
					Collection: name,
					Key:        key,
					Class:      store.DegradeIntegrity,
					Detail:     fmt.Sprintf("GetByKey failed for indexed key: %v", gerr),
				})
				break
			}
			if u := verifyRecord(name, key, rec); u != nil {
				failures = append(failures, u.Failures...)
				break
			}
		}
	}

	return failures, nil
}

func (a *Auditor) recordResult(res AuditResult, dur time.Duration, failures []store.ScanFailure, err error) *AuditReport {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now().UTC()
	a.lastRun = now
	a.lastReport.LastRunAt = now
	a.lastReport.LastResult = res
	a.lastReport.Duration = dur

	if res == AuditResultClean {
		a.lastReport.LastSuccessAt = now
		a.lastReport.Findings = nil
	} else if len(failures) > 0 {
		findings := make([]AuditFinding, 0, len(failures))
		for _, f := range failures {
			findings = append(findings, AuditFinding{
				Collection: f.Collection,
				Key:        f.Key,
				Class:      string(f.Class),
				Detail:     f.Detail,
			})
		}
		a.lastReport.Findings = findings
	} else if err != nil {
		a.lastReport.Findings = []AuditFinding{{
			Class:  "error",
			Detail: err.Error(),
		}}
	} else {
		a.lastReport.Findings = nil
	}

	rep := a.lastReport
	return &rep
}
