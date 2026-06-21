// Package config holds warden's user-facing configuration. Configuration lives
// in a single YAML file (default ~/.warden/config.yaml). Load reads it,
// Reconcile creates/migrates it, and DefaultPath resolves its location. The
// typed Config carries yaml tags for consumers; the parallel schema table (key
// + hint) drives file generation and migration. A drift-guard test asserts the
// two never diverge.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the typed view of warden's configuration. Every field carries a
// yaml tag matching a key in the on-disk config file. Duration-valued settings
// are stored as Go duration strings (e.g. "5m"); use the typed accessor methods
// (AutoRestartResetDuration, etc.) to read them.
type Config struct {
	Addr                   string `yaml:"addr"`
	DataDir                string `yaml:"data_dir"`
	ClaudeProjectsDir      string `yaml:"claude_projects_dir"`
	NotifyEnabled          bool   `yaml:"notify"`
	ApprovalsEnabled       bool   `yaml:"approvals"`
	AutoApproveEnabled     bool   `yaml:"auto_approve"`
	AutoApproveAllowSticky bool   `yaml:"auto_approve_allow_sticky"`
	DefaultPermissionMode  string `yaml:"default_permission_mode"`
	SpawnGateEnabled       bool   `yaml:"spawn_gate"`
	SpawnGateMaxAgents     int    `yaml:"spawn_gate_max_agents"`
	MetricsEnabled         bool   `yaml:"metrics"`
	AllowNonLoopback       bool   `yaml:"allow_nonloopback"`
	TokenGuard             bool   `yaml:"token_guard"`
	TokenWarnAlert         bool   `yaml:"token_warn_alert"`
	TokenAutoCompact       bool   `yaml:"token_auto_compact"`
	TokenWarn              int    `yaml:"token_warn"`
	TokenCritical          int    `yaml:"token_critical"`

	// Migrated from previously-scattered os.Getenv reads.
	PipelineKeepDone       bool   `yaml:"pipeline_keep_done"`
	ModelDefault           string `yaml:"model_default"`
	PipelineHint           bool   `yaml:"pipeline_hint"`
	AutoRestartMax         int    `yaml:"auto_restart_max"`
	AutoRestartReset       string `yaml:"auto_restart_reset"`
	RateLimitRetryInterval string `yaml:"rate_limit_retry_interval"`
	RateLimitBuffer        string `yaml:"rate_limit_buffer"`
	RateLimitAutoResume    bool   `yaml:"rate_limit_auto_resume"`
	RateLimitResumePrompt  string `yaml:"rate_limit_resume_prompt"`
}

// setting describes one config key for file generation/migration: its YAML key
// and the head-comment documenting its allowed values. The ordered schema slice
// is the source of truth for the file's layout; defaults() supplies the values.
type setting struct {
	Key  string
	Hint string
}

// schema is the ordered list of every config key with its documentation hint.
// Order here is the order keys are written to a freshly generated file. A
// reflection-based drift-guard test asserts this key set equals the set of
// yaml tags on Config.
var schema = []setting{
	{"addr", "Daemon listen address. Values: host:port (loopback only unless allow_nonloopback is true)"},
	{"data_dir", "Directory for warden state (sessions, inbox, pipelines, metrics). Values: absolute path"},
	{"claude_projects_dir", "Claude Code transcript root. Values: absolute path"},
	{"notify", "Desktop notifications on agent status changes. Values: true | false"},
	{"approvals", "Enable the approvals inbox (parse + answer permission prompts). Values: true | false"},
	{"auto_approve", "Automatically answer recognized yes/no permission prompts. Values: true | false"},
	{"auto_approve_allow_sticky", "When auto-approving, also accept \"yes, don't ask again\" (sticky) options. Values: true | false"},
	{"default_permission_mode", "Default permission mode for new agents.\nValues: auto | default | acceptEdits | bypassPermissions | dontAsk | plan"},
	{"spawn_gate", "Warn (soft, never blocks) before spawning when many agents are live. Values: true | false"},
	{"spawn_gate_max_agents", "Live-agent count that trips the spawn-gate warning. Values: integer"},
	{"metrics", "Record per-agent metrics to disk. Values: true | false"},
	{"allow_nonloopback", "Allow binding the auth-less daemon to a non-loopback address. Values: true | false"},
	{"token_guard", "Enable the context-token guard (warn / auto-compact). Values: true | false"},
	{"token_warn_alert", "Notify when an agent crosses the token warning threshold. Values: true | false"},
	{"token_auto_compact", "Auto-compact an agent that crosses the critical threshold. Values: true | false"},
	{"token_warn", "Token count for the warning threshold. Values: integer (must be < token_critical)"},
	{"token_critical", "Token count for the critical threshold. Values: integer (must be > token_warn)"},
	{"pipeline_keep_done", "Keep a pipeline job's agent alive after the job completes. Values: true | false"},
	{"model_default", "Default model for new agents. Values: a claude model id or alias (sonnet, opus, haiku, fable)"},
	{"pipeline_hint", "Append the pipeline-decomposition hint to standalone agents. Values: true | false"},
	{"auto_restart_max", "Max auto-restart attempts for an errored opted-in agent. Values: integer >= 0"},
	{"auto_restart_reset", "Sustained-health window that resets the restart counter. Values: Go duration (e.g. 5m, 1h)"},
	{"rate_limit_retry_interval", "Fallback wait before retrying after a rate limit. Values: Go duration (e.g. 30m, 1h)"},
	{"rate_limit_buffer", "Extra wait added on top of a parsed rate-limit reset time. Values: Go duration (e.g. 1m)"},
	{"rate_limit_auto_resume", "Auto-resume agents after a rate limit clears. Values: true | false"},
	{"rate_limit_resume_prompt", "Text to send when resuming a rate-limited agent. Empty = bare keypress (no injected user turn). Values: any string"},
}

// fileHeader is the comment written at the very top of a generated config file.
const fileHeader = "warden configuration — edit values below; run `warden config` to see what's live."

// defaults returns a fully-populated Config holding every setting's default.
// It is the single source of truth for default values (file generation reads
// the values from here; Load starts from here and overlays the file).
func defaults() Config {
	return Config{
		Addr:                   "127.0.0.1:8765",
		DataDir:                defaultDataDir(),
		ClaudeProjectsDir:      defaultClaudeProjectsDir(),
		NotifyEnabled:          false,
		ApprovalsEnabled:       true,
		AutoApproveEnabled:     false,
		AutoApproveAllowSticky: false,
		DefaultPermissionMode:  "auto",
		SpawnGateEnabled:       true,
		SpawnGateMaxAgents:     5,
		MetricsEnabled:         true,
		AllowNonLoopback:       false,
		TokenGuard:             true,
		TokenWarnAlert:         true,
		TokenAutoCompact:       true,
		TokenWarn:              200000,
		TokenCritical:          400000,
		PipelineKeepDone:       false,
		ModelDefault:           "claude-sonnet-4-6", // current "sonnet" alias; keep in sync with lifecycle.DefaultModel
		PipelineHint:           true,
		AutoRestartMax:         3,
		AutoRestartReset:       "5m",
		RateLimitRetryInterval: "30m",
		RateLimitBuffer:        "1m",
		RateLimitAutoResume:    true,
		RateLimitResumePrompt:  "",
	}
}

func defaultClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".claude/projects"
	}
	return filepath.Join(home, ".claude", "projects")
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".warden"
	}
	return filepath.Join(home, ".warden")
}

// DefaultPath returns the canonical config file location (~/.warden/config.yaml).
// It is bootstrap state: resolved before, and independent of, the data_dir
// setting the file itself contains. Falls back gracefully when home is unknown.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".warden", "config.yaml")
	}
	return filepath.Join(home, ".warden", "config.yaml")
}

// Load reads config from path, applying defaults for any missing keys and
// validating the result. A missing or unreadable file yields an all-defaults
// Config. Load never writes.
func Load(path string) Config {
	c := defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return c // absent/unreadable → all defaults
	}
	// Unmarshal overlays only the keys present in the file; absent keys keep
	// their default value (c was pre-populated by defaults()).
	if err := yaml.Unmarshal(data, &c); err != nil {
		log.Printf("WARN: config %s: parse error (%v); using defaults", path, err)
		return defaults()
	}
	validate(&c)
	return c
}

// validate normalizes a loaded Config against the same rules the env-based
// loader enforced: required-string fallbacks, the permission-mode whitelist,
// the token warn/critical ordering, and well-formed duration strings.
func validate(c *Config) {
	d := defaults()
	if strings.TrimSpace(c.Addr) == "" {
		c.Addr = d.Addr
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = d.DataDir
	}
	if strings.TrimSpace(c.ClaudeProjectsDir) == "" {
		c.ClaudeProjectsDir = d.ClaudeProjectsDir
	}
	if strings.TrimSpace(c.ModelDefault) == "" {
		c.ModelDefault = d.ModelDefault
	}
	c.DefaultPermissionMode = validPermissionMode(c.DefaultPermissionMode)
	if c.TokenCritical <= c.TokenWarn { // inverted/degenerate → defaults (warning must be reachable)
		c.TokenWarn, c.TokenCritical = d.TokenWarn, d.TokenCritical
	}
	if c.AutoRestartMax < 0 {
		c.AutoRestartMax = d.AutoRestartMax
	}
	c.AutoRestartReset = validDuration(c.AutoRestartReset, d.AutoRestartReset)
	c.RateLimitRetryInterval = validDuration(c.RateLimitRetryInterval, d.RateLimitRetryInterval)
	c.RateLimitBuffer = validDuration(c.RateLimitBuffer, d.RateLimitBuffer)
}

func validPermissionMode(v string) string {
	switch v {
	case "acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan":
		return v
	}
	if v != "" {
		log.Printf("WARN: invalid default_permission_mode=%q, using 'auto'", v)
	}
	return "auto"
}

func validDuration(v, def string) string {
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return v
	}
	if strings.TrimSpace(v) != "" {
		log.Printf("WARN: invalid duration %q, using %q", v, def)
	}
	return def
}

// Reconcile is the only writer. When the file is absent it generates a full,
// commented file from the schema + defaults. When present it parses the node
// tree and appends only the keys not already there (with their hint comments),
// preserving existing values, comments, and unknown keys untouched.
func Reconcile(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		out, err := renderFull()
		if err != nil {
			return err
		}
		return writeFile(path, out)
	}
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	// Empty or non-mapping document → regenerate from scratch.
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		out, err := renderFull()
		if err != nil {
			return err
		}
		return writeFile(path, out)
	}
	mapping := doc.Content[0]

	present := map[string]bool{}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		present[mapping.Content[i].Value] = true
	}
	defVals, err := defaultValueNodes()
	if err != nil {
		return err
	}
	changed := false
	for _, s := range schema {
		if present[s.Key] {
			continue
		}
		val, ok := defVals[s.Key]
		if !ok {
			continue
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s.Key, HeadComment: comment(s.Hint)}
		mapping.Content = append(mapping.Content, key, val)
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := marshalNode(&doc)
	if err != nil {
		return err
	}
	return writeFile(path, out)
}

// renderFull builds a complete, commented config document from defaults().
func renderFull() ([]byte, error) {
	mapping, err := defaultsMapping()
	if err != nil {
		return nil, err
	}
	hints := map[string]string{}
	for _, s := range schema {
		hints[s.Key] = s.Hint
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		if h, ok := hints[key.Value]; ok {
			key.HeadComment = comment(h)
		}
	}
	mapping.HeadComment = comment(fileHeader)
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{mapping}}
	return marshalNode(doc)
}

// defaultsMapping marshals defaults() into a YAML mapping node (keys in struct
// order, values as properly-typed scalars). It is the value source for both
// full generation and add-missing reconcile.
func defaultsMapping() (*yaml.Node, error) {
	b, err := yaml.Marshal(defaults())
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config: unexpected defaults node shape")
	}
	return doc.Content[0], nil
}

// defaultValueNodes returns a key→value-node map of every default, used to
// append missing keys during reconcile.
func defaultValueNodes() (map[string]*yaml.Node, error) {
	mapping, err := defaultsMapping()
	if err != nil {
		return nil, err
	}
	out := map[string]*yaml.Node{}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		out[mapping.Content[i].Value] = mapping.Content[i+1]
	}
	return out, nil
}

// comment turns hint text into a YAML head-comment, prefixing every line with
// "# " (yaml.v3 emits the stored string verbatim).
func comment(text string) string {
	return "# " + strings.ReplaceAll(text, "\n", "\n# ")
}

func marshalNode(n *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// legacyEnvNames are the config var basenames warden read from the environment
// before the move to a config file. They are now ignored; WarnIfLegacyEnv warns
// once at startup if any are still set.
var legacyEnvNames = []string{
	"ADDR", "DATA_DIR", "NOTIFY", "APPROVALS", "AUTO_APPROVE",
	"AUTO_APPROVE_ALLOW_STICKY",
	"DEFAULT_PERMISSION_MODE", "SPAWN_GATE", "SPAWN_GATE_MAX_AGENTS",
	"METRICS", "ALLOW_NONLOOPBACK", "TOKEN_GUARD", "TOKEN_WARN_ALERT",
	"TOKEN_AUTO_COMPACT", "TOKEN_WARN", "TOKEN_CRITICAL",
	"PIPELINE_KEEP_DONE", "MODEL_DEFAULT", "NO_PIPELINE_HINT",
	"AUTO_RESTART_MAX", "AUTO_RESTART_RESET", "RATE_LIMIT_RETRY_INTERVAL",
	"RATE_LIMIT_BUFFER", "RATE_LIMIT_AUTO_RESUME",
}

// WarnIfLegacyEnv logs a warning for each WARDEN_*/AGENTCTL_* (or bare
// CLAUDE_PROJECTS_DIR) config env var that is still set. Configuration now comes
// only from the file at path; these vars are ignored. Per-agent IPC vars
// (WARDEN_SESSION_ID, WARDEN_PIPELINE_ID, WARDEN_JOB_ID) are not config and are
// unaffected.
func WarnIfLegacyEnv(path string) {
	var found []string
	for _, name := range legacyEnvNames {
		for _, prefix := range []string{"WARDEN_", "AGENTCTL_"} {
			if _, ok := os.LookupEnv(prefix + name); ok {
				found = append(found, prefix+name)
			}
		}
	}
	if _, ok := os.LookupEnv("CLAUDE_PROJECTS_DIR"); ok {
		found = append(found, "CLAUDE_PROJECTS_DIR")
	}
	for _, k := range found {
		log.Printf("WARN: %s is set but ignored — warden now reads configuration from %s (run `warden config`)", k, path)
	}
}

// IsLoopbackHost reports whether addr (host:port, or a bare host) binds only the
// loopback interface. An empty host (e.g. ":8765") binds all interfaces and is
// NOT loopback. Unresolvable hostnames are treated as non-loopback (fail safe).
// No DNS lookups.
func IsLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port present
	}
	host = strings.TrimSpace(host)
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// GetDefaultPermissionMode returns the configured default permission mode for agents.
func (c Config) GetDefaultPermissionMode() string { return c.DefaultPermissionMode }

// GetModelDefault returns the configured default model id/alias for new agents.
func (c Config) GetModelDefault() string { return c.ModelDefault }

// GetPipelineHint reports whether the pipeline-decomposition hint is appended
// to standalone agents.
func (c Config) GetPipelineHint() bool { return c.PipelineHint }

// AutoRestartResetDuration returns the sustained-health window that resets the
// auto-restart counter.
func (c Config) AutoRestartResetDuration() time.Duration {
	return durOr(c.AutoRestartReset, 5*time.Minute)
}

// RateLimitRetryIntervalDuration returns the fallback wait before retrying a
// rate-limited agent.
func (c Config) RateLimitRetryIntervalDuration() time.Duration {
	return durOr(c.RateLimitRetryInterval, 30*time.Minute)
}

// RateLimitBufferDuration returns the buffer added to a parsed rate-limit reset.
func (c Config) RateLimitBufferDuration() time.Duration {
	return durOr(c.RateLimitBuffer, time.Minute)
}

func durOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}
