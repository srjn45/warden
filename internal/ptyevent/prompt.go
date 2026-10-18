package ptyevent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// PromptClass separates prompts warden may ever answer by policy from those it
// never may.
type PromptClass string

const (
	// ClassPermission asks to grant the agent an authority (run, write, fetch,
	// trust a folder, …). Postures decide these.
	ClassPermission PromptClass = "permission"
	// ClassDecision is a strategic or multiple-choice question (which approach,
	// which file, plan approval, free-text). No posture approves these — they
	// route to the decision router (brain/human).
	ClassDecision PromptClass = "decision"
	// ClassUnknown: the classifier could not tell. Always needs_attention.
	ClassUnknown PromptClass = "unknown"
)

// Valid reports whether c is known.
func (c PromptClass) Valid() bool {
	return c == ClassPermission || c == ClassDecision || c == ClassUnknown
}

// Capability is what a permission grants. A prompt may carry several (a shell
// command that pushes is execute+network+publish); postures require ALL of them
// to be permitted.
type Capability string

const (
	CapRead        Capability = "read"        // read files / list dirs inside the workspace
	CapWrite       Capability = "write"       // create/edit files
	CapExecute     Capability = "execute"     // run a command / tool / script
	CapNetwork     Capability = "network"     // fetch a URL, call an API, open a socket
	CapCredential  Capability = "credential"  // read/use secrets, keychain, tokens, login
	CapPublish     Capability = "publish"     // push, release, deploy, post, send
	CapDestructive Capability = "destructive" // delete, overwrite, reset, force, drop
	CapTrust       Capability = "trust"       // trust a workspace/directory (loads its hooks/config — a real permission)
	CapOther       Capability = "other"       // a permission the classifier cannot map
)

// Valid reports whether c is known.
func (c Capability) Valid() bool {
	switch c {
	case CapRead, CapWrite, CapExecute, CapNetwork, CapCredential, CapPublish, CapDestructive, CapTrust, CapOther:
		return true
	}
	return false
}

// OptionSemantic is what choosing an option means, independent of its label.
type OptionSemantic string

const (
	SemAllowOnce    OptionSemantic = "allow_once"    // grant this one request
	SemAllowSession OptionSemantic = "allow_session" // grant for the rest of the session
	SemAllowAlways  OptionSemantic = "allow_always"  // standing grant (don't ask again, trust folder)
	SemDeny         OptionSemantic = "deny"          // refuse, agent continues
	SemDenyFeedback OptionSemantic = "deny_feedback" // refuse and type guidance
	SemCancel       OptionSemantic = "cancel"        // abort the turn / exit the CLI
	SemChoice       OptionSemantic = "choice"        // a strategic alternative (decision prompts)
	SemFreeform     OptionSemantic = "freeform"      // opens a text field
)

// Valid reports whether s is known.
func (s OptionSemantic) Valid() bool {
	switch s {
	case SemAllowOnce, SemAllowSession, SemAllowAlways, SemDeny, SemDenyFeedback, SemCancel, SemChoice, SemFreeform:
		return true
	}
	return false
}

// Allows reports whether the option grants the request.
func (s OptionSemantic) Allows() bool {
	return s == SemAllowOnce || s == SemAllowSession || s == SemAllowAlways
}

// Refuses reports whether the option refuses without aborting.
func (s OptionSemantic) Refuses() bool { return s == SemDeny || s == SemDenyFeedback }

// Scope is how far a grant reaches.
type Scope string

const (
	ScopeOnce      Scope = "once"
	ScopeSession   Scope = "session"
	ScopeProject   Scope = "project"   // this repo/worktree (e.g. "don't ask again for rm in /proj")
	ScopeWorkspace Scope = "workspace" // a trusted directory tree
	ScopeGlobal    Scope = "global"    // user-wide setting
)

// Valid reports whether s is known ("" is allowed for non-grant options).
func (s Scope) Valid() bool {
	switch s {
	case "", ScopeOnce, ScopeSession, ScopeProject, ScopeWorkspace, ScopeGlobal:
		return true
	}
	return false
}

// Provenance is who produced the classification.
type Provenance string

const (
	// ProvParser: a backend adapter's deterministic parser.
	ProvParser Provenance = "parser"
	// ProvLearned: a learned shape (see LearnedShape) matched the screen.
	ProvLearned Provenance = "learned"
	// ProvFastBrain: a model read the screen; its claims were verified against
	// the screen (Verified) or the prompt is not confident.
	ProvFastBrain Provenance = "fastbrain"
)

// Valid reports whether p is known.
func (p Provenance) Valid() bool { return p == ProvParser || p == ProvLearned || p == ProvFastBrain }

// Option is one choice on the prompt.
type Option struct {
	Index    int            `json:"index"` // 1-based, top-down as rendered
	Label    string         `json:"label"` // as rendered (redacted)
	Key      string         `json:"key,omitempty"`
	Semantic OptionSemantic `json:"semantic"`
	Scope    Scope          `json:"scope,omitempty"`
	Default  bool           `json:"default,omitempty"` // what Enter selects right now
}

// CancelPath is how to back out without choosing: a key (Escape, Ctrl-C) or an
// option index. A prompt with no known cancel path can never be auto-denied.
type CancelPath struct {
	Key         string `json:"key,omitempty"`
	OptionIndex int    `json:"option_index,omitempty"`
}

// PromptEvidence is the redacted screen evidence behind a classification.
type PromptEvidence struct {
	// Raw is the redacted screen excerpt the prompt was read from.
	Raw        string      `json:"raw"`
	Redactions []Redaction `json:"redactions,omitempty"`
	// FromSeq..ToSeq are the output events the excerpt was rendered from.
	FromSeq uint64 `json:"from_seq,omitempty"`
	ToSeq   uint64 `json:"to_seq,omitempty"`
}

// Prompt is the normalized, provider-neutral view of one on-screen prompt.
type Prompt struct {
	// Fingerprint identifies the prompt SHAPE (backend + templated question +
	// templated options + shape version), not this instance: two `Bash(...)`
	// prompts with different commands share it. See Fingerprint().
	Fingerprint  string `json:"fingerprint"`
	ShapeVersion int    `json:"shape_version"`
	// InstanceKey identifies THIS instance (fingerprint + subject + target); the
	// TOCTOU check compares it.
	InstanceKey    string `json:"instance_key"`
	Backend        string `json:"backend"`
	BackendVersion string `json:"backend_version,omitempty"`

	Class        PromptClass  `json:"class"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Subject is the tool/actor asking ("Bash", "Edit", "WebFetch", "workspace").
	Subject string `json:"subject,omitempty"`
	// Target is what it acts on: the command, path, URL or directory (redacted).
	Target string `json:"target,omitempty"`
	// TargetInWorkspace is true when Target resolves inside the session's own
	// worktree (the adapter computes it; trust prompts depend on it).
	TargetInWorkspace bool   `json:"target_in_workspace,omitempty"`
	Question          string `json:"question,omitempty"`

	Options    []Option   `json:"options"`
	CancelPath CancelPath `json:"cancel_path"`
	// Navigate: options are chosen by cursor movement + Enter, not hotkeys.
	Navigate bool `json:"navigate,omitempty"`

	Provenance Provenance `json:"provenance"`
	Confidence float64    `json:"confidence"`
	// Verified is true when every option label was located on the live screen
	// (required for fastbrain/learned provenance to be confident).
	Verified bool `json:"verified"`
	// LearnedShapeID is set when Provenance is learned.
	LearnedShapeID string `json:"learned_shape_id,omitempty"`

	Evidence PromptEvidence `json:"evidence"`
}

// Validation errors for Prompt.
var (
	ErrPromptClass      = errors.New("unknown prompt class")
	ErrPromptProvenance = errors.New("unknown provenance")
	ErrPromptConfidence = errors.New("confidence must be in [0,1]")
	ErrPromptOptions    = errors.New("invalid options")
	ErrPromptCaps       = errors.New("invalid capabilities")
	ErrPromptCancel     = errors.New("cancel path names no option")
	ErrPromptFP         = errors.New("missing fingerprint")
)

// Validate checks internal consistency. It does NOT judge confidence (see
// Confident) — an unconfident prompt is valid and simply needs attention.
func (p *Prompt) Validate() error {
	if p.Fingerprint == "" {
		return ErrPromptFP
	}
	if !p.Class.Valid() {
		return fmt.Errorf("%w: %q", ErrPromptClass, p.Class)
	}
	if !p.Provenance.Valid() {
		return fmt.Errorf("%w: %q", ErrPromptProvenance, p.Provenance)
	}
	if p.Confidence < 0 || p.Confidence > 1 {
		return fmt.Errorf("%w: %v", ErrPromptConfidence, p.Confidence)
	}
	if len(p.Options) == 0 {
		return fmt.Errorf("%w: none", ErrPromptOptions)
	}
	defaults := 0
	for i, o := range p.Options {
		if o.Index != i+1 {
			return fmt.Errorf("%w: option %d has index %d", ErrPromptOptions, i+1, o.Index)
		}
		if !o.Semantic.Valid() || !o.Scope.Valid() {
			return fmt.Errorf("%w: option %d semantic=%q scope=%q", ErrPromptOptions, o.Index, o.Semantic, o.Scope)
		}
		if o.Default {
			defaults++
		}
	}
	if defaults > 1 {
		return fmt.Errorf("%w: %d defaults", ErrPromptOptions, defaults)
	}
	for _, c := range p.Capabilities {
		if !c.Valid() {
			return fmt.Errorf("%w: %q", ErrPromptCaps, c)
		}
	}
	if p.Class == ClassPermission && len(p.Capabilities) == 0 {
		return fmt.Errorf("%w: a permission must name at least one capability", ErrPromptCaps)
	}
	if ci := p.CancelPath.OptionIndex; ci != 0 && (ci < 1 || ci > len(p.Options)) {
		return fmt.Errorf("%w: %d", ErrPromptCancel, ci)
	}
	return nil
}

// HasCap reports whether p requests capability c.
func (p *Prompt) HasCap(c Capability) bool { return slices.Contains(p.Capabilities, c) }

// Confidence floors per provenance. A parser match is deterministic but still
// carries a confidence (an adapter may report a degraded match below the floor).
const (
	MinConfidenceParser    = 0.9
	MinConfidenceLearned   = 0.9
	MinConfidenceFastBrain = 0.95
)

// Confident reports whether a prompt is classified well enough to be acted on
// by policy. Learned and fastbrain readings must also be Verified on screen; a
// learned reading must come from a shape that is Usable (the caller passes that
// in, since shape state lives in the store). ClassUnknown is never confident.
func Confident(p *Prompt, learnedUsable bool) bool {
	if p == nil || p.Class == ClassUnknown || p.Validate() != nil {
		return false
	}
	switch p.Provenance {
	case ProvParser:
		return p.Confidence >= MinConfidenceParser
	case ProvLearned:
		return learnedUsable && p.Verified && p.Confidence >= MinConfidenceLearned
	case ProvFastBrain:
		return p.Verified && p.Confidence >= MinConfidenceFastBrain
	}
	return false
}

var (
	reQuoted = regexp.MustCompile("[\"'`][^\"'`]*[\"'`]")
	reParens = regexp.MustCompile(`\(([^()]*)\)`)
	rePath   = regexp.MustCompile(`(?:~|\.{1,2})?/[^\s"'` + "`" + `()]+`)
	reNum    = regexp.MustCompile(`\d+`)
	reSpace  = regexp.MustCompile(`\s+`)
)

// Template normalizes prompt text to its shape: instance-specific parts
// (quoted strings, call arguments, paths, numbers) become placeholders and
// whitespace/case are folded. It is the basis of Fingerprint, so changing it
// re-keys every learned shape — bump ShapeVersion when you do.
func Template(s string) string {
	s = reQuoted.ReplaceAllString(s, "‹q›")
	s = reParens.ReplaceAllString(s, "(‹arg›)")
	s = rePath.ReplaceAllString(s, "‹path›")
	s = reNum.ReplaceAllString(s, "‹n›")
	return strings.ToLower(strings.TrimSpace(reSpace.ReplaceAllString(s, " ")))
}

// Fingerprint returns the shape fingerprint for a prompt.
func Fingerprint(backend string, shapeVersion int, question string, options []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\x00%s\x00%s", shapeVersion, backend, Template(question))
	for _, o := range options {
		fmt.Fprintf(h, "\x00%s", Template(o))
	}
	return "fp1:" + hex.EncodeToString(h.Sum(nil))[:24]
}

// InstanceKey returns the per-instance key (shape + subject + target).
func InstanceKey(fingerprint, subject, target string) string {
	h := sha256.Sum256([]byte(fingerprint + "\x00" + subject + "\x00" + target))
	return "ik1:" + hex.EncodeToString(h[:])[:24]
}
