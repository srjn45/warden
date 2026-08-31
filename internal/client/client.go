package client

import (
	"bufio"
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
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/spend"
	"github.com/srjn45/warden/internal/store"
)

// apiPrefix is the versioned base path every data/action route hangs off of on
// the daemon (mirrors r.Route("/api/v1") in internal/daemon). It is prepended to
// every request path here, so handler-relative paths elsewhere stay unprefixed.
const apiPrefix = "/api/v1"

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
// no deadline of its own. Reads are quick; spawn/adopt/remove-worktree/check run
// synchronously on the daemon (git worktree add, running the test suite,
// transcript scan) and can take far longer than a read — a single blanket client
// timeout would abort a slow-but-successful spawn while the daemon kept working,
// orphaning sessions. longTimeout is kept ABOVE the daemon's slow-route write
// budget (daemon.slowWriteTimeoutDur, 10m) so the daemon stays authoritative: it
// returns success or its own clean (cleanup-aware) timeout before the client
// gives up. In a very large monorepo even a single `git worktree add` (a full
// working-tree checkout) can run for minutes, so this is deliberately generous.
// Vars (not consts) so tests can shrink them.
var (
	defaultTimeout = 30 * time.Second
	longTimeout    = 12 * time.Minute
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
	req, err := http.NewRequestWithContext(ctx, method, c.base+apiPrefix+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// Identify the calling agent (when this CLI/MCP runs inside one) so the daemon
	// can enforce autopilot's request-scoped ownership guard (autopilot.md §8). A
	// human shell / the web UI sets no session env, so the header is simply absent
	// and the guard no-ops.
	if actor := auth.ActorFromEnv(); actor != "" {
		req.Header.Set(auth.ActorHeader, actor)
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
	return c.list(ctx, false)
}

// ListAll includes daemon-owned system sessions hidden from the ordinary fleet.
func (c *Client) ListAll(ctx context.Context) ([]*store.Session, error) {
	return c.list(ctx, true)
}

func (c *Client) list(ctx context.Context, all bool) ([]*store.Session, error) {
	var resp struct {
		Sessions []*store.Session `json:"sessions"`
	}
	path := "/sessions"
	if all {
		path += "?all=true"
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// WatchAll is Watch with system sessions included.
func (c *Client) WatchAll(ctx context.Context, onSnapshot func([]*store.Session) error) error {
	return c.watch(ctx, true, onSnapshot)
}

// SearchParams mirrors the daemon's GET /search query.
type SearchParams struct {
	Query  string // required; whitespace-separated terms (AND)
	Closed bool   // also search the archived (closed/) store
}

// Search runs an in-memory full-text search across sessions (subject, prompt,
// type, name, pane, id, ticket, branch). With Closed=true it also searches the
// archived store. The daemon rejects a blank query with a 400.
func (c *Client) Search(ctx context.Context, p SearchParams) ([]*store.Session, error) {
	q := url.Values{}
	q.Set("q", p.Query)
	if p.Closed {
		q.Set("closed", "true")
	}
	var resp struct {
		Sessions []*store.Session `json:"sessions"`
	}
	if err := c.do(ctx, http.MethodGet, "/search?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// HistoryParams mirrors the daemon's GET /history query.
type HistoryParams struct {
	Since time.Time // zero = no lower bound
	Type  string    // "" = any task type
	Limit int       // <=0 = no cap
}

// History browses the archived (closed/) store, newest-first, narrowed by the
// optional since/type/limit filters.
func (c *Client) History(ctx context.Context, p HistoryParams) ([]*store.Session, error) {
	q := url.Values{}
	if !p.Since.IsZero() {
		q.Set("since", p.Since.UTC().Format(time.RFC3339))
	}
	if p.Type != "" {
		q.Set("type", p.Type)
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	path := "/history"
	if e := q.Encode(); e != "" {
		path += "?" + e
	}
	var resp struct {
		Sessions []*store.Session `json:"sessions"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// Import POSTs an export envelope to the daemon, which inserts its Session
// records into the active store (metadata only — worktrees are not recreated).
// With merge=true an existing record is overwritten on id collision; otherwise
// it is skipped. The returned result lists the ids in each bucket.
func (c *Client) Import(ctx context.Context, env *store.Export, merge bool) (*store.ImportResult, error) {
	path := "/import"
	if merge {
		path += "?merge=true"
	}
	var res store.ImportResult
	if err := c.do(ctx, http.MethodPost, path, env, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Watch opens the daemon's SSE session stream (GET /events/stream) and invokes
// onSnapshot once for the initial snapshot and again for every state change the
// daemon pushes. It blocks until ctx is cancelled, the connection drops, or
// onSnapshot returns an error (which it returns). A ctx cancellation surfaces as
// ctx.Err(); callers that cancel deliberately (e.g. on Ctrl+C) should treat
// context.Canceled as a clean stop.
func (c *Client) Watch(ctx context.Context, onSnapshot func([]*store.Session) error) error {
	return c.watch(ctx, false, onSnapshot)
}

func (c *Client) watch(ctx context.Context, all bool, onSnapshot func([]*store.Session) error) error {
	// No per-call deadline: this is a long-lived stream, not a request/response.
	path := "/events/stream"
	if all {
		path += "?all=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+apiPrefix+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
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

	// Parse the SSE stream: accumulate "data:" lines until a blank line ends the
	// event, then decode the joined payload as a sessions snapshot. Comment lines
	// (": ping" heartbeats) are ignored.
	sc := bufio.NewScanner(resp.Body)
	// Snapshots carry every session's full event log, so a default 64KB token can
	// overflow with many busy agents — give the scanner room to grow.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data []byte
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			if len(data) == 0 {
				continue
			}
			var r struct {
				Sessions []*store.Session `json:"sessions"`
			}
			if err := json.Unmarshal(data, &r); err == nil {
				if err := onSnapshot(r.Sessions); err != nil {
					return err
				}
			}
			data = data[:0]
		case bytes.HasPrefix(line, []byte("data:")):
			v := bytes.TrimPrefix(line, []byte("data:"))
			v = bytes.TrimPrefix(v, []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, v...)
		}
	}
	if err := sc.Err(); err != nil {
		// A deliberate cancellation reads back as a transport error; report the
		// cancellation instead so callers can recognize the clean-stop path.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return ctx.Err()
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
	InRepo         bool
	Prompt         string
	Cwd            string
	PermissionMode string
	AutoRestart    bool
	Force          bool
	Model          string
	Backend        string
	Kind           string // "" / "agent" ⇒ AI agent; "terminal" ⇒ plain ${SHELL:-bash} pane (backend/model/role/prompt ignored)
	Tags           []string
	ParentID       string
	ForkFrom       string // id of an existing agent whose recorded session to FORK (codex fork); empty = normal spawn
	Role           string // built-in role name; empty = general (no persona). Persona injected + role defaults fill unset fields.
	Tier           string // explicit model tier ("tier-1"/"tier-2"/"tier-3") for the quota-balanced resolver; empty = derive from task/role
	Task           string // task name (task registry) for tier routing via task.TierFor; empty = none
}

func (c *Client) Spawn(ctx context.Context, p SpawnParams) (*store.Session, error) {
	var s store.Session
	body := map[string]any{
		"type": p.Type, "ticket": p.Ticket, "name": p.Name, "repo": p.Repo,
		"branch": p.Branch, "pr": p.PR, "worktree": p.Worktree, "in_repo": p.InRepo,
		"prompt": p.Prompt, "cwd": p.Cwd, "permission_mode": p.PermissionMode,
		"auto_restart": p.AutoRestart, "force": p.Force,
		"model": p.Model, "backend": p.Backend, "kind": p.Kind, "tags": p.Tags, "parent_id": p.ParentID,
		"fork_from": p.ForkFrom, "role": p.Role, "tier": p.Tier, "task": p.Task,
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

// GuardVerdict is the daemon's isolation decision for one file-mutating tool call.
type GuardVerdict struct {
	Decision string `json:"decision"` // "allow" | "deny"
	Reason   string `json:"reason"`
}

// Guard asks the daemon whether an agent may Edit/Write the given path. It is
// called by `warden hook guard` from a PreToolUse hook, so the caller passes a
// short-deadline context — a slow/absent daemon must not stall the agent's edit.
func (c *Client) Guard(ctx context.Context, session, tool, path string) (GuardVerdict, error) {
	var v GuardVerdict
	body := map[string]string{"session": session, "tool": tool, "path": path}
	if err := c.do(ctx, http.MethodPost, "/hooks/guard", body, &v); err != nil {
		return GuardVerdict{}, err
	}
	return v, nil
}

// GitCommit stages+commits dir's changes via the daemon's rail-enforcing commit
// (protected-branch refusal, pre-commit hook parsing, bookkeeping). session is
// the calling agent's id ("" for a human run); when set the daemon pins the
// action to that agent's own worktree. Uses longTimeout — commit runs the
// repo's own pre-commit hooks, which in a large monorepo can take minutes.
func (c *Client) GitCommit(ctx context.Context, session, dir, message string) (lifecycle.CommitResult, error) {
	var res lifecycle.CommitResult
	body := map[string]string{"session": session, "dir": dir, "message": message}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/git/commit", body, &res); err != nil {
		return lifecycle.CommitResult{}, err
	}
	return res, nil
}

// GitPush pushes dir's current branch to origin (protected-branch refusal).
// force sends git push --force-with-lease. Uses longTimeout — push is a network
// round-trip.
func (c *Client) GitPush(ctx context.Context, session, dir string, force bool) (lifecycle.PushResult, error) {
	var res lifecycle.PushResult
	body := map[string]any{"session": session, "dir": dir, "force": force}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/git/push", body, &res); err != nil {
		return lifecycle.PushResult{}, err
	}
	return res, nil
}

// GitSync fetches origin/base and rebases dir's branch onto it, returning the
// conflicting paths when the rebase is left in progress. Uses longTimeout —
// fetch is a network round-trip.
func (c *Client) GitSync(ctx context.Context, session, dir, base string) (lifecycle.SyncResult, error) {
	var res lifecycle.SyncResult
	body := map[string]string{"session": session, "dir": dir, "base": base}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/git/sync", body, &res); err != nil {
		return lifecycle.SyncResult{}, err
	}
	return res, nil
}

// Check runs the project's configured check command(s) in dir via the daemon and
// returns a pass/fail summary with output only for the failures. name selects a
// configured entry ("" runs all). Uses longTimeout — a check runs a test/build
// command that can take minutes. When set, session pins the run to the agent's
// own worktree.
func (c *Client) Check(ctx context.Context, session, dir, name string) (lifecycle.CheckResult, error) {
	var res lifecycle.CheckResult
	body := map[string]string{"session": session, "dir": dir, "name": name}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/check", body, &res); err != nil {
		return lifecycle.CheckResult{}, err
	}
	return res, nil
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

// RecoverResult is one archived record's recovery candidacy/outcome, mirroring
// the daemon's /recover response.
type RecoverResult struct {
	ID          string `json:"id"`
	TmuxSession string `json:"tmux_session"`
	Workdir     string `json:"workdir"`
	Name        string `json:"name,omitempty"`
	Subject     string `json:"subject,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
	Recovered   bool   `json:"recovered"`
	Error       string `json:"error,omitempty"`
}

// Recover scans archived records for ones whose tmux session is still alive.
// apply=false (the default) only reports candidates; apply=true re-inserts
// each one into the active store under its original id.
func (c *Client) Recover(ctx context.Context, apply bool) ([]RecoverResult, error) {
	body := map[string]any{"apply": apply}
	var resp struct {
		Results []RecoverResult `json:"results"`
	}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/recover", body, &resp); err != nil {
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

// SnapshotCreate captures the worktree state + transcript of an agent at a
// known-good point (#46). session pins the daemon to the agent's own worktree
// (and supplies the tmux pane for the transcript); dir is the human fallback.
// Uses longTimeout — capture shells out to git + tmux.
func (c *Client) SnapshotCreate(ctx context.Context, session, dir, message string) (*snapshot.Snapshot, error) {
	var snap snapshot.Snapshot
	body := map[string]string{"session": session, "dir": dir, "message": message}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/snapshots", body, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// SnapshotList returns snapshots newest-first, optionally filtered to one session.
func (c *Client) SnapshotList(ctx context.Context, session string) ([]*snapshot.Snapshot, error) {
	var resp struct {
		Snapshots []*snapshot.Snapshot `json:"snapshots"`
	}
	q := "/snapshots"
	if session != "" {
		q += "?session=" + url.QueryEscape(session)
	}
	if err := c.do(ctx, http.MethodGet, q, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Snapshots, nil
}

// SnapshotRestore re-applies a snapshot onto its recorded worktree, refusing a
// dirty tree unless force is set. Uses longTimeout — restore shells out to git.
func (c *Client) SnapshotRestore(ctx context.Context, id string, force bool) (*snapshot.RestoreResult, error) {
	var res snapshot.RestoreResult
	body := map[string]bool{"force": force}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/snapshots/"+id+"/restore", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Savings fetches the aggregated token-savings summary. A non-zero since limits
// the window; the zero time requests all-time. bucket ("day"|"hour"; "" for none)
// additionally requests the zero-filled saved-tokens trend (Summary.Buckets);
// samples requests the retained provenance pairs (Summary.Samples, for the audit
// view). The daemon returns 403 (a StatusError with Code 403) when the savings
// ledger is disabled, which the CLI turns into a friendly enable hint.
func (c *Client) Savings(ctx context.Context, since time.Time, bucket string, samples bool) (*savings.Summary, error) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if samples {
		q.Set("samples", "1")
	}
	path := "/savings"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var sum savings.Summary
	if err := c.do(ctx, http.MethodGet, path, nil, &sum); err != nil {
		return nil, err
	}
	return &sum, nil
}

// Spend fetches the cost rollup: the measured Claude spend priced per model and
// aggregated per-agent / per-repo / per-day, plus the daily/weekly totals the
// budget gate enforces. The daemon returns 403 (a StatusError with Code 403) when
// spend tracking is disabled, which the CLI turns into a friendly enable hint.
func (c *Client) Spend(ctx context.Context) (*spend.Report, error) {
	var rep spend.Report
	if err := c.do(ctx, http.MethodGet, "/spend", nil, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
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

// CreatePR opens a GitHub pull request for an agent's branch via the daemon
// (push → digest → gh pr create). base selects the PR base ("" = main). Uses
// longTimeout — it pushes and shells gh over the network. An already-existing PR
// comes back as a successful result with Created=false.
func (c *Client) CreatePR(ctx context.Context, id, base string) (lifecycle.PRResult, error) {
	var res lifecycle.PRResult
	body := map[string]string{"base": base}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/sessions/"+id+"/create-pr", body, &res); err != nil {
		return lifecycle.PRResult{}, err
	}
	return res, nil
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

// CloneRepo clones url into the daemon's configured workspace directory (the
// `workspace_path` config setting) and returns the resulting local directory.
// Backs the TUI's "Open remote project" flow. Uses longTimeout — clone is a
// network round-trip that can take a while for a large repo.
func (c *Client) CloneRepo(ctx context.Context, remoteURL string) (string, error) {
	var resp struct {
		Dir string `json:"dir"`
	}
	body := map[string]string{"url": remoteURL}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/fs/clone", body, &resp); err != nil {
		return "", err
	}
	return resp.Dir, nil
}

// OpenLocalProject registers an existing local directory as a project via
// POST /projects/local. The daemon normalizes the path, verifies it exists, and
// persists + opens the project (restoring hibernated agents). Returns the project.
func (c *Client) OpenLocalProject(ctx context.Context, path, name string) (projectstore.Project, error) {
	var p projectstore.Project
	body := map[string]string{"path": path, "name": name}
	if err := c.do(ctx, http.MethodPost, "/projects/local", body, &p); err != nil {
		return projectstore.Project{}, err
	}
	return p, nil
}

// OpenRemoteProject clones a remote URL into the daemon's workspace and registers
// the result as a project via POST /projects/remote. Uses longTimeout — clone is a
// network round-trip that can take a while for a large repo.
func (c *Client) OpenRemoteProject(ctx context.Context, remoteURL, name string) (projectstore.Project, error) {
	var p projectstore.Project
	body := map[string]string{"url": remoteURL, "name": name}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/projects/remote", body, &p); err != nil {
		return projectstore.Project{}, err
	}
	return p, nil
}

// CreateProject scaffolds a brand-new project (git init + README + initial commit)
// in the daemon's workspace and registers it via POST /projects/new. Uses
// longTimeout — git init + commit can be slow on some filesystems.
func (c *Client) CreateProject(ctx context.Context, name string) (projectstore.Project, error) {
	var p projectstore.Project
	body := map[string]string{"name": name}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/projects/new", body, &p); err != nil {
		return projectstore.Project{}, err
	}
	return p, nil
}

// ListProjects returns every persisted project (open and closed), sorted by
// display name. Read-only; backs the TUI's project-grouped navigator (Phase 4).
func (c *Client) ListProjects(ctx context.Context) ([]projectstore.Project, error) {
	var resp struct {
		Projects []projectstore.Project `json:"projects"`
	}
	if err := c.do(ctx, http.MethodGet, "/projects", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

// ListProjectGroups returns every project group (Project Groups feature, Phase 1),
// sorted by display name. Read-only; backs the TUI's per-project group label.
func (c *Client) ListProjectGroups(ctx context.Context) ([]projectstore.ProjectGroup, error) {
	var resp struct {
		Groups []projectstore.ProjectGroup `json:"groups"`
	}
	if err := c.do(ctx, http.MethodGet, "/project-groups", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Groups, nil
}

// CloseProject hibernates a project (IDE-like): the record is kept, its status
// flips to closed, and the daemon gracefully terminates its live agents (restored
// on reopen). The id is a filesystem path / remote URL, so it is percent-encoded
// into the path segment (the daemon decodes it). Returns the updated project.
func (c *Client) CloseProject(ctx context.Context, id string) (projectstore.Project, error) {
	var p projectstore.Project
	path := "/projects/" + url.PathEscape(id) + "/close"
	if err := c.do(ctx, http.MethodPost, path, nil, &p); err != nil {
		return projectstore.Project{}, err
	}
	return p, nil
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

// ConflictAgent identifies an agent editing a file (collab conflict view).
type ConflictAgent struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Conflict is one file edited by two or more agents.
type Conflict struct {
	File   string          `json:"file"`
	Agents []ConflictAgent `json:"agents"`
}

// CollabConflicts returns the current inter-agent file conflicts.
func (c *Client) CollabConflicts(ctx context.Context) ([]Conflict, error) {
	var resp struct {
		Conflicts []Conflict `json:"conflicts"`
	}
	if err := c.do(ctx, http.MethodGet, "/collab/conflicts", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Conflicts, nil
}

// CIStatus is the latest CI run observed for an agent's branch.
type CIStatus struct {
	State    string `json:"state"` // success | failure | pending | none
	Workflow string `json:"workflow,omitempty"`
	URL      string `json:"url,omitempty"`
}

// BranchStatus is one agent's branch+CI snapshot (branch-tracker view).
type BranchStatus struct {
	AgentID string   `json:"agent_id"`
	Name    string   `json:"name,omitempty"`
	Branch  string   `json:"branch"`
	CI      CIStatus `json:"ci"`
	Behind  int      `json:"behind"`
	Ahead   int      `json:"ahead"`
	Merged  bool     `json:"merged"`
}

// BranchStatuses returns each tracked agent's CI + branch-vs-main status. An
// empty list means no tracked branches (or the tracker is disabled).
func (c *Client) BranchStatuses(ctx context.Context) ([]BranchStatus, error) {
	var resp struct {
		Branches []BranchStatus `json:"branches"`
	}
	if err := c.do(ctx, http.MethodGet, "/collab/branches", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Branches, nil
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

// PipelinePause halts DAG progress: in-flight jobs keep running but no new job
// spawns until PipelineResume.
func (c *Client) PipelinePause(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/pipelines/"+url.PathEscape(id)+"/pause", nil, nil)
}

// PipelineResume lifts a pause and reconciles.
func (c *Client) PipelineResume(ctx context.Context, id string) error {
	// longTimeout: resume reconciles synchronously and may spawn worktree jobs.
	return c.doT(ctx, longTimeout, http.MethodPost, "/pipelines/"+url.PathEscape(id)+"/resume", nil, nil)
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

// ScheduleCreate registers a recurring (cron) or single-shot (at) schedule that
// fires an agent spawn or a pipeline. The daemon validates the timing spec and
// (for pipeline mode) the YAML payload, and returns the stored schedule.
func (c *Client) ScheduleCreate(ctx context.Context, req ScheduleCreateRequest) (*schedule.Schedule, error) {
	var sc schedule.Schedule
	if err := c.do(ctx, http.MethodPost, "/schedules", req, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// ScheduleCreateRequest is the JSON body for POST /schedules.
type ScheduleCreateRequest struct {
	Name   string `json:"name"`
	Cron   string `json:"cron,omitempty"`
	At     string `json:"at,omitempty"`
	Type   string `json:"type,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Branch string `json:"branch,omitempty"`
	Spec   string `json:"spec,omitempty"`
}

func (c *Client) ScheduleList(ctx context.Context) ([]*schedule.Schedule, error) {
	var resp struct {
		Schedules []*schedule.Schedule `json:"schedules"`
	}
	if err := c.do(ctx, http.MethodGet, "/schedules", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Schedules, nil
}

func (c *Client) ScheduleDelete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/schedules/"+url.PathEscape(id), nil, nil)
}

// ScheduleGet fetches one schedule by id.
func (c *Client) ScheduleGet(ctx context.Context, id string) (*schedule.Schedule, error) {
	var sc schedule.Schedule
	if err := c.do(ctx, http.MethodGet, "/schedules/"+url.PathEscape(id), nil, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// ScheduleEnable re-arms a schedule and returns the updated record.
func (c *Client) ScheduleEnable(ctx context.Context, id string) (*schedule.Schedule, error) {
	var sc schedule.Schedule
	if err := c.do(ctx, http.MethodPost, "/schedules/"+url.PathEscape(id)+"/enable", nil, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// ScheduleDisable stops a schedule from firing and returns the updated record.
func (c *Client) ScheduleDisable(ctx context.Context, id string) (*schedule.Schedule, error) {
	var sc schedule.Schedule
	if err := c.do(ctx, http.MethodPost, "/schedules/"+url.PathEscape(id)+"/disable", nil, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// AutopilotStatus mirrors the daemon's GET /autopilot response (autopilot.md §5).
type AutopilotStatus struct {
	Enabled      bool                 `json:"enabled"`
	EnabledRepos []string             `json:"enabled_repos"`
	Runs         []AutopilotRunStatus `json:"runs"`
}

// AutopilotRunStatus is one run's slice of AutopilotStatus.
type AutopilotRunStatus struct {
	RunID           string              `json:"run_id"`
	Name            string              `json:"name"`
	PlanFile        string              `json:"plan_file"`
	Repo            string              `json:"repo"`
	State           string              `json:"state"`
	Gate            string              `json:"gate"`
	Brain           *AutopilotBrain     `json:"brain"`
	WorkersInFlight int                 `json:"workers_in_flight"`
	Tasks           AutopilotTaskCounts `json:"tasks"`
	Backoff         *AutopilotBackoff   `json:"backoff"`
	LandedTotal     int                 `json:"landed_total"`
}

// RegisterAutopilotRun adds a named plan to the durable registry without
// starting it.
func (c *Client) RegisterAutopilotRun(ctx context.Context, name, repo, planFile string) (AutopilotRunStatus, error) {
	var out AutopilotRunStatus
	err := c.doT(ctx, longTimeout, http.MethodPost, "/autopilot/runs", map[string]string{"name": name, "repo": repo, "plan_file": planFile}, &out)
	return out, err
}

func (c *Client) ListAutopilotRuns(ctx context.Context) ([]AutopilotRunStatus, error) {
	var out []AutopilotRunStatus
	err := c.do(ctx, http.MethodGet, "/autopilot/runs", nil, &out)
	return out, err
}

func (c *Client) ControlAutopilotRun(ctx context.Context, runID, action string) (AutopilotRunStatus, error) {
	var out AutopilotRunStatus
	err := c.doT(ctx, longTimeout, http.MethodPost, "/autopilot/runs/"+url.PathEscape(runID)+"/"+url.PathEscape(action), nil, &out)
	return out, err
}

type AutopilotPlanTask struct {
	ID       string   `json:"id"`
	Prompt   string   `json:"prompt"`
	After    []string `json:"after"`
	Status   string   `json:"status"`
	LandedPR int      `json:"landed_pr,omitempty"`
}

func (c *Client) UpdateAutopilotTaskStatus(ctx context.Context, runID, taskID, status string, landedPR int) (AutopilotPlanTask, error) {
	var out AutopilotPlanTask
	body := map[string]any{"run_id": runID, "task_id": taskID, "status": status}
	if landedPR > 0 {
		body["landed_pr"] = landedPR
	}
	err := c.doT(ctx, longTimeout, http.MethodPost, "/autopilot/tasks/status", body, &out)
	return out, err
}

// AutopilotBrain describes the run's brain agent (nil in the S1 inert core).
type AutopilotBrain struct {
	AgentID       string `json:"agent_id"`
	Backend       string `json:"backend"`
	Tier          string `json:"tier"`
	LastHeartbeat string `json:"last_heartbeat"`
	ContextLevel  string `json:"context_level"`
}

// AutopilotTaskCounts is the ledger task rollup shown in status.
type AutopilotTaskCounts struct {
	Pending    int `json:"pending"`
	InProgress int `json:"in_progress"`
	Landed     int `json:"landed"`
	Failed     int `json:"failed"`
}

// AutopilotBackoff describes the guardian's capped backoff (nil unless degraded).
type AutopilotBackoff struct {
	Stage       int    `json:"stage"`
	NextRetryAt string `json:"next_retry_at"`
	LastError   string `json:"last_error"`
}

// AutopilotPreflightError is the 409 body when enabling fails preflight: the full
// list of actionable failures (autopilot.md §5.1). Client surfaces it verbatim.
type AutopilotPreflightError struct {
	Summary  string
	Failures []string
}

func (e *AutopilotPreflightError) Error() string {
	if len(e.Failures) == 0 {
		return e.Summary
	}
	return e.Summary + ": " + strings.Join(e.Failures, "; ")
}

// GetAutopilot fetches the autopilot status (GET /autopilot).
func (c *Client) GetAutopilot(ctx context.Context) (AutopilotStatus, error) {
	var st AutopilotStatus
	if err := c.do(ctx, http.MethodGet, "/autopilot", nil, &st); err != nil {
		return AutopilotStatus{}, err
	}
	return st, nil
}

// SetAutopilot flips the per-repo switch (POST /autopilot). repo scopes the toggle
// to one repository (empty ⇒ the daemon's working directory). On a 409 preflight
// failure it returns a *AutopilotPreflightError carrying the full failure list so
// callers can print every problem in one pass.
func (c *Client) SetAutopilot(ctx context.Context, enabled bool, repo string) (AutopilotStatus, error) {
	var st AutopilotStatus
	body := map[string]any{"enabled": enabled}
	if strings.TrimSpace(repo) != "" {
		body["repo"] = repo
	}
	// longTimeout: enabling runs the preflight, which may auto-create the
	// integration branch and shell git/gh.
	err := c.doT(ctx, longTimeout, http.MethodPost, "/autopilot", body, &st)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusConflict {
			if pfe := parseAutopilotPreflight(se.Body); pfe != nil {
				return AutopilotStatus{}, pfe
			}
		}
		return AutopilotStatus{}, err
	}
	return st, nil
}

// parseAutopilotPreflight decodes a 409 body into the typed preflight error, or
// nil if the body is not the expected shape.
func parseAutopilotPreflight(body []byte) *AutopilotPreflightError {
	var wire struct {
		Error    string   `json:"error"`
		Failures []string `json:"failures"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || len(wire.Failures) == 0 {
		return nil
	}
	summary := wire.Error
	if summary == "" {
		summary = "autopilot preflight failed"
	}
	return &AutopilotPreflightError{Summary: summary, Failures: wire.Failures}
}

// CompleteAutopilot declares the calling brain's run complete (POST
// /autopilot/complete). The daemon derives the run from the caller's own brain
// identity, writes the in-place completion marker into the plan file so preflight
// skips it, tears the brain down, and retains the ledger. Idempotent. Returns the
// resulting status (the run reports state=complete).
func (c *Client) CompleteAutopilot(ctx context.Context) (AutopilotStatus, error) {
	var st AutopilotStatus
	// longTimeout: completion writes the plan file and tears the brain's tmux
	// session down.
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/autopilot/complete", nil, &st); err != nil {
		return AutopilotStatus{}, err
	}
	return st, nil
}

// AutopilotLandResult mirrors the daemon's POST /autopilot/land 200 body
// (autopilot.md §6). AlreadyLanded is true on an idempotent re-issue.
type AutopilotLandResult struct {
	SHA           string `json:"sha"`
	PR            int    `json:"pr"`
	Branch        string `json:"branch"`
	AlreadyLanded bool   `json:"already_landed"`
}

// AutopilotLandError is the 409 body when a land precondition fails (autopilot.md
// §6): a typed Kind the brain reasons over plus optional Detail. No side effects
// occurred.
type AutopilotLandError struct {
	Kind    string
	Detail  string
	Summary string
}

func (e *AutopilotLandError) Error() string {
	msg := e.Summary
	if msg == "" {
		msg = "autopilot land failed"
	}
	if e.Kind != "" {
		msg += " (" + e.Kind + ")"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Land merges one autopilot worker branch into the integration branch (POST
// /autopilot/land). On a 409 precondition failure it returns a typed
// *AutopilotLandError carrying the kind so callers branch on the failure.
func (c *Client) Land(ctx context.Context, agentOrBranch string) (AutopilotLandResult, error) {
	var res AutopilotLandResult
	body := map[string]string{"agent_or_branch": agentOrBranch}
	// longTimeout: land shells gh (PR lookup, gate, merge) and may run the check rail.
	err := c.doT(ctx, longTimeout, http.MethodPost, "/autopilot/land", body, &res)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusConflict {
			if le := parseAutopilotLand(se.Body); le != nil {
				return AutopilotLandResult{}, le
			}
		}
		return AutopilotLandResult{}, err
	}
	return res, nil
}

// parseAutopilotLand decodes a 409 body into the typed land error, or nil if the
// body is not the expected shape.
func parseAutopilotLand(body []byte) *AutopilotLandError {
	var wire struct {
		Error  string `json:"error"`
		Kind   string `json:"kind"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.Kind == "" {
		return nil
	}
	return &AutopilotLandError{Kind: wire.Kind, Detail: wire.Detail, Summary: wire.Error}
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

// GetAgentHistory fetches per-agent performance summaries with anomaly warnings
// (GET /metrics/history?summary=true). since is an RFC3339 timestamp ("" lets
// the daemon default to its look-back window); agent ("" ⇒ all) narrows to one
// agent ID.
func (c *Client) GetAgentHistory(ctx context.Context, since, agent string) ([]metrics.AgentSummary, error) {
	q := url.Values{}
	q.Set("summary", "true")
	if since != "" {
		q.Set("since", since)
	}
	if agent != "" {
		q.Set("agent", agent)
	}
	p := "/metrics/history?" + q.Encode()
	var resp struct {
		Summaries []metrics.AgentSummary `json:"summaries"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Summaries, nil
}

func (c *Client) SetAutoApprove(ctx context.Context, id string, enabled bool) error {
	body := map[string]bool{"enabled": enabled}
	return c.do(ctx, http.MethodPatch, "/sessions/"+id+"/auto-approve", body, nil)
}

// GetAutoApprovePolicy reads the live auto-approve policy (default allow/deny
// rules plus per-agent overrides) the daemon's poller is running.
func (c *Client) GetAutoApprovePolicy(ctx context.Context) (approval.Policy, error) {
	var pol approval.Policy
	if err := c.do(ctx, http.MethodGet, "/auto-approve/policy", nil, &pol); err != nil {
		return approval.Policy{}, err
	}
	return pol, nil
}

// PutAutoApprovePolicy replaces the live auto-approve policy (effective
// immediately) and persists it to the config file. Returns the stored policy.
func (c *Client) PutAutoApprovePolicy(ctx context.Context, pol approval.Policy) (approval.Policy, error) {
	var out approval.Policy
	if err := c.do(ctx, http.MethodPut, "/auto-approve/policy", pol, &out); err != nil {
		return approval.Policy{}, err
	}
	return out, nil
}

func (c *Client) SetPermissionMode(ctx context.Context, id string, mode string) error {
	body := map[string]string{"permission_mode": mode}
	return c.do(ctx, http.MethodPatch, "/sessions/"+id+"/permission-mode", body, nil)
}

// RoleInfo is a built-in agent role for a picker (name + one-line description),
// mirroring the daemon's GET /roles response items.
type RoleInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SetRole persists an agent's built-in role (empty ⇒ general) and relaunches it so
// the new persona re-injects. The daemon validates the role name.
func (c *Client) SetRole(ctx context.Context, id, roleName string) error {
	body := map[string]string{"role": roleName}
	return c.do(ctx, http.MethodPatch, "/sessions/"+id+"/role", body, nil)
}

// ListRoles returns warden's built-in roles (general first, then alphabetical) for
// a role picker.
func (c *Client) ListRoles(ctx context.Context) ([]RoleInfo, error) {
	var resp struct {
		Roles []RoleInfo `json:"roles"`
	}
	if err := c.do(ctx, http.MethodGet, "/roles", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Roles, nil
}

// Backend is one row of the agent-backend registry, mirroring the daemon's
// backendstore.Backend wire shape (docs/specs/2026-08-06-backend-registry.md).
type Backend struct {
	ID           string    `json:"id"`
	Installed    bool      `json:"installed"`
	BinaryPath   string    `json:"binary_path"`
	DetectedAt   time.Time `json:"detected_at"`
	Tier         string    `json:"tier"`
	Default      bool      `json:"default"`
	Enabled      bool      `json:"enabled"`
	IsLocal      bool      `json:"is_local"`
	LimitedUntil time.Time `json:"limited_until,omitzero"`
}

// BackendSettings is the store-level backend policy singleton, mirroring the
// daemon's backendstore.Settings wire shape.
type BackendSettings struct {
	ID                   string `json:"id"`
	InternalThinkingMode string `json:"internal_thinking_mode"`
	AllowPaidAutopilot   bool   `json:"allow_paid_autopilot"`
}

// BackendsState is the full backend registry (rows sorted by id) plus settings,
// the response of GET /backends, POST /backends/rescan, and PUT /backends/default.
type BackendsState struct {
	Backends []Backend       `json:"backends"`
	Settings BackendSettings `json:"settings"`
}

// ListBackends returns the persisted agent-backend registry plus settings. The
// store is warden's source of truth for which backends exist, their tier, the
// default, and whether each is enabled.
func (c *Client) ListBackends(ctx context.Context) (BackendsState, error) {
	var out BackendsState
	if err := c.do(ctx, http.MethodGet, "/backends", nil, &out); err != nil {
		return BackendsState{}, err
	}
	return out, nil
}

// RescanBackends re-detects installed backends (reconciling detection fields while
// preserving tier/default/enabled) and returns the refreshed registry.
func (c *Client) RescanBackends(ctx context.Context) (BackendsState, error) {
	var out BackendsState
	if err := c.do(ctx, http.MethodPost, "/backends/rescan", nil, &out); err != nil {
		return BackendsState{}, err
	}
	return out, nil
}

// SetBackendTier assigns a backend's billing tier (free|subscription|pay_per_use|
// unclassified). The reserved local tier is system-set and rejected by the daemon.
func (c *Client) SetBackendTier(ctx context.Context, id, tier string) (Backend, error) {
	var out Backend
	body := map[string]any{"tier": tier}
	if err := c.do(ctx, http.MethodPatch, "/backends/"+id, body, &out); err != nil {
		return Backend{}, err
	}
	return out, nil
}

// SetBackendEnabled toggles whether a backend may be used.
func (c *Client) SetBackendEnabled(ctx context.Context, id string, enabled bool) (Backend, error) {
	var out Backend
	body := map[string]any{"enabled": enabled}
	if err := c.do(ctx, http.MethodPatch, "/backends/"+id, body, &out); err != nil {
		return Backend{}, err
	}
	return out, nil
}

// SetDefaultBackend makes id the single default backend. The daemon rejects an
// unknown, uninstalled, disabled, or reserved (local/terminal) target.
func (c *Client) SetDefaultBackend(ctx context.Context, id string) (BackendsState, error) {
	var out BackendsState
	body := map[string]string{"id": id}
	if err := c.do(ctx, http.MethodPut, "/backends/default", body, &out); err != nil {
		return BackendsState{}, err
	}
	return out, nil
}

// SetThinkingMode sets the internal-thinking routing mode (local_only |
// free_plus_local). Returns the updated settings.
func (c *Client) SetThinkingMode(ctx context.Context, mode string) (BackendSettings, error) {
	var out BackendSettings
	body := map[string]string{"mode": mode}
	if err := c.do(ctx, http.MethodPut, "/backends/thinking-mode", body, &out); err != nil {
		return BackendSettings{}, err
	}
	return out, nil
}

// SetForceCompact sets an agent's force-compact override. state must be one of
// "on", "off", or "inherit" (clears the override so the agent follows the global
// token_force_compact).
func (c *Client) SetForceCompact(ctx context.Context, id, state string) error {
	body := map[string]string{"state": state}
	return c.do(ctx, http.MethodPatch, "/sessions/"+id+"/force-compact", body, nil)
}

// SetName renames an agent (blank clears the name). The daemon validates the
// format and rejects a name already used by another session.
func (c *Client) SetName(ctx context.Context, id, name string) error {
	body := map[string]string{"name": name}
	return c.do(ctx, http.MethodPatch, "/sessions/"+id+"/name", body, nil)
}

// ListModels returns all registered models, optionally filtered by tier (tier-1, tier-2, tier-3).
func (c *Client) ListModels(ctx context.Context, tier string) ([]backendstore.ModelEntry, error) {
	var out []backendstore.ModelEntry
	path := "/models"
	if tier != "" {
		path += "?tier=" + url.QueryEscape(tier)
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetModelTier updates the tier for a specific model in the catalog.
func (c *Client) SetModelTier(ctx context.Context, backend, model, tier string) (backendstore.ModelEntry, error) {
	var out backendstore.ModelEntry
	body := map[string]string{"tier": tier}
	path := fmt.Sprintf("/models/%s/%s/tier", url.PathEscape(backend), url.PathEscape(model))
	if err := c.do(ctx, http.MethodPut, path, body, &out); err != nil {
		return backendstore.ModelEntry{}, err
	}
	return out, nil
}

// ListRoleTiers returns all role-to-tier mappings.
func (c *Client) ListRoleTiers(ctx context.Context) ([]backendstore.RoleTierMapping, error) {
	var out []backendstore.RoleTierMapping
	if err := c.do(ctx, http.MethodGet, "/roles/tiers", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetRoleTier updates the default model tier for an agent role.
func (c *Client) SetRoleTier(ctx context.Context, role, tier string) (backendstore.RoleTierMapping, error) {
	var out backendstore.RoleTierMapping
	body := map[string]string{"tier": tier}
	path := fmt.Sprintf("/roles/tiers/%s", url.PathEscape(role))
	if err := c.do(ctx, http.MethodPut, path, body, &out); err != nil {
		return backendstore.RoleTierMapping{}, err
	}
	return out, nil
}

// SwitchSessionParams holds the parameters for switching an agent session mid-task.
type SwitchSessionParams struct {
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	Tier    string `json:"tier,omitempty"`
	Role    string `json:"role,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
}

// SwitchSession hot-swaps an agent session mid-task.
func (c *Client) SwitchSession(ctx context.Context, id string, params SwitchSessionParams) (lifecycle.SwapResult, error) {
	var out lifecycle.SwapResult
	path := fmt.Sprintf("/sessions/%s/switch", url.PathEscape(id))
	if err := c.do(ctx, http.MethodPost, path, params, &out); err != nil {
		return lifecycle.SwapResult{}, err
	}
	return out, nil
}

// GetHandoverSettings returns the current handover configuration.
func (c *Client) GetHandoverSettings(ctx context.Context) (backendstore.HandoverSettings, error) {
	var out backendstore.HandoverSettings
	if err := c.do(ctx, http.MethodGet, "/handover/settings", nil, &out); err != nil {
		return backendstore.HandoverSettings{}, err
	}
	return out, nil
}

// SetHandoverSettings updates the handover configuration.
func (c *Client) SetHandoverSettings(ctx context.Context, settings backendstore.HandoverSettings) (backendstore.HandoverSettings, error) {
	var out backendstore.HandoverSettings
	if err := c.do(ctx, http.MethodPut, "/handover/settings", settings, &out); err != nil {
		return backendstore.HandoverSettings{}, err
	}
	return out, nil
}
