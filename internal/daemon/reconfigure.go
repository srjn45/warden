package daemon

import (
	"log/slog"
	"strings"

	"github.com/srjn45/warden/internal/config"
)

// SetBaselineConfig records the config the daemon applied at startup as the
// last-good baseline WITHOUT re-applying it (startup already wired every
// subsystem). ApplyConfig diffs each reload against this baseline to report which
// changed keys need a restart. Call once, after the subsystems are wired.
func (s *Server) SetBaselineConfig(cfg config.Config) {
	s.reloadMu.Lock()
	s.appliedConfig = cfg
	s.reloadMu.Unlock()
}

// snapshotConfig returns a copy of the last-applied config under reloadMu, for
// handlers that need a consistent read of live config values (e.g. the backend
// rescan's local-LLM gate).
func (s *Server) snapshotConfig() config.Config {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	return s.appliedConfig
}

// AddReloadHook registers a subsystem reconfigure closure run by ApplyConfig for
// subsystems the Server does not own directly (the lifecycle config swap, the
// notifier chain, the autopilot ControllerConfig). Hooks run in registration
// order after the Server-owned subsystems are updated. Call during startup wiring.
func (s *Server) AddReloadHook(fn func(config.Config)) {
	if fn == nil {
		return
	}
	s.reloadMu.Lock()
	s.reloadHooks = append(s.reloadHooks, fn)
	s.reloadMu.Unlock()
}

// ApplyConfig is the single config hot-reload fan-out (feature 3): it pushes a
// freshly reloaded, validated config into every subsystem that can accept it
// live, then logs which changed keys still require a restart. It is the callback
// the config watcher invokes on a good reload; a BAD edit never reaches here (the
// watcher keeps the last-good config), so ApplyConfig always receives a valid one.
//
// Live subsystems, updated here or via a registered reload hook:
//   - auto_approve policy + tokens.* context/token guard → the poller (thread-safe
//     setters), so guard thresholds, warn/critical bands, and auto-/force-compact
//     take effect on the next tick;
//   - api_docs → the public-docs route gate;
//   - scheduler_enabled → the schedule route gate (the reconcile LOOP lifecycle
//     still needs a restart — reported below);
//   - rails.*, model_default, default_permission_mode, hint gates → the lifecycle
//     (reload hook → Lifecycle.SetConfig), applied on the next spawn/resume;
//   - notify.* + webhook → the swappable notifier (reload hook → notify.Switch.Set);
//   - autopilot plan/brain/merge template + per-repo reconcile → the controller
//     (reload hook → Controller.Reconfigure), preserving the persisted enable set.
//
// Everything else (bind address, data dir, plugins, local_llm, the collab/
// branch-track/rate-limit loop intervals, HTTP write budgets, metrics recorder,
// auto-restart, logging) is structural or wired into a loop started once at boot;
// those are reported as changed-but-need-restart rather than silently ignored.
func (s *Server) ApplyConfig(cfg config.Config) {
	s.reloadMu.Lock()
	old := s.appliedConfig
	hooks := make([]func(config.Config), len(s.reloadHooks))
	copy(hooks, s.reloadHooks)
	s.appliedConfig = cfg
	s.reloadMu.Unlock()

	// --- Subsystems the Server owns directly ---
	if s.poller != nil {
		// Auto-approve policy: same live-swap the PUT /auto-approve/policy handler uses.
		s.poller.SetAutoApprovePolicy(cfg.AutoApprove)
		// Context/token guard: guard on/off, warn/critical bands, warn alerting,
		// auto-/force-compact, and the compact resume prompt — applied on the next tick.
		s.poller.SetContextGuard(
			cfg.Tokens.Guard,
			cfg.Tokens.Warn,
			cfg.Tokens.Critical,
			cfg.Tokens.WarnAlert,
			cfg.Tokens.AutoCompact,
			cfg.Tokens.ForceCompact,
			cfg.Tokens.CompactResumePrompt,
		)
	}
	// Public OpenAPI docs route gate.
	s.SetAPIDocs(cfg.ApiDocs)
	// Scheduler ROUTE gate (403 when off) — a single bool, set like SetAPIDocs.
	// The reconcile LOOP was started once at boot, so newly enabling/disabling the
	// scheduler or changing its cadence needs a restart to start/stop/re-tick the
	// loop; that is reported in the restart-only diff below.
	s.scheduler = cfg.SchedulerEnabled

	// --- Subsystems reached through daemon-registered reload hooks ---
	// (lifecycle config swap, notifier chain rebuild, autopilot Controller.Reconfigure)
	for _, fn := range hooks {
		fn(cfg)
	}

	// --- Report changed keys that could not be applied live ---
	s.logRestartOnly(old, cfg)
}

// logRestartOnly compares the restart-only keys of old vs new and logs a single
// consolidated warning naming those that changed, so an operator who edited one
// isn't left wondering why nothing happened. Purely informational — nothing here
// is applied live by design (each backs a bind, a store, an external process, or a
// loop started once at daemon boot).
func (s *Server) logRestartOnly(old, cur config.Config) {
	var changed []string
	add := func(name string, differs bool) {
		if differs {
			changed = append(changed, name)
		}
	}
	add("addr", old.Addr != cur.Addr)
	add("data_dir", old.DataDir != cur.DataDir)
	add("claude_projects_dir", old.ClaudeProjectsDir != cur.ClaudeProjectsDir)
	add("metrics", old.MetricsEnabled != cur.MetricsEnabled)
	add("scheduler_enabled (reconcile loop)", old.SchedulerEnabled != cur.SchedulerEnabled)
	add("trusted_proxies", !strEqual(old.TrustedProxies, cur.TrustedProxies))
	add("http.timeout_fast", old.HTTP.TimeoutFast != cur.HTTP.TimeoutFast)
	add("http.timeout_slow", old.HTTP.TimeoutSlow != cur.HTTP.TimeoutSlow)
	add("plugins.enabled", old.Plugins.Enabled != cur.Plugins.Enabled)
	add("plugins.registry", len(old.Plugins.Registry) != len(cur.Plugins.Registry))
	add("local_llm.*", old.LocalLLM != cur.LocalLLM)
	add("collab.enabled", old.Collab.Enabled != cur.Collab.Enabled)
	add("collab.interval", old.Collab.Interval != cur.Collab.Interval)
	add("collab.git_reconcile_interval", old.Collab.GitReconcileInterval != cur.Collab.GitReconcileInterval)
	add("branch_track.enabled", old.BranchTrack.Enabled != cur.BranchTrack.Enabled)
	add("branch_track.interval", old.BranchTrack.Interval != cur.BranchTrack.Interval)
	add("rate_limit.*", old.RateLimit != cur.RateLimit)
	add("auto_restart.*", old.AutoRestart != cur.AutoRestart)
	add("log.*", old.Log != cur.Log)
	add("memory.curate", old.Memory.Curate != cur.Memory.Curate)
	add("snapshots", old.Snapshots != cur.Snapshots)

	if len(changed) > 0 {
		slog.Warn("config: some changed settings need a daemon restart to take effect",
			"keys", strings.Join(changed, ", "))
	}
}

// strEqual reports whether two string slices are element-wise equal (order-
// sensitive), used to diff trusted_proxies for the restart-only report.
func strEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
