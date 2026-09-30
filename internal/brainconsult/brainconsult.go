// Package brainconsult provides the shared Consultor abstraction: a single
// interface that spawns a short-lived role=brain agent, injects a structured
// prompt, waits for a single structured reply, tears the agent down, and
// returns the parsed result.
//
// It is importable by both internal/daemon (pipeline layer) and
// internal/autopilot (manager resolver) without creating an import cycle.
package brainconsult

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
)

// Action is the closed set of actions a brain may recommend.
// Consumers may offer subsets via Request.Allowed.
type Action string

const (
	ActionWait               Action = "wait"
	ActionNudgeAgent         Action = "nudge_agent"
	ActionRetryJob           Action = "retry_job"
	ActionMarkFailed         Action = "mark_failed"
	ActionSkipJob            Action = "skip_job"
	ActionEscalate           Action = "escalate"
	ActionNoop               Action = "noop"
	ActionUpdateTaskProgress Action = "update_task_progress"
)

// knownActions is the full closed enum for validation.
var knownActions = map[Action]bool{
	ActionWait:               true,
	ActionNudgeAgent:         true,
	ActionRetryJob:           true,
	ActionMarkFailed:         true,
	ActionSkipJob:            true,
	ActionEscalate:           true,
	ActionNoop:               true,
	ActionUpdateTaskProgress: true,
}

// ErrNoBrainReply is returned when the brain did not produce a parseable reply
// within the configured timeout.
var ErrNoBrainReply = errors.New("brain consult: no parseable reply received")

// Request is the input to a consultation.
type Request struct {
	Intent       string   // one-line summary, e.g. "stuck pipeline job"
	Situation    string   // free-form description of the current state
	Goal         string   // what outcome is desired
	AlreadyTried []string // actions already attempted (matched against Action constants)
	Evidence     string   // log excerpts, error messages, agent output snippets
	Allowed      []Action // subset of actions the caller accepts; nil = all of D2

	// Optional audit context fields; caller sets the ones that apply.
	PipelineID string
	JobID      string
	RunID      string
	TaskID     string

	// Repo overrides Options.Repo when non-empty — the working directory the
	// short-lived brain spawns in. Autopilot managers set this to their run's
	// repo so multi-repo daemons consult in the right tree.
	Repo string
}

// Result is what the brain decided.
type Result struct {
	Action       Action            // one of the D2 enum values
	Reason       string            // one-line explanation from the brain
	BrainID      string            // agent id of the spawned brain (for tracing/audit)
	TaskProgress map[string]string // non-nil when Action == ActionUpdateTaskProgress: task id → status
}

// Consultor is the single interface for all brain consult call sites.
type Consultor interface {
	// Consult spawns a role=brain agent, injects the structured prompt (D3),
	// waits for a single structured reply, tears the agent down, and returns
	// the result. It honours ctx cancellation and the configured timeout (D7).
	Consult(ctx context.Context, req Request) (Result, error)
}

// Spawner is the minimal lifecycle surface Consultor needs. The daemon
// implements this via a thin adapter that translates BrainSpawnArgs to its own
// SpawnRequest, keeping brainconsult import-cycle free.
type Spawner interface {
	// Spawn launches a headless brain agent and returns the session record.
	Spawn(ctx context.Context, args BrainSpawnArgs) (*agentstore.Agent, error)
	// Output returns the last n lines of the agent's pane output.
	Output(ctx context.Context, tmuxSession string, lines int) (string, error)
	// Teardown force-kills the agent's tmux session without touching the store.
	Teardown(ctx context.Context, sess *agentstore.Agent) error
}

// BrainSpawnArgs is the minimal set of fields Consultor needs to spawn a brain.
type BrainSpawnArgs struct {
	Cwd     string
	Prompt  string
	Role    string
	Backend string
	Tags    []string
	Repo    string
}

// Options configures a Consultor instance.
type Options struct {
	// Timeout is the per-consult deadline (default 10m).
	Timeout time.Duration
	// Role is the agent role to spawn (default "autopilot").
	Role string
	// Backend is the agent backend to use; empty uses the daemon default.
	Backend string
	// Repo is the working directory for the brain agent.
	Repo string
}

func (o *Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 10 * time.Minute
	}
	return o.Timeout
}

func (o *Options) role() string {
	if o.Role == "" {
		return "autopilot"
	}
	return o.Role
}

// New returns a Consultor backed by spawner, writing audit events via aw.
// aw may be nil (auditing disabled). opts tunes timeout and spawn parameters.
func New(spawner Spawner, aw *audit.Writer, opts Options) Consultor {
	return &consultor{
		spawner: spawner,
		aw:      aw,
		opts:    opts,
	}
}

type consultor struct {
	spawner Spawner
	aw      *audit.Writer
	opts    Options
}

// maxEvidenceBytes caps the evidence field injected into the brain prompt to
// prevent runaway token usage (D3).
const maxEvidenceBytes = 8 * 1024

// promptTmpl is the D3 frozen prompt template.
var promptTmpl = template.Must(template.New("consult").Funcs(template.FuncMap{
	"join": func(items []string, sep string) string { return strings.Join(items, sep) },
	"joinActions": func(items []Action, sep string) string {
		strs := make([]string, len(items))
		for i, a := range items {
			strs[i] = string(a)
		}
		return strings.Join(strs, sep)
	},
}).Parse(`{{if .Situation}}Scenario: {{.Situation}}
{{end}}{{if .Goal}}Goal: {{.Goal}}
{{end}}Already tried: {{join .AlreadyTried ", "}}
Evidence: {{.Evidence}}
Allowed next steps: [{{joinActions .Allowed ", "}}]

Reply with exactly one action id from the allowed list and a one-line reason,
as a single JSON object on a line by itself:
{"action": "<id>", "reason": "<one-line reason>"}`))

type promptData struct {
	Situation    string
	Goal         string
	AlreadyTried []string
	Evidence     string
	Allowed      []Action
}

func buildPrompt(req Request) (string, error) {
	evidence := req.Evidence
	if len(evidence) > maxEvidenceBytes {
		evidence = evidence[:maxEvidenceBytes]
	}

	allowed := req.Allowed
	if len(allowed) == 0 {
		allowed = []Action{
			ActionWait, ActionNudgeAgent, ActionRetryJob,
			ActionMarkFailed, ActionSkipJob, ActionEscalate, ActionNoop,
		}
	}

	var buf bytes.Buffer
	data := promptData{
		Situation:    strings.TrimSpace(req.Situation),
		Goal:         strings.TrimSpace(req.Goal),
		AlreadyTried: req.AlreadyTried,
		Evidence:     evidence,
		Allowed:      allowed,
	}
	if err := promptTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("build brain prompt: %w", err)
	}
	return buf.String(), nil
}

// brainReply is the frozen JSON reply format the brain produces.
type brainReply struct {
	Action       string            `json:"action"`
	Reason       string            `json:"reason"`
	TaskProgress map[string]string `json:"task_progress,omitempty"`
}

// Consult implements Consultor. It spawns a role=brain agent, injects the D3
// prompt, polls for the structured JSON reply, tears the agent down (always,
// via defer), and returns the result. It honours ctx cancellation and the
// configured timeout.
func (c *consultor) Consult(ctx context.Context, req Request) (Result, error) {
	timeout := c.opts.timeout()
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prompt, err := buildPrompt(req)
	if err != nil {
		return Result{}, err
	}

	allowed := req.Allowed
	if len(allowed) == 0 {
		allowed = []Action{
			ActionWait, ActionNudgeAgent, ActionRetryJob,
			ActionMarkFailed, ActionSkipJob, ActionEscalate, ActionNoop,
		}
	}

	repo := req.Repo
	if repo == "" {
		repo = c.opts.Repo
	}
	sess, err := c.spawner.Spawn(tctx, BrainSpawnArgs{
		Cwd:     repo,
		Repo:    repo,
		Prompt:  prompt,
		Role:    c.opts.role(),
		Backend: c.opts.Backend,
	})
	if err != nil {
		return Result{}, fmt.Errorf("brain consult: spawn: %w", err)
	}

	// Always tear the brain down when Consult returns, regardless of outcome.
	defer func() {
		tdctx, tdcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer tdcancel()
		if terr := c.spawner.Teardown(tdctx, sess); terr != nil {
			slog.Warn("brain consult: teardown failed", "brain_id", sess.ID, "err", terr)
		}
	}()

	result, err := c.waitForReply(tctx, sess.ID, sess.TmuxSession, allowed)
	if err != nil {
		return Result{BrainID: sess.ID}, err
	}
	result.BrainID = sess.ID

	c.aw.Log(audit.Event{
		Action: audit.ActionBrainConsult,
		Target: sess.ID,
		Detail: omitEmpty(map[string]string{
			"intent":      req.Intent,
			"action":      string(result.Action),
			"reason":      result.Reason,
			"pipeline_id": req.PipelineID,
			"job_id":      req.JobID,
			"run_id":      req.RunID,
			"task_id":     req.TaskID,
		}),
	})

	return result, nil
}

// pollInterval is how often Consult polls the brain's pane for a reply.
const pollInterval = 2 * time.Second

// outputLines is how many pane lines are captured per poll.
const outputLines = 200

// waitForReply polls the brain's pane until a parseable reply line appears or
// ctx is cancelled. The reply must contain an action from allowed.
func (c *consultor) waitForReply(ctx context.Context, brainID, tmuxSess string, allowed []Action) (Result, error) {
	allowedSet := make(map[Action]bool, len(allowed))
	for _, a := range allowed {
		allowedSet[a] = true
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return Result{}, ErrNoBrainReply
		case <-ticker.C:
			out, err := c.spawner.Output(ctx, tmuxSess, outputLines)
			if err != nil {
				slog.Warn("brain consult: output capture failed", "brain_id", brainID, "err", err)
				continue
			}
			if r, ok := parseReply(out, allowedSet); ok {
				return r, nil
			}
		}
	}
}

// parseReply scans output for a line matching the frozen JSON reply format and
// validates the action against allowedSet. Returns false when no valid line is
// found. If the brain emits an unrecognised action (not in the full closed
// enum) the caller treats it as noop per D2. If the action is recognised but
// not in allowedSet, it is also treated as noop.
func parseReply(output string, allowedSet map[Action]bool) (Result, bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r brainReply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Action == "" {
			continue
		}
		a := Action(r.Action)

		// An unrecognised action (not in the closed enum) → log + treat as noop.
		if !knownActions[a] {
			slog.Warn("brain consult: unrecognised action from brain; treating as noop",
				"action", r.Action, "reason", r.Reason)
			return Result{Action: ActionNoop, Reason: r.Reason}, true
		}

		// A recognised action not in the caller's allowed set → noop.
		if !allowedSet[a] {
			slog.Warn("brain consult: brain returned action not in allowed set; treating as noop",
				"action", r.Action)
			return Result{Action: ActionNoop, Reason: r.Reason}, true
		}

		return Result{Action: a, Reason: r.Reason, TaskProgress: r.TaskProgress}, true
	}
	return Result{}, false
}

// omitEmpty removes empty-string values from m so the audit Detail is compact.
func omitEmpty(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
