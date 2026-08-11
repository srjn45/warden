// Package schedule models recurring and one-shot agent/pipeline triggers and the
// pure logic that drives them: cron/at parsing, next-fire computation, and a
// file-backed store. All decision logic is side-effect-free; the daemon's
// reconcile loop performs the actual spawns. The whole feature is opt-in behind
// the scheduler_enabled config gate (default off) — mirroring the deliberately
// conservative decision recorded in the scheduled-pipelines decision doc.
package schedule

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/srjn45/warden/internal/store"
)

// Kind distinguishes a recurring (cron) schedule from a single-shot (at) one.
type Kind string

const (
	KindCron Kind = "cron" // recurring; re-arms after each fire via the cron spec
	KindAt   Kind = "at"   // single-shot; fires once at/after At then goes inactive
)

// Mode distinguishes what a schedule fires: a single agent spawn or a pipeline.
type Mode string

const (
	ModeAgent    Mode = "agent"    // spawn one agent (type/repo/prompt/name/branch)
	ModePipeline Mode = "pipeline" // create+start a pipeline from a stored YAML spec
)

// cronParser matches robfig/cron's default 5-field spec (minute-resolution),
// with the usual @hourly/@daily/@weekly descriptors. The daemon ticks once a
// minute, so second-resolution specs would not buy anything.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Schedule is one recurring or single-shot trigger. ID == Name (validated by
// store.SafeID), so a schedule is addressable by the name the operator gives it.
// The fire payload is either an agent spawn (Agent* fields) or a pipeline (the
// raw YAML Spec) depending on Mode.
type Schedule struct {
	ID      string `json:"id"`   // == Name; stable key
	Name    string `json:"name"` // operator-chosen handle
	Kind    Kind   `json:"kind"` // cron | at
	Mode    Mode   `json:"mode"` // agent | pipeline
	Cron    string `json:"cron,omitempty"`
	At      string `json:"at,omitempty"` // RFC3339-ish single-shot time
	Enabled bool   `json:"enabled"`

	// Agent fire payload (Mode == agent). Mirrors the spawn passthrough fields.
	Type   string `json:"type,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Agent  string `json:"agent,omitempty"`  // optional agent name passthrough
	Branch string `json:"branch,omitempty"` // optional development branch / pr-review checkout

	// Pipeline fire payload (Mode == pipeline): the raw pipeline YAML spec. It is
	// validated at create time (in the route handler) and re-parsed on each fire.
	Spec string `json:"spec,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	LastRun   *time.Time `json:"last_run,omitempty"`
	NextRun   *time.Time `json:"next_run,omitempty"`
	LastError string     `json:"last_error,omitempty"`

	// Durable last-run outcome, so a schedule row can show what its most recent
	// fire produced even after that session has been rotated or deleted. For an
	// agent-mode fire LastRunSessionID is the spawned agent; for a pipeline-mode
	// fire it is the created pipeline id (its jobs back-ref via Session.ScheduleID).
	// LastRunStatus is refreshed from the run's live status on each reconcile tick
	// while the session record still exists (running → exited/error).
	LastRunSessionID string `json:"last_run_session_id,omitempty"`
	LastRunStatus    string `json:"last_run_status,omitempty"`
}

// Params are the validated inputs used to build a Schedule (one per CLI/route
// create call). Exactly one of Cron/At and exactly one fire mode must be set.
type Params struct {
	Name string
	Cron string
	At   string

	// Agent mode (Spec empty).
	Type   string
	Repo   string
	Prompt string
	Agent  string
	Branch string

	// Pipeline mode (Spec set; agent fields ignored).
	Spec string
}

// New builds a validated Schedule from p, stamping CreatedAt and the first
// NextRun relative to now. It returns an error if the params are inconsistent
// (see Validate) or the cron/at spec does not parse.
func New(p Params, now time.Time) (*Schedule, error) {
	s := &Schedule{
		ID:        p.Name,
		Name:      p.Name,
		Cron:      strings.TrimSpace(p.Cron),
		At:        strings.TrimSpace(p.At),
		Enabled:   true,
		Type:      p.Type,
		Repo:      p.Repo,
		Prompt:    p.Prompt,
		Agent:     p.Agent,
		Branch:    p.Branch,
		Spec:      p.Spec,
		CreatedAt: now,
	}
	if s.Cron != "" {
		s.Kind = KindCron
	} else {
		s.Kind = KindAt
	}
	if strings.TrimSpace(p.Spec) != "" {
		s.Mode = ModePipeline
	} else {
		s.Mode = ModeAgent
	}
	if err := Validate(s); err != nil {
		return nil, err
	}
	if err := Recompute(s, now); err != nil {
		return nil, err
	}
	return s, nil
}

// Validate checks a schedule is well-formed: a safe id/name, exactly one timing
// spec (cron xor at) that parses, exactly one fire mode, and the fields that
// mode requires. It does NOT validate the pipeline YAML itself — the route
// handler does that with pipeline.ParseSpec to keep this package dependency-light.
func Validate(s *Schedule) error {
	if err := store.SafeID(s.Name); err != nil {
		return fmt.Errorf("invalid schedule name %q: must have no '/', '\\', ':', or '..'", s.Name)
	}
	switch {
	case s.Cron != "" && s.At != "":
		return fmt.Errorf("provide exactly one of --cron or --at, not both")
	case s.Cron == "" && s.At == "":
		return fmt.Errorf("provide a timing spec: --cron \"<spec>\" or --at <time>")
	case s.Cron != "":
		if _, err := ParseCron(s.Cron); err != nil {
			return fmt.Errorf("invalid cron %q: %w", s.Cron, err)
		}
	default:
		if _, err := ParseAt(s.At); err != nil {
			return fmt.Errorf("invalid --at time %q: %w (want RFC3339, e.g. 2026-06-27T09:00:00Z, or 2026-06-27T09:00)", s.At, err)
		}
	}
	switch s.Mode {
	case ModePipeline:
		if strings.TrimSpace(s.Spec) == "" {
			return fmt.Errorf("pipeline mode requires a spec")
		}
	case ModeAgent:
		if strings.TrimSpace(s.Prompt) == "" {
			return fmt.Errorf("agent mode requires --prompt")
		}
		// A typed spawn needs a repo (mirrors the daemon's spawn precondition); a
		// free-form spawn (empty type) does not.
		if strings.TrimSpace(s.Type) != "" && strings.TrimSpace(s.Repo) == "" {
			return fmt.Errorf("a typed agent schedule (--type %s) requires --repo", s.Type)
		}
	default:
		return fmt.Errorf("unknown fire mode %q", s.Mode)
	}
	return nil
}

// ParseCron parses a 5-field cron spec (with @descriptors) into a cron.Schedule.
func ParseCron(spec string) (cron.Schedule, error) {
	return cronParser.Parse(spec)
}

// atLayouts are the accepted single-shot time formats, tried in order. The bare
// (zone-less) layouts are interpreted in the machine's local time.
var atLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
}

// ParseAt parses a single-shot --at time. A value carrying a zone (RFC3339) is
// taken as-is; a zone-less value is interpreted in local time.
func ParseAt(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	for _, layout := range atLayouts {
		if strings.Contains(layout, "Z07:00") {
			if t, err := time.Parse(layout, v); err == nil {
				return t, nil
			}
			continue
		}
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format")
}

// NextCron returns the next activation strictly after `after` for a cron spec.
// robfig/cron's Next never backfills, so a long-idle daemon resumes at the next
// future occurrence rather than replaying missed ones.
func NextCron(spec string, after time.Time) (time.Time, error) {
	sched, err := ParseCron(spec)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after), nil
}

// Recompute (re)derives NextRun from the schedule's timing spec relative to now.
// For a cron schedule it is the next future occurrence after now. For an at
// schedule it is the fixed At instant (which may be in the past — Due then
// reports true so a past-due single-shot fires once on the next tick). A
// disabled schedule has no NextRun.
func Recompute(s *Schedule, now time.Time) error {
	if !s.Enabled {
		s.NextRun = nil
		return nil
	}
	switch s.Kind {
	case KindCron:
		next, err := NextCron(s.Cron, now)
		if err != nil {
			return err
		}
		s.NextRun = &next
	case KindAt:
		at, err := ParseAt(s.At)
		if err != nil {
			return err
		}
		s.NextRun = &at
	default:
		return fmt.Errorf("unknown schedule kind %q", s.Kind)
	}
	return nil
}

// SetEnabled flips a schedule's enabled state and re-arms it: enabling recomputes
// NextRun from now (cron → next occurrence; at → its configured time, which fires
// on the next tick if already past), disabling clears NextRun so it never fires.
// It returns an error only if an enabled schedule's spec fails to recompute, which
// should not happen for a schedule that validated at create time.
func SetEnabled(s *Schedule, enabled bool, now time.Time) error {
	s.Enabled = enabled
	return Recompute(s, now)
}

// Due reports whether s should fire at now: enabled, with a NextRun that is not
// in the future.
func Due(s *Schedule, now time.Time) bool {
	return s.Enabled && s.NextRun != nil && !s.NextRun.After(now)
}

// Advance records a fire at now and re-arms the schedule. A cron schedule's
// NextRun rolls forward to its next future occurrence; a single-shot at schedule
// goes inactive (Enabled=false, NextRun cleared) so it never re-fires. fireErr
// (possibly nil) is stored as LastError for operator visibility.
func Advance(s *Schedule, now time.Time, sessionID string, fireErr error) {
	t := now
	s.LastRun = &t
	if fireErr != nil {
		s.LastError = fireErr.Error()
	} else {
		s.LastError = ""
	}
	// Record the run this fire produced (empty on a fire that spawned nothing, or
	// on failure). Status is left for the reconcile loop to fill from the live
	// session; a fresh fire clears any stale status from the prior run.
	s.LastRunSessionID = sessionID
	s.LastRunStatus = ""
	switch s.Kind {
	case KindCron:
		if next, err := NextCron(s.Cron, now); err == nil {
			s.NextRun = &next
		} else {
			// A spec that parsed at create time should not fail here; disable
			// defensively rather than spin.
			s.Enabled = false
			s.NextRun = nil
		}
	default: // at: one and done
		s.Enabled = false
		s.NextRun = nil
	}
}
