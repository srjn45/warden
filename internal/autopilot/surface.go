package autopilot

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/srjn45/warden/internal/fastbrain"
)

// Operator-facing status surface (run-to-final-pr spec §G.3). The in-memory
// pieces live on run.surface; a ledger record (autopilot.<run>.surface) makes
// them survive a daemon restart, and per-task fix state is read straight from
// the ledger where the fix loop already persists it.

// Diagnosis is the guardian's last triage decision for a run.
type Diagnosis struct {
	Action         string  `json:"action"`
	Confidence     float64 `json:"confidence"`
	Rationale      string  `json:"rationale,omitempty"`
	FailOpenReason string  `json:"fail_open_reason,omitempty"`
	Source         string  `json:"source,omitempty"`  // heuristic | model | failopen
	Outcome        string  `json:"outcome,omitempty"` // applied | mechanical
	At             string  `json:"at"`
}

// FixStatus is one task's gate/fix-loop state, assembled from the ledger.
type FixStatus struct {
	Task              string `json:"task"`
	PR                int    `json:"pr,omitempty"`
	Gate              string `json:"gate"` // red | conflict | clear
	Kind              string `json:"kind,omitempty"`
	RedStreak         int    `json:"red_streak"`
	FixAttempts       int    `json:"fix_attempts"`
	Reruns            int    `json:"reruns,omitempty"`
	Fixing            bool   `json:"fixing,omitempty"`
	LastDispatchedSHA string `json:"last_dispatched_sha,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

// ResolverStatus summarises the run's resolver-agent activity.
type ResolverStatus struct {
	Attempts    int    `json:"attempts"`               // total spawns this run
	LastClass   string `json:"last_class,omitempty"`   // red_gate | manager_stall
	LastTask    string `json:"last_task,omitempty"`    // task the blocker belonged to
	LastBranch  string `json:"last_branch,omitempty"`  // branch it worked on
	LastOutcome string `json:"last_outcome,omitempty"` // started | exhausted | start_failed
	LastAt      string `json:"last_at,omitempty"`
}

// surfaceRecord is the persisted slice of the status surface.
type surfaceRecord struct {
	LastDiagnosis *Diagnosis      `json:"last_diagnosis,omitempty"`
	Resolver      *ResolverStatus `json:"resolver,omitempty"`
	FinalPR       *FinalPR        `json:"final_pr,omitempty"`
}

// SurfaceKey is the ctx key of the persisted status-surface record.
func (l *Ledger) SurfaceKey() string { return l.key("surface") }

func (l *Ledger) loadSurface() surfaceRecord {
	var rec surfaceRecord
	_ = l.readJSON(l.SurfaceKey(), &rec)
	return rec
}

func (l *Ledger) saveSurface(rec surfaceRecord) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = l.store.Set(l.SurfaceKey(), string(b), "daemon-surface")
}

// runLedger returns the run's ledger, or nil when the runtime has none.
func (c *Controller) runLedger(runID string) *Ledger {
	if c.runtime == nil {
		return nil
	}
	return c.runtime.NewLedger(runID)
}

// surfaceLocked returns the run's surface record, hydrating it once from the
// ledger after a restart. The caller must hold c.mu.
func (c *Controller) surfaceLocked(r *run) *surfaceRecord {
	if !r.surfaceLoaded {
		r.surfaceLoaded = true
		if l := c.runLedger(r.runID); l != nil {
			r.surface = l.loadSurface()
		}
	}
	return &r.surface
}

// persistSurfaceLocked writes the surface record. The caller must hold c.mu.
func (c *Controller) persistSurfaceLocked(r *run) {
	if l := c.runLedger(r.runID); l != nil {
		l.saveSurface(*c.surfaceLocked(r))
	}
}

// recordDiagnosis stores the last triage decision (caller holds c.mu).
func (c *Controller) recordDiagnosis(r *run, d fastbrain.StallDiagnosis, outcome string) {
	s := c.surfaceLocked(r)
	s.LastDiagnosis = &Diagnosis{
		Action: string(d.Action), Confidence: d.Confidence, Rationale: d.Rationale,
		FailOpenReason: d.FailOpen, Source: d.Source, Outcome: outcome,
		At: c.now().UTC().Format(time.RFC3339),
	}
	c.persistSurfaceLocked(r)
}

// recordResolver stores the latest resolver outcome (caller holds c.mu).
func (c *Controller) recordResolver(r *run, req ResolverRequest, branch, outcome string) {
	s := c.surfaceLocked(r)
	rs := ResolverStatus{}
	if s.Resolver != nil {
		rs = *s.Resolver
	}
	if outcome == "started" {
		rs.Attempts++
	}
	rs.LastClass, rs.LastTask, rs.LastBranch = req.Class, req.TaskID, branch
	rs.LastOutcome, rs.LastAt = outcome, c.now().UTC().Format(time.RFC3339)
	s.Resolver = &rs
	c.persistSurfaceLocked(r)
}

// surfaceView is what status assembly adds to a RunStatus.
type surfaceView struct {
	diag     *Diagnosis
	fix      []FixStatus
	resolver *ResolverStatus
	finalPR  *FinalPR
}

// surfaceViewLocked assembles the status surface; in-memory completion state
// wins over the persisted final PR (which only fills in after a restart).
func (c *Controller) surfaceViewLocked(r *run) surfaceView {
	s := c.surfaceLocked(r)
	v := surfaceView{diag: s.LastDiagnosis, finalPR: r.completion.finalPR.snapshot()}
	if v.finalPR == nil {
		v.finalPR = s.FinalPR.snapshot()
	}
	if s.Resolver != nil || len(r.resolverAttempts) > 0 {
		rs := ResolverStatus{Attempts: sumAttempts(r.resolverAttempts)}
		if s.Resolver != nil {
			rs = *s.Resolver
		}
		v.resolver = &rs
	}
	if l := c.runLedger(r.runID); l != nil {
		v.fix = fixStatuses(l, r.plan.Tasks)
	}
	return v
}

// fixStatuses lists the tasks that have ever entered the fix loop.
func fixStatuses(l *Ledger, tasks []PlanTask) []FixStatus {
	var out []FixStatus
	for _, t := range tasks {
		fs, err := l.FixState(t.ID)
		if err != nil || (fs.HeadSHA == "" && fs.FixAttempts == 0 && fs.Reruns == 0 && fs.RedStreak == 0) {
			continue
		}
		gate := "clear"
		if fs.RedStreak > 0 || fs.Fixing {
			gate = fs.Kind
			if gate == "" {
				gate = "red"
			}
		}
		out = append(out, FixStatus{Task: t.ID, PR: fs.PR, Gate: gate, Kind: fs.Kind,
			RedStreak: fs.RedStreak, FixAttempts: fs.FixAttempts, Reruns: fs.Reruns,
			Fixing: fs.Fixing, LastDispatchedSHA: fs.LastDispatchedSHA, UpdatedAt: fs.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out
}

func (v surfaceView) apply(st *RunStatus) {
	st.LastDiagnosis, st.Fix, st.Resolver, st.FinalPR = v.diag, v.fix, v.resolver, v.finalPR
}
