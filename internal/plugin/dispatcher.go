package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"time"
)

// DefaultTimeout bounds every plugin invocation. A hook runs inline at a
// lifecycle point, so a slow or hung plugin must be killed quickly and skipped
// rather than stalling the agent — same posture as the PreToolUse guard's 3s cap.
const DefaultTimeout = 5 * time.Second

// Dispatcher invokes subscribed plugins at lifecycle hook events over the
// JSON-over-stdio protocol. Every invocation is FAIL-OPEN: a missing binary, a
// non-zero exit, a timeout, or malformed output is logged and skipped — it never
// blocks, errors, or panics the caller. A nil *Dispatcher is a valid no-op, so
// callers can hold one unconditionally and call Dispatch whether or not plugins
// are enabled.
type Dispatcher struct {
	reg     *Registry
	timeout time.Duration
	// runner executes one plugin and returns its raw stdout; swappable in tests
	// so the happy path can be exercised without a real subprocess. nil ⇒ the
	// real exec-based runner.
	runner func(ctx context.Context, path string, stdin []byte) ([]byte, error)
}

// NewDispatcher builds a dispatcher over reg with DefaultTimeout. A nil registry
// is fine (Dispatch becomes a no-op).
func NewDispatcher(reg *Registry) *Dispatcher {
	return &Dispatcher{reg: reg, timeout: DefaultTimeout}
}

// Dispatch invokes every plugin subscribed to event, sequentially, each bounded
// by the dispatcher timeout. It is best-effort and fail-open: errors are logged,
// never returned. Payload is optional event-specific context (e.g. commit
// message). Safe on a nil dispatcher or nil/empty registry.
func (d *Dispatcher) Dispatch(ctx context.Context, event HookEvent, meta SessionMeta, payload map[string]string) {
	if d == nil || d.reg == nil {
		return
	}
	subs := d.reg.subscribers(event)
	if len(subs) == 0 {
		return
	}
	req := Request{
		ProtocolVersion: ProtocolVersion,
		Event:           event,
		Session:         meta,
		Payload:         payload,
	}
	body, err := json.Marshal(req)
	if err != nil { // a map of strings + scalars can't fail; defensive only
		slog.Warn("plugin: marshal request failed", "event", event, "err", err)
		return
	}
	for _, p := range subs {
		d.invoke(ctx, p, body)
	}
}

// invoke runs one plugin and records the outcome. It is the sole fail-open
// boundary: nothing it does propagates an error to Dispatch's caller.
func (d *Dispatcher) invoke(ctx context.Context, p Plugin, body []byte) {
	run := d.runner
	if run == nil {
		run = execRunner(d.timeout)
	}
	out, err := run(ctx, p.Path, body)
	if err != nil {
		slog.Warn("plugin: invocation failed (skipped, fail-open)", "plugin", p.Name, "path", p.Path, "err", err)
		return
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		// Empty stdout is allowed: a plugin that just observed the event need
		// not reply. Treated as a silent ack.
		return
	}
	var resp Response
	if err := json.Unmarshal(out, &resp); err != nil {
		slog.Warn("plugin: malformed response (ignored, fail-open)", "plugin", p.Name, "err", err)
		return
	}
	if resp.ProtocolVersion != ProtocolVersion {
		slog.Warn("plugin: response protocol version mismatch", "plugin", p.Name, "got", resp.ProtocolVersion, "want", ProtocolVersion)
	}
	if !resp.OK {
		slog.Info("plugin: reported not-ok (advisory only)", "plugin", p.Name, "message", resp.Message)
		return
	}
	if resp.Message != "" {
		slog.Debug("plugin: ok", "plugin", p.Name, "message", resp.Message)
	}
}

// execRunner returns a runner that shells out to the plugin executable with a
// hard CommandContext timeout, feeding the request on stdin and capturing stdout.
// A separate timeout context guarantees the kill even if the caller's ctx has no
// deadline.
func execRunner(timeout time.Duration) func(ctx context.Context, path string, stdin []byte) ([]byte, error) {
	return func(ctx context.Context, path string, stdin []byte) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, path)
		cmd.Stdin = bytes.NewReader(stdin)
		var out bytes.Buffer
		cmd.Stdout = &out
		// WaitDelay bounds the wait after the context fires: without it, Run blocks
		// until EOF on the stdout pipe, which a grandchild (e.g. a `sleep` spawned by
		// the plugin) can hold open long past the kill — defeating the timeout. With
		// it, Run force-closes the pipes shortly after the process is signalled.
		cmd.WaitDelay = time.Second
		// stderr is left to inherit-nothing (discarded) so a chatty plugin can't
		// corrupt the stdout protocol channel; the exit code carries the failure.
		if err := cmd.Run(); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}
}
