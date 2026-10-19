package daemon

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/decisionrouter"
	"github.com/srjn45/warden/internal/promptengine"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyobs"
)

// wirePromptEngine connects the state classifier's prompt reports to the
// normalized prompt router for runtime-backed sessions (contract §14 t6). It is
// inert while runtime.pty is off (no events flow) and a dry run in shadow.
// Called from Run so Fast-Brain and brain-consult are already configured.
func (s *Server) wirePromptEngine() {
	if s.pty == nil || s.classifier == nil || s.promptEngine != nil {
		return
	}
	learnedDir := filepath.Join(s.pty.Dir(), "learned")
	var shapes decisionrouter.LearnedShapeStore
	if fs, err := decisionrouter.OpenFileLearnedShapeStore(filepath.Join(learnedDir, "shapes.json")); err != nil {
		// Never silently reset: a corrupt file may hold retirements. Fall back to
		// memory (no learned shape is usable after a restart: fail safe).
		slog.Warn("promptengine: learned shape store unreadable; learned shapes disabled for this run", "err", err)
		shapes = decisionrouter.NewMemoryLearnedShapeStore()
	} else {
		shapes = fs
	}
	fixtures := decisionrouter.DirFixtureStore{Dir: filepath.Join(learnedDir, "fixtures")}

	classif := decisionrouter.NewPromptClassifier(shapes, s.fastBrain)
	router := decisionrouter.New(decisionrouter.RouterOptions{
		Classifier: classif,
		ShapeStore: shapes,
		FastBrain:  s.fastBrain,
		Consultor:  s.brainConsultor,
		HumanInbox: promptengine.HumanNotifier{Notify: s.notify},
	})
	learner := &decisionrouter.Learner{Shapes: shapes, Fixtures: fixtures,
		Outcome: func(backend, outcome string) { s.ptyMetrics.Inc(ptyobs.MLearnedShape, backend, outcome) }}

	rt := &ptyRuntime{m: s.pty}
	eng := promptengine.New(promptengine.Config{
		Policy: func(ag *agentstore.Agent) approval.Policy {
			if s.poller == nil {
				return approval.Policy{}
			}
			return s.poller.AutoApprovePolicySnapshot().For(ag.Name, ag.ID)
		},
		Backend: func(ag *agentstore.Agent) agentbackend.Backend {
			if s.poller != nil && s.poller.Backend != nil {
				return s.poller.Backend(ag)
			}
			if b, err := agentbackend.Get(ag.AiCli); err == nil && b != nil {
				return b
			}
			return agentbackend.Default()
		},
		CLIVersion: newVersionProbe().Version,
		Notify:     s.notify,
		Trace:      s.openDecisionTrace(),
		Metrics:    s.ptyMetrics,
	}, rt, s.store, router, classif, learner)
	s.promptEngine = eng
	s.classifier.OnPrompt = eng.OnPrompt
	s.classifier.OnUnreadable = eng.OnUnreadable
}

// ptyRuntime adapts the PTY manager to promptengine.Runtime. A direct session
// is written through its input lease; a mirrored tmux pane through send-keys.
type ptyRuntime struct{ m *ptyhost.Manager }

func (r *ptyRuntime) Target(name string) (promptengine.Target, bool) {
	t, ok := r.m.Lookup(name)
	if !ok || t.Stream == nil {
		return promptengine.Target{}, false
	}
	if t.Direct != nil {
		return promptengine.Target{Stream: t.Stream, Writer: promptengine.DirectWriter{S: t.Direct}}, true
	}
	return promptengine.Target{Stream: t.Stream, Writer: promptengine.TmuxWriter{Host: r.m.Host(), Name: name}}, true
}

func (r *ptyRuntime) Authoritative(name string) bool { return r.m.IsAuthoritative(name) }

// versionProbe reports an installed CLI's `--version` text, cached: learned
// shapes pin it, so a CLI upgrade makes them unusable until re-validated. An
// unknown version ("") matches no pinned shape — fail safe.
type versionProbe struct {
	mu    sync.Mutex
	cache map[string]versionEntry
}

type versionEntry struct {
	v  string
	at time.Time
}

func newVersionProbe() *versionProbe { return &versionProbe{cache: map[string]versionEntry{}} }

func (p *versionProbe) Version(backendID string) string {
	p.mu.Lock()
	if e, ok := p.cache[backendID]; ok && time.Since(e.at) < 10*time.Minute {
		p.mu.Unlock()
		return e.v
	}
	p.mu.Unlock()

	v := ""
	if b, err := agentbackend.Get(backendID); err == nil && b != nil && b.Binary() != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err := exec.CommandContext(ctx, b.Binary(), "--version").Output()
		cancel()
		if err == nil {
			v = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		}
	}
	p.mu.Lock()
	p.cache[backendID] = versionEntry{v: v, at: time.Now()}
	p.mu.Unlock()
	return v
}
