package wardendb

import (
	"context"
	"time"
)

// Report is the diagnostic report of a warden-db directory: a JSON-serializable
// rendering of a VerifyResult that is safe to write to a log, a support bundle
// or a terminal. It carries structure only — codes, classes, segment names,
// offsets and counts. It never carries a record payload, an engine message
// (which may quote indexed field values), or the owner's command line or
// listen address.
type Report struct {
	Dir           string    `json:"dir"`
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Health        Health    `json:"health"`
	Exists        bool      `json:"exists"`
	// Online: produced by the owning handle rather than a read-only pass.
	Online bool       `json:"online"`
	Lock   ReportLock `json:"lock"`

	VerifyStartedAt  time.Time `json:"verify_started_at"`
	VerifyFinishedAt time.Time `json:"verify_finished_at"`

	Blocking    int `json:"blocking_findings"`
	Recoverable int `json:"recoverable_findings"`

	Collections []ReportCollection `json:"collections"`
	// Database lists the findings that belong to no single collection (the
	// cross-collection transaction state).
	Database []ReportFinding `json:"database_findings"`
}

// ReportLock is the ownership section of a Report.
type ReportLock struct {
	State LockState    `json:"state"`
	Owner *ReportOwner `json:"owner,omitempty"`
}

// ReportOwner identifies the process that owns the directory.
type ReportOwner struct {
	PID     int       `json:"pid"`
	Kind    string    `json:"kind"`
	Launch  string    `json:"launch,omitempty"`
	Version string    `json:"version,omitempty"`
	Started time.Time `json:"started"`
}

// ReportCollection is the per-collection section of a Report.
type ReportCollection struct {
	Name         string `json:"name"`
	Health       Health `json:"health"`
	Online       bool   `json:"online"`
	Registered   bool   `json:"registered"`
	Segments     int    `json:"segments"`
	SegmentBytes int64  `json:"segment_bytes"`
	LiveRecords  int    `json:"live_records"`
	Blocking     int    `json:"blocking_findings"`
	Recoverable  int    `json:"recoverable_findings"`
	// Truncated counts findings not listed below (engine and report caps).
	Truncated int             `json:"truncated_findings"`
	Findings  []ReportFinding `json:"findings"`
}

// ReportFinding is one finding, without its free-text message.
type ReportFinding struct {
	Collection string       `json:"collection,omitempty"`
	Severity   string       `json:"severity"`
	Code       string       `json:"code"`
	Class      FindingClass `json:"class"`
	Blocking   bool         `json:"blocking"`
	Segment    string       `json:"segment,omitempty"`
	Offset     int64        `json:"offset,omitempty"`
}

func reportFindingsOf(s FindingSet) []ReportFinding {
	out := make([]ReportFinding, 0, len(s.Blocking)+len(s.Recoverable))
	add := func(fs []Finding, block bool) {
		for _, f := range fs {
			out = append(out, ReportFinding{
				Collection: f.Collection, Severity: f.Severity, Code: f.Code, Class: ClassOf(f),
				Blocking: block, Segment: f.Segment, Offset: f.Offset,
			})
		}
	}
	add(s.Blocking, true)
	add(s.Recoverable, false)
	return out
}

// Report renders the result as a diagnostic report.
func (r *VerifyResult) Report() *Report {
	rep := &Report{
		Dir: r.Dir, SchemaVersion: SchemaVersion, GeneratedAt: time.Now().UTC(),
		Health: r.Health, Exists: r.Exists, Online: r.Online,
		Lock:            ReportLock{State: r.Lock.State},
		VerifyStartedAt: r.StartedAt.UTC(), VerifyFinishedAt: r.FinishedAt.UTC(),
		Blocking: r.Blocking, Recoverable: r.Recoverable,
		Collections: make([]ReportCollection, 0, len(r.Collections)),
		Database:    reportFindingsOf(r.Database),
	}
	if o := r.Lock.Owner; o != nil {
		rep.Lock.Owner = &ReportOwner{PID: o.PID, Kind: o.Kind, Launch: o.Launch, Version: o.Version, Started: o.Started}
	}
	for _, c := range r.Collections {
		fs := reportFindingsOf(c.FindingSet)
		rep.Collections = append(rep.Collections, ReportCollection{
			Name: c.Name, Health: c.Health, Online: c.Online, Registered: c.Registered,
			Segments: c.Segments, SegmentBytes: c.SegmentBytes, LiveRecords: c.LiveRecords,
			Blocking: c.BlockingTotal, Recoverable: c.RecoverableTotal,
			Truncated: c.Truncated + c.BlockingTotal + c.RecoverableTotal - len(fs),
			Findings:  fs,
		})
	}
	return rep
}

// Diagnose is the read-only diagnostic entry point: Verify rendered as a
// Report. Like Verify and Probe it never takes ownership and never mutates.
func Diagnose(ctx context.Context, dataDir string) (*Report, error) {
	res, err := Verify(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	return res.Report(), nil
}

// Diagnose is DB.Verify rendered as a Report. Like DB.Verify, a blocking
// finding retires the handle.
func (db *DB) Diagnose(ctx context.Context) (*Report, error) {
	res, err := db.Verify(ctx)
	if err != nil {
		return nil, err
	}
	return res.Report(), nil
}
