package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/store"
)

// ErrDaemonDown signals the daemon is unreachable (connection refused / timeout).
var ErrDaemonDown = errors.New("daemon not running\n\nRun: warden daemon\nOr install as a service: ./scripts/install.sh")

// StatusError is returned for non-2xx daemon responses, exposing the HTTP code.
type StatusError struct {
	Code int
	Msg  string
	Body []byte // raw response body (for structured 4xx payloads)
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("daemon error (%d): %s", e.Code, e.Msg)
}

// ErrConfirmationRequired is returned by Spawn when the daemon's memory-pressure
// gate warns (HTTP 428). Retry Spawn with SpawnParams.Force = true to proceed.
type ErrConfirmationRequired struct {
	Verdict pressure.Verdict
}

func (e *ErrConfirmationRequired) Error() string {
	return "spawn gate: " + e.Verdict.Reason + " — re-run with force to spawn anyway"
}

// Per-operation default deadlines, applied only when the caller's context has
// no deadline of its own. Reads are quick; spawn/adopt/remove-worktree run
// synchronously on the daemon (git worktree add, transcript scan) and can take
// far longer than a read — a single blanket client timeout would abort a
// slow-but-successful spawn while the daemon kept working, orphaning sessions.
// Vars (not consts) so tests can shrink them.
var (
	defaultTimeout = 30 * time.Second
	longTimeout    = 5 * time.Minute
)

type Client struct {
	base  string
	http  *http.Client
	token string
}

func New(base string) *Client {
	// No blanket http.Client.Timeout: per-call deadlines come from the context
	// (see do/doT), so long operations are not capped to a read-sized timeout.
	//
	// The bearer token lets the local CLI/TUI work transparently against an
	// authenticated daemon. It resolves from WARDEN_TOKEN if exported, otherwise
	// from the managed install's token file (~/.warden/token.env) — so a remote
	// install authenticates local clients without every shell exporting the
	// secret. When neither is present (the loopback-only default) the daemon
	// disables auth and ignores the empty header.
	return &Client{base: base, http: &http.Client{}, token: auth.ResolveToken()}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doT(ctx, defaultTimeout, method, path, in, out)
}

func (c *Client) doT(ctx context.Context, timeout time.Duration, method, path string, in, out any) error {
	// Give a deadline-less caller a sensible default so a hung daemon can't block
	// forever; never shorten or extend a deadline the caller set deliberately.
	if timeout > 0 {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Only a refused connection (nothing listening) means the daemon is down.
		// Other transport errors (timeouts, resets) keep their real message so we
		// don't mislabel a slow-but-running daemon as "not running".
		if isConnRefused(err) {
			return ErrDaemonDown
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error
		if msg == "" {
			msg = resp.Status
		}
		return &StatusError{Code: resp.StatusCode, Msg: msg, Body: raw}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

func (c *Client) List(ctx context.Context) ([]*store.Session, error) {
	var resp struct {
		Sessions []*store.Session `json:"sessions"`
	}
	if err := c.do(ctx, http.MethodGet, "/sessions", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

func (c *Client) Get(ctx context.Context, id string) (*store.Session, error) {
	var s store.Session
	if err := c.do(ctx, http.MethodGet, "/sessions/"+id, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SpawnParams mirrors the daemon's /spawn body (kept in the client package so
// the CLI and MCP server don't import the daemon package).
type SpawnParams struct {
	Type           string
	Ticket         string
	Name           string
	Repo           string
	Branch         string
	PR             string
	Worktree       bool
	Prompt         string
	Cwd            string
	PermissionMode string
	AutoRestart    bool
	Force          bool
	Model          string
}

func (c *Client) Spawn(ctx context.Context, p SpawnParams) (*store.Session, error) {
	var s store.Session
	body := map[string]any{
		"type": p.Type, "ticket": p.Ticket, "name": p.Name, "repo": p.Repo,
		"branch": p.Branch, "pr": p.PR, "worktree": p.Worktree,
		"prompt": p.Prompt, "cwd": p.Cwd, "permission_mode": p.PermissionMode,
		"auto_restart": p.AutoRestart, "force": p.Force,
		"model": p.Model,
	}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/spawn", body, &s); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusPreconditionRequired {
			var cr struct {
				ConfirmationRequired bool             `json:"confirmation_required"`
				Verdict              pressure.Verdict `json:"verdict"`
			}
			if json.Unmarshal(se.Body, &cr) == nil && cr.ConfirmationRequired {
				return nil, &ErrConfirmationRequired{Verdict: cr.Verdict}
			}
		}
		return nil, err
	}
	return &s, nil
}

// AdoptParams mirrors the daemon's /adopt body.
type AdoptParams struct {
	Cwd         string
	SessionID   string
	TmuxSession string
}

// AdoptResult is the /adopt response: the new session plus an optional warning.
type AdoptResult struct {
	Session *store.Session
	Warning string
}

func (c *Client) Adopt(ctx context.Context, p AdoptParams) (*AdoptResult, error) {
	var resp struct {
		Session *store.Session `json:"session"`
		Warning string         `json:"warning"`
	}
	body := map[string]any{
		"cwd": p.Cwd, "session_id": p.SessionID, "tmux_session": p.TmuxSession,
	}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/adopt", body, &resp); err != nil {
		return nil, err
	}
	return &AdoptResult{Session: resp.Session, Warning: resp.Warning}, nil
}

func (c *Client) Terminate(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/terminate", nil, nil)
}

func (c *Client) Delete(ctx context.Context, id string, hard bool) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/delete", map[string]bool{"hard": hard}, nil)
}

func (c *Client) RemoveWorktree(ctx context.Context, id string, force, deleteAdoptedBranch bool) error {
	body := map[string]bool{"force": force, "delete_adopted_branch": deleteAdoptedBranch}
	return c.doT(ctx, longTimeout, http.MethodPost, "/sessions/"+id+"/remove-worktree", body, nil)
}

// ListWorktrees returns the read-only join behind `warden worktree ls` for repo.
func (c *Client) ListWorktrees(ctx context.Context, repo string) ([]lifecycle.WorktreeListing, error) {
	var resp struct {
		Worktrees []lifecycle.WorktreeListing `json:"worktrees"`
	}
	q := "/worktrees?repo=" + url.QueryEscape(repo)
	if err := c.do(ctx, http.MethodGet, q, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Worktrees, nil
}

// PruneParams mirrors the daemon's /prune body.
type PruneParams struct {
	Repo            string
	DryRun          bool
	Force           bool
	IncludeArchived bool
}

// Prune reconciles repo's .worktrees against warden's records and returns the
// per-worktree results (a dirty/unpushed skip is a result entry, not an error).
func (c *Client) Prune(ctx context.Context, p PruneParams) ([]lifecycle.PruneResult, error) {
	body := map[string]any{
		"repo": p.Repo, "dry_run": p.DryRun, "force": p.Force, "include_archived": p.IncludeArchived,
	}
	var resp struct {
		Results []lifecycle.PruneResult `json:"results"`
	}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/prune", body, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

func (c *Client) Input(ctx context.Context, id, text string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/input", map[string]string{"text": text}, nil)
}

func (c *Client) Restore(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/restore", nil, nil)
}

// Approvals fetches the live approval queue. Returns (enabled, views, err);
// enabled is false when the daemon has the feature toggled off.
func (c *Client) Approvals(ctx context.Context) (bool, []approval.View, error) {
	var resp struct {
		Enabled   bool            `json:"enabled"`
		Approvals []approval.View `json:"approvals"`
	}
	if err := c.do(ctx, http.MethodGet, "/approvals", nil, &resp); err != nil {
		return false, nil, err
	}
	return resp.Enabled, resp.Approvals, nil
}

// Approve answers a recognized prompt with the 1-based option and the options
// fingerprint the UI rendered (for the daemon's re-verify guard).
func (c *Client) Approve(ctx context.Context, id string, option int, fingerprint string) error {
	body := map[string]any{"option": option, "fingerprint": fingerprint}
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/approve", body, nil)
}

// Digest fetches an agent's completion digest. Uses longTimeout because the
// daemon's narrator (claude -p) dominates latency.
func (c *Client) Digest(ctx context.Context, id string) (*digest.Digest, error) {
	var d digest.Digest
	if err := c.doT(ctx, longTimeout, http.MethodGet, "/sessions/"+id+"/digest", nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// PressureStatus mirrors GET /pressure.
type PressureStatus struct {
	Level       int    `json:"level"`
	LevelName   string `json:"level_name"`
	AgentCount  int    `json:"agent_count"`
	MaxAgents   int    `json:"max_agents"`
	Elevated    bool   `json:"elevated"`
	GateEnabled bool   `json:"gate_enabled"`
}

func (c *Client) Pressure(ctx context.Context) (PressureStatus, error) {
	var p PressureStatus
	err := c.do(ctx, http.MethodGet, "/pressure", nil, &p)
	return p, err
}

// DirEntry is one subdirectory in a DirListing.
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// DirListing mirrors the daemon's GET /fs/dirs response: a directory, its
// parent (empty at the filesystem root), and its immediate subdirectories.
type DirListing struct {
	Path    string     `json:"path"`
	Parent  string     `json:"parent"`
	Entries []DirEntry `json:"entries"`
}

// ListDirs lists the immediate subdirectories of path (empty path = the
// daemon's default, the user's home directory).
func (c *Client) ListDirs(ctx context.Context, path string) (DirListing, error) {
	p := "/fs/dirs"
	if path != "" {
		p += "?path=" + url.QueryEscape(path)
	}
	var l DirListing
	if err := c.do(ctx, http.MethodGet, p, nil, &l); err != nil {
		return DirListing{}, err
	}
	return l, nil
}

func (c *Client) Output(ctx context.Context, id string, lines int) (string, error) {
	var resp struct {
		Output string `json:"output"`
	}
	path := fmt.Sprintf("/sessions/%s/output?lines=%d", id, lines)
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return "", err
	}
	return resp.Output, nil
}

// ContextEntry mirrors the daemon's shared-context entry (GET/PUT /context).
type ContextEntry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ctxKeyValid rejects keys that can't survive a URL path segment round-trip.
// Keys are dot-namespaced; a "/" or "\" gets percent-escaped on the way out and
// is NOT decoded back by the router, so it would be stored under a corrupted key
// (e.g. "a/b" -> "a%2Fb") and break prefix operations. Reject before escaping —
// this is the common chokepoint for the CLI and MCP paths.
func ctxKeyValid(key string) error {
	if key == "" || strings.ContainsAny(key, `/\`) {
		return fmt.Errorf("invalid context key %q: must be non-empty and contain no '/' or '\\'", key)
	}
	return nil
}

// CtxSet writes value at key, attributing the write to `by`.
func (c *Client) CtxSet(ctx context.Context, key, value, by string) (ContextEntry, error) {
	if err := ctxKeyValid(key); err != nil {
		return ContextEntry{}, err
	}
	var e ContextEntry
	body := map[string]string{"value": value, "by": by}
	err := c.do(ctx, http.MethodPut, "/context/"+url.PathEscape(key), body, &e)
	return e, err
}

// ErrCASConflict is returned by CtxCAS when the daemon reports (HTTP 409) that
// the current value did not match the expected value — re-read and retry.
var ErrCASConflict = errors.New("context value conflict")

// CtxCAS writes value at key only if the current value equals expected
// (expected "" means "the key must be absent"), attributing the write to `by`.
// Returns ErrCASConflict on mismatch.
func (c *Client) CtxCAS(ctx context.Context, key, expected, value, by string) (ContextEntry, error) {
	if err := ctxKeyValid(key); err != nil {
		return ContextEntry{}, err
	}
	var e ContextEntry
	body := map[string]string{"expected": expected, "value": value, "by": by}
	err := c.do(ctx, http.MethodPost, "/context/"+url.PathEscape(key)+"/cas", body, &e)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusConflict {
		return ContextEntry{}, ErrCASConflict
	}
	return e, err
}

// CtxAppend atomically appends sep+value to key's current value, creating the
// key (with no leading sep) when absent, attributing the write to `by`.
func (c *Client) CtxAppend(ctx context.Context, key, value, sep, by string) (ContextEntry, error) {
	if err := ctxKeyValid(key); err != nil {
		return ContextEntry{}, err
	}
	var e ContextEntry
	body := map[string]string{"value": value, "sep": sep, "by": by}
	err := c.do(ctx, http.MethodPost, "/context/"+url.PathEscape(key)+"/append", body, &e)
	return e, err
}

// CtxGet reads the entry at key (StatusError 404 if absent).
func (c *Client) CtxGet(ctx context.Context, key string) (ContextEntry, error) {
	if err := ctxKeyValid(key); err != nil {
		return ContextEntry{}, err
	}
	var e ContextEntry
	err := c.do(ctx, http.MethodGet, "/context/"+url.PathEscape(key), nil, &e)
	return e, err
}

// CtxList lists entries under prefix (empty = all).
func (c *Client) CtxList(ctx context.Context, prefix string) ([]ContextEntry, error) {
	p := "/context"
	if prefix != "" {
		p += "?prefix=" + url.QueryEscape(prefix)
	}
	var resp struct {
		Entries []ContextEntry `json:"entries"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

// CtxDel deletes key.
func (c *Client) CtxDel(ctx context.Context, key string) error {
	if err := ctxKeyValid(key); err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/context/"+url.PathEscape(key), nil, nil)
}

// Message mirrors the daemon's mailbox message (directed messages).
type Message struct {
	ID   string    `json:"id"`
	From string    `json:"from"`
	To   string    `json:"to"`
	Body string    `json:"body"`
	TS   time.Time `json:"ts"`
	Read bool      `json:"read"`
}

// MsgSend delivers body to recipient `to` from `from`; returns the stored
// message and whether the recipient was woken.
func (c *Client) MsgSend(ctx context.Context, to, from, body string) (Message, bool, error) {
	var resp struct {
		Message Message `json:"message"`
		Woke    bool    `json:"woke"`
	}
	reqBody := map[string]string{"from": from, "body": body}
	if err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(to)+"/messages", reqBody, &resp); err != nil {
		return Message{}, false, err
	}
	return resp.Message, resp.Woke, nil
}

// MsgInbox returns id's messages (unreadOnly filters to unread); the daemon
// marks the returned messages read.
func (c *Client) MsgInbox(ctx context.Context, id string, unreadOnly bool) ([]Message, error) {
	p := "/sessions/" + url.PathEscape(id) + "/messages"
	if unreadOnly {
		p += "?unread=true"
	}
	var resp struct {
		Messages []Message `json:"messages"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// MsgRecent returns recent message traffic across ALL agent inboxes, newest
// first (limit <= 0 lets the daemon pick its default). This is the read-only
// global view behind the inspector — it never marks anything read.
func (c *Client) MsgRecent(ctx context.Context, limit int) ([]Message, error) {
	p := "/messages"
	if limit > 0 {
		p += "?limit=" + strconv.Itoa(limit)
	}
	var resp struct {
		Messages []Message `json:"messages"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// MsgWait blocks (server long-poll) until a message for id arrives (optionally
// filtered by sender `from`) or timeoutSec elapses. Returns nil on timeout. The
// HTTP deadline is set beyond the server window so the client never cuts the
// long-poll short.
func (c *Client) MsgWait(ctx context.Context, id, from string, timeoutSec int) (*Message, error) {
	p := fmt.Sprintf("/sessions/%s/messages/wait?timeout=%d", url.PathEscape(id), timeoutSec)
	if from != "" {
		p += "&from=" + url.QueryEscape(from)
	}
	var resp struct {
		Found   bool     `json:"found"`
		Message *Message `json:"message"`
	}
	if err := c.doT(ctx, time.Duration(timeoutSec+10)*time.Second, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	if !resp.Found {
		return nil, nil
	}
	return resp.Message, nil
}

// PipelineCreate sends a YAML spec to the daemon, which parses, validates, and
// stores it.
func (c *Client) PipelineCreate(ctx context.Context, specYAML string) (*pipeline.Pipeline, error) {
	var p pipeline.Pipeline
	if err := c.do(ctx, http.MethodPost, "/pipelines", map[string]string{"spec": specYAML}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) PipelineList(ctx context.Context) ([]*pipeline.Pipeline, error) {
	var resp struct {
		Pipelines []*pipeline.Pipeline `json:"pipelines"`
	}
	if err := c.do(ctx, http.MethodGet, "/pipelines", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Pipelines, nil
}

func (c *Client) PipelineGet(ctx context.Context, id string) (*pipeline.Pipeline, error) {
	var p pipeline.Pipeline
	if err := c.do(ctx, http.MethodGet, "/pipelines/"+url.PathEscape(id), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) PipelineStart(ctx context.Context, id string) error {
	// longTimeout: starting reconciles synchronously and may spawn worktree jobs
	// (git worktree add), which can outlast the short read deadline.
	return c.doT(ctx, longTimeout, http.MethodPost, "/pipelines/"+url.PathEscape(id)+"/start", nil, nil)
}

func (c *Client) PipelineCancel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/pipelines/"+url.PathEscape(id)+"/cancel", nil, nil)
}

func (c *Client) PipelineDelete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/pipelines/"+url.PathEscape(id), nil, nil)
}

func (c *Client) PipelineEmit(ctx context.Context, pid, job, text string) error {
	path := "/pipelines/" + url.PathEscape(pid) + "/jobs/" + url.PathEscape(job) + "/emit"
	// longTimeout: emit reconciles and may spawn dependent worktree jobs.
	return c.doT(ctx, longTimeout, http.MethodPost, path, map[string]string{"text": text}, nil)
}

// PipelineEditJob updates a pending job's prompt and/or handoff (nil = unchanged).
func (c *Client) PipelineEditJob(ctx context.Context, pid, job string, prompt, handoff *string) error {
	body := map[string]*string{}
	if prompt != nil {
		body["prompt"] = prompt
	}
	if handoff != nil {
		body["handoff"] = handoff
	}
	path := "/pipelines/" + url.PathEscape(pid) + "/jobs/" + url.PathEscape(job) + "/edit"
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// PipelineRetry re-runs a failed/needs-attention job.
func (c *Client) PipelineRetry(ctx context.Context, pid, job string) error {
	path := "/pipelines/" + url.PathEscape(pid) + "/jobs/" + url.PathEscape(job) + "/retry"
	// longTimeout: retry reconciles and may spawn a worktree job.
	return c.doT(ctx, longTimeout, http.MethodPost, path, nil, nil)
}

// GetMetrics fetches the live resource snapshot (GET /metrics).
func (c *Client) GetMetrics(ctx context.Context) (*metrics.Sample, error) {
	var s metrics.Sample
	if err := c.do(ctx, http.MethodGet, "/metrics", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetMetricsHistory fetches recorded samples (GET /metrics/history). since is an
// RFC3339 timestamp ("" lets the daemon default to its look-back window); limit
// <= 0 lets the daemon pick its cap.
func (c *Client) GetMetricsHistory(ctx context.Context, since string, limit int) ([]metrics.Sample, error) {
	p := "/metrics/history"
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if e := q.Encode(); e != "" {
		p += "?" + e
	}
	var resp struct {
		Samples []metrics.Sample `json:"samples"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Samples, nil
}

func (c *Client) SetAutoApprove(ctx context.Context, id string, enabled bool) error {
	body := map[string]bool{"enabled": enabled}
	return c.do(ctx, http.MethodPatch, "/sessions/"+id+"/auto-approve", body, nil)
}

func (c *Client) SetPermissionMode(ctx context.Context, id string, mode string) error {
	body := map[string]string{"permission_mode": mode}
	return c.do(ctx, http.MethodPatch, "/sessions/"+id+"/permission-mode", body, nil)
}
