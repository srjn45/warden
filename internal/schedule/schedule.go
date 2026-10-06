// Package schedule models recurring and one-shot agent/pipeline triggers and the
// pure logic that drives them: cron/at parsing, next-fire computation, and a
// file-backed store. All decision logic is side-effect-free; the daemon's
// reconcile loop performs the actual spawns. The whole feature is opt-in behind
// the scheduler_enabled config gate (default off) — mirroring the deliberately
// conservative decision recorded in the scheduled-pipelines decision doc.
package schedule

import (
	"encoding/json"
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
// with the usual @hourly/@daily/@weekly descriptors. A spec is evaluated in the
// daemon host's local time unless it starts with TZ=<zone> (or CRON_TZ=<zone>). The daemon ticks once a
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
	// Disabled records that an operator turned the schedule off, so a schedule
	// that was switched off can be told apart from a single-shot that is spent
	// (both have Enabled=false). Absent on schedules stored before it existed.
	Disabled bool `json:"disabled,omitempty"`

	// Agent fire payload (Mode == agent). Mirrors the spawn passthrough fields.
	Type   string `json:"type,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Cwd    string `json:"cwd,omitempty"`  // launch directory for a free-form agent (absolute)
	Role   string `json:"role,omitempty"` // agent role; empty = worker with a repo, else general
	Prompt string `json:"prompt,omitempty"`
	Agent  string `json:"agent,omitempty"`  // optional agent name passthrough
	Branch string `json:"branch,omitempty"` // optional development branch / pr-review checkout

	// Further agent spawn passthrough, same meaning as the matching `warden start`
	// flags. All optional; empty means "the daemon's default".
	Model          string   `json:"model,omitempty"`           // model ID for AiCli
	AiCli          string   `json:"ai_cli,omitempty"`          // AI CLI id (claude, aider, …)
	PermissionMode string   `json:"permission_mode,omitempty"` // explicit permission mode
	AutoRestart    bool     `json:"auto_restart,omitempty"`    // auto-resume the agent if it crashes
	Tags           []string `json:"tags,omitempty"`            // labels stamped on every spawned agent
	Tier           string   `json:"tier,omitempty"`            // model tier for the quota-balanced resolver
	ProjectID      string   `json:"project_id,omitempty"`      // project the agent joins; empty = path-matched to the launch dir at fire time

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

// State is the one-word lifecycle answer every surface shows for a schedule.
type State string

const (
	StateEnabled  State = "enabled"  // armed; a recurring schedule with a failed last run is still enabled
	StateDisabled State = "disabled" // turned off by the operator
	StateDone     State = "done"     // single-shot that fired successfully and will not fire again
	StateFailed   State = "failed"   // single-shot whose only fire failed
)

// State derives the schedule's lifecycle state from its stored fields. A
// disabled schedule that never recorded the operator's choice (stored before
// Disabled existed) counts as spent only when it is a single-shot that has
// fired; anything else switched off is disabled.
func (s Schedule) State() State {
	if s.Enabled {
		return StateEnabled
	}
	if !s.Disabled && s.Kind == KindAt && s.LastRun != nil {
		if s.LastError != "" {
			return StateFailed
		}
		return StateDone
	}
	return StateDisabled
}

// MarshalJSON adds the derived, read-only `state` field to the wire form so the
// web cockpit, the app and MCP get the same answer as the CLI.
func (s Schedule) MarshalJSON() ([]byte, error) {
	type plain Schedule
	return json.Marshal(struct {
		plain
		State State `json:"state"`
	}{plain(s), s.State()})
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
	Cwd    string
	Role   string
	Prompt string
	Agent  string
	Branch string

	Model          string
	AiCli          string
	PermissionMode string
	AutoRestart    bool
	Tags           []string
	Tier           string
	ProjectID      string

	// Pipeline mode (Spec set; agent fields ignored).
	Spec string
}

// New builds a validated Schedule from p, stamping CreatedAt and the first
// NextRun relative to now. It returns an error if the params are inconsistent
// (see Validate) or the cron/at spec does not parse.
func New(p Params, now time.Time) (*Schedule, error) {
	s := &Schedule{
		ID:      p.Name,
		Name:    p.Name,
		Cron:    strings.TrimSpace(p.Cron),
		At:      strings.TrimSpace(p.At),
		Enabled: true,
		Type:    p.Type,
		Repo:    p.Repo,
		Cwd:     p.Cwd,
		Role:    p.Role,
		Prompt:  p.Prompt,
		Agent:   p.Agent,
		Branch:  p.Branch,
		Spec:    p.Spec,

		Model:          p.Model,
		AiCli:          p.AiCli,
		PermissionMode: p.PermissionMode,
		AutoRestart:    p.AutoRestart,
		Tags:           p.Tags,
		Tier:           p.Tier,
		ProjectID:      p.ProjectID,
		CreatedAt:      now,
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
		if strings.TrimSpace(s.Model) != "" && strings.TrimSpace(s.AiCli) == "" {
			return fmt.Errorf("--model requires --aicli (alias --ai-cli)")
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

// AgentFlagConflicts lists the agent-only options set on a create request that
// also carries a pipeline spec, as the CLI flag names, in a stable order. A
// pipeline schedule fires the pipeline as written, so these would be silently
// ignored; the create path rejects the combination and names them instead.
func AgentFlagConflicts(p Params) []string {
	var out []string
	add := func(set bool, flag string) {
		if set {
			out = append(out, flag)
		}
	}
	add(strings.TrimSpace(p.Prompt) != "", "--prompt")
	add(strings.TrimSpace(p.Repo) != "", "--repo")
	add(strings.TrimSpace(p.Cwd) != "", "--cwd")
	add(strings.TrimSpace(p.Role) != "", "--role")
	add(strings.TrimSpace(p.Agent) != "", "--agent")
	add(strings.TrimSpace(p.Branch) != "", "--branch")
	add(strings.TrimSpace(p.Model) != "", "--model")
	add(strings.TrimSpace(p.AiCli) != "", "--aicli")
	add(strings.TrimSpace(p.PermissionMode) != "", "--permission-mode")
	add(p.AutoRestart, "--auto-restart")
	add(len(p.Tags) > 0, "--tags")
	add(strings.TrimSpace(p.Tier) != "", "--tier")
	add(strings.TrimSpace(p.ProjectID) != "", "--project")
	add(strings.TrimSpace(p.Type) != "", "type")
	return out
}

// CheckAtInFuture rejects a single-shot time that is not strictly after now. It
// is the create-time (and edit-time) guard against a wrong date launching an
// agent on the next tick; New and Recompute deliberately do not apply it, so the
// daemon's startup path still fires a single-shot that came due during downtime.
// The message shows the time as parsed (with its zone) and the current time.
func CheckAtInFuture(at string, now time.Time) error {
	t, err := ParseAt(at)
	if err != nil {
		return fmt.Errorf("invalid --at time %q: %w (want RFC3339, e.g. 2026-06-27T09:00:00Z, or 2026-06-27T09:00)", at, err)
	}
	if !t.After(now) {
		return fmt.Errorf("--at %q is %s, which is not in the future (it is now %s); pass a later time, or --now to fire once immediately",
			strings.TrimSpace(at), t.Format(time.RFC3339), now.Format(time.RFC3339))
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
	s.Disabled = !enabled
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

// RecordRun stamps a manual fire (`schedule run`) on s: the fire time, outcome
// and the run it produced, exactly as Advance does, but WITHOUT touching
// Enabled or NextRun — a test-fire must not consume a single-shot or move a
// cron schedule's next occurrence.
func RecordRun(s *Schedule, now time.Time, sessionID string, fireErr error) {
	t := now
	s.LastRun = &t
	if fireErr != nil {
		s.LastError = fireErr.Error()
	} else {
		s.LastError = ""
	}
	s.LastRunSessionID = sessionID
	s.LastRunStatus = ""
}

// Patch is a partial edit of a schedule: only non-nil fields change, and an
// empty string clears an optional field. Mirrors the `schedule edit` flags.
type Patch struct {
	Cron, At                                             *string // timing (exclusive)
	Repo, Cwd, Role, Prompt, Agent, Branch, Model, AiCli *string // agent payload
	Spec                                                 *string // pipeline payload
}

// agentFields lists the agent-payload fields the patch sets, as flag names.
func (p Patch) agentFields() []string {
	var out []string
	add := func(v *string, flag string) {
		if v != nil {
			out = append(out, flag)
		}
	}
	add(p.Prompt, "--prompt")
	add(p.Repo, "--repo")
	add(p.Cwd, "--cwd")
	add(p.Role, "--role")
	add(p.Agent, "--agent")
	add(p.Branch, "--branch")
	add(p.Model, "--model")
	add(p.AiCli, "--aicli")
	return out
}

// Empty reports whether the patch changes nothing.
func (p Patch) Empty() bool {
	return p.Cron == nil && p.At == nil && p.Spec == nil && len(p.agentFields()) == 0
}

// ApplyPatch applies p to s in place and re-validates the result. It enforces
// the edit rules: timing may switch between cron and at; a past at is rejected;
// agent and pipeline mode cannot be switched (create a new schedule). When s is
// enabled its NextRun is recomputed from now. On error s may be partially
// modified — apply to a copy first.
func ApplyPatch(s *Schedule, p Patch, now time.Time) error {
	if p.Empty() {
		return fmt.Errorf("nothing to change: pass at least one of --cron, --at, --prompt, --cwd, --repo, --branch, --role, --model, --aicli, --agent or --pipeline")
	}
	if p.Cron != nil && p.At != nil {
		return fmt.Errorf("provide exactly one of --cron or --at, not both")
	}
	switch s.Mode {
	case ModePipeline:
		if fl := p.agentFields(); len(fl) > 0 {
			return fmt.Errorf("%s only apply to an agent schedule; %s fires a pipeline — create a new schedule to fire an agent", strings.Join(fl, ", "), s.Name)
		}
	default:
		if p.Spec != nil {
			return fmt.Errorf("--pipeline cannot be set on %s: it fires an agent — create a new schedule to fire a pipeline", s.Name)
		}
	}
	if p.Cron != nil {
		if strings.TrimSpace(*p.Cron) == "" {
			return fmt.Errorf("--cron cannot be empty")
		}
		s.Kind, s.Cron, s.At = KindCron, strings.TrimSpace(*p.Cron), ""
	}
	if p.At != nil {
		if err := CheckAtInFuture(*p.At, now); err != nil {
			return err
		}
		s.Kind, s.At, s.Cron = KindAt, strings.TrimSpace(*p.At), ""
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&s.Repo, p.Repo)
	set(&s.Cwd, p.Cwd)
	set(&s.Role, p.Role)
	set(&s.Prompt, p.Prompt)
	set(&s.Agent, p.Agent)
	set(&s.Branch, p.Branch)
	set(&s.Model, p.Model)
	set(&s.AiCli, p.AiCli)
	if p.Spec != nil {
		s.Spec = *p.Spec
	}
	if err := Validate(s); err != nil {
		return err
	}
	return Recompute(s, now)
}
