package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
)

// ErrDaemonDown signals the daemon is unreachable (connection refused / timeout).
var ErrDaemonDown = errors.New("daemon not running — start it with `agentctl daemon` (or via launchd)")

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
		var nerr net.Error
		if errors.As(err, &nerr) || isConnRefused(err) {
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
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("daemon error (%d): %s", resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func isConnRefused(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
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
