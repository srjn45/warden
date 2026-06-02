package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"syscall"
	"time"

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

type Client struct {
	base string
	http *http.Client
}

func New(base string) *Client {
	return &Client{base: base, http: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
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
	Type     string
	Ticket   string
	Repo     string
	Branch   string
	PR       string
	Worktree bool
	Prompt   string
}

func (c *Client) Spawn(ctx context.Context, p SpawnParams) (*store.Session, error) {
	var s store.Session
	body := map[string]any{
		"type": p.Type, "ticket": p.Ticket, "repo": p.Repo,
		"branch": p.Branch, "pr": p.PR, "worktree": p.Worktree,
		"prompt": p.Prompt,
	}
	if err := c.do(ctx, http.MethodPost, "/spawn", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) Terminate(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/terminate", nil, nil)
}

func (c *Client) Delete(ctx context.Context, id string, hard bool) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/delete", map[string]bool{"hard": hard}, nil)
}

func (c *Client) RemoveWorktree(ctx context.Context, id string, force bool) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/remove-worktree", map[string]bool{"force": force}, nil)
}

func (c *Client) Input(ctx context.Context, id, text string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/input", map[string]string{"text": text}, nil)
}

func (c *Client) Restore(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/restore", nil, nil)
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
