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
	"strings"
	"syscall"
	"time"

	"github.com/srajanpathak/agentctl/internal/approval"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/store"
)

// ErrDaemonDown signals the daemon is unreachable (connection refused / timeout).
var ErrDaemonDown = errors.New("daemon not running — start it with `agentctl daemon` (or via launchd)")

// StatusError is returned for non-2xx daemon responses, exposing the HTTP code.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("daemon error (%d): %s", e.Code, e.Msg)
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
	base string
	http *http.Client
}

func New(base string) *Client {
	// No blanket http.Client.Timeout: per-call deadlines come from the context
	// (see do/doT), so long operations are not capped to a read-sized timeout.
	return &Client{base: base, http: &http.Client{}}
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
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Error
		if msg == "" {
			msg = resp.Status
		}
		return &StatusError{Code: resp.StatusCode, Msg: msg}
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
	Type       string
	Ticket     string
	Repo       string
	Branch     string
	PR         string
	Worktree   bool
	Prompt     string
	Cwd        string
	Supervised bool
}

func (c *Client) Spawn(ctx context.Context, p SpawnParams) (*store.Session, error) {
	var s store.Session
	body := map[string]any{
		"type": p.Type, "ticket": p.Ticket, "repo": p.Repo,
		"branch": p.Branch, "pr": p.PR, "worktree": p.Worktree,
		"prompt": p.Prompt, "cwd": p.Cwd, "supervised": p.Supervised,
	}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/spawn", body, &s); err != nil {
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

func (c *Client) RemoveWorktree(ctx context.Context, id string, force bool) error {
	return c.doT(ctx, longTimeout, http.MethodPost, "/sessions/"+id+"/remove-worktree", map[string]bool{"force": force}, nil)
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

func (c *Client) PipelineEmit(ctx context.Context, pid, job, text string) error {
	path := "/pipelines/" + url.PathEscape(pid) + "/jobs/" + url.PathEscape(job) + "/emit"
	// longTimeout: emit reconciles and may spawn dependent worktree jobs.
	return c.doT(ctx, longTimeout, http.MethodPost, path, map[string]string{"text": text}, nil)
}
