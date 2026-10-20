//go:build linux || darwin

package ptyattach

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/srjn45/warden/internal/ptyhost"
)

// ErrUnknownRuntime means the resolver could not establish a runtime kind for
// a session. Callers must present the resolver's reason instead of guessing a
// tmux target.
var ErrUnknownRuntime = errors.New("ptyattach: unknown session runtime")

// Target is the resolved identity of a session to attach to. It deliberately
// carries RuntimeInfo rather than a presentation-derived guess so every UI can
// use the same direct-or-tmux decision.
type Target struct {
	SessionID string
	Info      ptyhost.RuntimeInfo
}

// Client is the shared attach/control client consumed by CLI, TUI, and web
// surfaces. The existing Run entry point remains available for callers that
// already own a fully formed WebSocket URL.
type Client interface {
	Attach(context.Context, Target, Options) (Result, error)
	// Fallback returns the compatibility command for a tmux target. Unknown
	// targets return ErrUnknownRuntime (with the resolver reason) so callers
	// never silently guess a tmux target.
	Fallback(Target) (argv []string, err error)
}

type client struct {
	daemonURL string
	token     string
}

// Open creates an attach client for daemonURL. daemonURL accepts either an
// HTTP(S) daemon address or an already-WebSocket URL.
func Open(daemonURL, token string) Client {
	return &client{daemonURL: daemonURL, token: token}
}

func (c *client) Attach(ctx context.Context, t Target, o Options) (Result, error) {
	switch t.Info.Kind {
	case "direct":
		if t.SessionID == "" {
			return Result{}, fmt.Errorf("ptyattach: direct session has no session id")
		}
		if o.URL == "" {
			u, err := attachURL(c.daemonURL, t.SessionID)
			if err != nil {
				return Result{}, err
			}
			o.URL = u
		}
		if o.Token == "" {
			o.Token = c.token
		}
		return Run(ctx, o)
	case "tmux":
		return Result{}, ErrNotDirect
	default:
		reason := t.Info.Reason
		if reason == "" {
			reason = "the daemon did not report a supported runtime kind"
		}
		return Result{}, fmt.Errorf("%w: %s", ErrUnknownRuntime, reason)
	}
}

func (c *client) Fallback(t Target) ([]string, error) {
	if t.Info.Kind == "unknown" || t.Info.Kind == "" {
		reason := t.Info.Reason
		if reason == "" {
			reason = "the daemon did not report a supported runtime kind"
		}
		return nil, fmt.Errorf("%w: %s", ErrUnknownRuntime, reason)
	}
	if t.Info.Kind != "tmux" {
		return nil, nil
	}
	key := t.Info.Key
	if key == "" {
		key = t.SessionID
	}
	if key == "" {
		return nil, fmt.Errorf("%w: tmux runtime has no attach key", ErrUnknownRuntime)
	}
	return []string{"tmux", "attach", "-t", key}, nil
}

func attachURL(daemonURL, sessionID string) (string, error) {
	if daemonURL == "" {
		return "", errors.New("ptyattach: daemon URL is required")
	}
	u, err := url.Parse(daemonURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("ptyattach: invalid daemon URL %q", daemonURL)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("ptyattach: invalid daemon URL scheme %q", u.Scheme)
	}
	u.Path = path.Join(u.Path, "api", "v1", "sessions", url.PathEscape(sessionID), "pty")
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}
