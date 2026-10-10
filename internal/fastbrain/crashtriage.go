package fastbrain

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CrashClass is the triage verdict for an agent failure.
type CrashClass string

const (
	CrashInternalBug      CrashClass = "internal_bug"      // warden itself is at fault
	CrashTransientError   CrashClass = "transient_error"   // 429 / network; switch backend
	CrashEnvironmentError CrashClass = "environment_error" // missing host tools
	CrashTaskFailure      CrashClass = "task_failure"      // the agent's project failed
)

// ExcerptLines is how many trailing lines of stderr/pane text are considered.
const ExcerptLines = 40

// CrashInput describes a failed agent process. Raw text is sanitized before it
// reaches the model or a draft.
type CrashInput struct {
	AgentID  string
	ExitCode int
	Signal   string
	Command  string
	Excerpt  string // stderr / pane tail; only the last ExcerptLines are used
	Version  string // warden version, e.g. "9.10.1" (a leading "v" is tolerated)
	GOOS     string
	GOARCH   string
	// StageDir, when non-empty and the crash is a warden bug, is where the
	// draft is written (see DefaultCrashDir). Empty = do not stage.
	StageDir string
}

// CrashDiagnosis is the triage result.
type CrashDiagnosis struct {
	Class                CrashClass
	IsWardenBug          bool
	SuggestSwitchBackend bool // transient_error: try another backend
	Confidence           float64
	Rationale            string
	Source               string      // "model" or "heuristic"
	Draft                *IssueDraft // non-nil only when IsWardenBug
	DraftPath            string      // set when the draft was staged
}

const crashSystem = `You triage a crashed coding-agent process. Classify the failure as exactly one of: "internal_bug" (Go panic, nil pointer, invariant violation inside the warden tool), "transient_error" (429 rate limit, network timeout, connection reset), "environment_error" (missing host tool such as docker/make/npm not on PATH), "task_failure" (test or compiler errors in the project the agent is working on). Reply with ONLY this JSON, no prose: {"class": "<one of the four>", "confidence": 0.0-1.0, "reason": "<short>"}`

func crashPrompt(in CrashInput, excerpt string) string {
	return fmt.Sprintf("%s\n\nExit code: %d\nSignal: %s\nCommand: %s\nLast output:\n%s\n",
		crashSystem, in.ExitCode, Sanitize(in.Signal), Sanitize(in.Command), excerpt)
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var (
	rePanic     = regexp.MustCompile(`(?m)^(?:panic:|fatal error:|goroutine \d+ \[)|runtime error:|nil pointer dereference`)
	reWardenFr  = regexp.MustCompile(`srjn45/warden|(?:^|[\s(])(?:internal|cmd)/[\w./-]+\.go`)
	reTransient = regexp.MustCompile(`(?i)\b429\b|rate.?limit|too many requests|timed? ?out|i/o timeout|connection reset|connection refused|temporar(?:y|ily) unavailable|\b50[234]\b|overloaded`)
	reEnv       = regexp.MustCompile(`(?i)(?:executable file not found|command not found|no such file or directory.*\b(?:docker|make|npm|node|git)\b|\b(?:docker|make|npm|node|git)\b: not found|not found in \$?PATH)`)
)

// heuristicClass is the deterministic fallback used when no model answers.
func heuristicClass(text string) CrashClass {
	switch {
	case rePanic.MatchString(text) && reWardenFr.MatchString(text):
		return CrashInternalBug
	case reEnv.MatchString(text):
		return CrashEnvironmentError
	case reTransient.MatchString(text):
		return CrashTransientError
	default:
		return CrashTaskFailure
	}
}

func validClass(c CrashClass) bool {
	switch c {
	case CrashInternalBug, CrashTransientError, CrashEnvironmentError, CrashTaskFailure:
		return true
	}
	return false
}

// DiagnoseFailure implements Engine. It never returns an error for model
// problems (those fail open to the heuristic); only for invalid input or a
// staging failure.
func (e *engine) DiagnoseFailure(ctx context.Context, in CrashInput) (CrashDiagnosis, error) {
	return diagnose(ctx, e, in)
}

// DiagnoseFailure is a thin wrapper over the Engine method; a nil engine uses
// the heuristic classifier only.
func DiagnoseFailure(ctx context.Context, e Engine, in CrashInput) (CrashDiagnosis, error) {
	if e == nil {
		return diagnose(ctx, nil, in)
	}
	return e.DiagnoseFailure(ctx, in)
}

func diagnose(ctx context.Context, d decider, in CrashInput) (CrashDiagnosis, error) {
	if strings.TrimSpace(in.Excerpt) == "" && in.ExitCode == 0 && in.Signal == "" {
		return CrashDiagnosis{}, fmt.Errorf("%w: empty crash input", ErrInvalidRequest)
	}
	excerpt := Sanitize(tailLines(in.Excerpt, ExcerptLines))
	out := CrashDiagnosis{Class: heuristicClass(excerpt), Source: "heuristic"}

	if d != nil {
		resp, err := d.Decide(ctx, Request{
			Kind: KindDiagnoseFailure, Tier: TierFast, Prompt: crashPrompt(in, excerpt),
			Metadata: AgentMeta(in.AgentID), Timeout: KindDeadline(KindDiagnoseFailure),
		})
		if err == nil && resp.OK() {
			var m struct {
				Class string `json:"class"`
			}
			if json.Unmarshal(resp.Output.Parsed, &m) == nil && validClass(CrashClass(m.Class)) {
				out.Class, out.Source = CrashClass(m.Class), "model"
				out.Confidence, out.Rationale = resp.Confidence, Sanitize(reason(resp))
			}
		}
	}
	if out.Source == "heuristic" {
		out.Rationale = "heuristic classifier"
	}

	out.IsWardenBug = out.Class == CrashInternalBug
	out.SuggestSwitchBackend = out.Class == CrashTransientError
	if out.IsWardenBug {
		out.Draft = buildDraft(in, excerpt, time.Now())
		if in.StageDir != "" {
			p, err := StageCrashDraft(in.StageDir, out.Draft)
			if err != nil {
				return out, err
			}
			out.DraftPath = p
		}
	}
	return out, nil
}
