package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/curate"
	"github.com/srjn45/warden/internal/daemon"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/internalrouter"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/logging"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/memory"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/plugin"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/spend"
	"github.com/srjn45/warden/internal/store"
)

// requireTokenForNonLoopback rejects a non-loopback bind that has no bearer
// token. A token is mandatory for any non-loopback address (audit #7): serving
// the API on a public interface without auth is never permitted, and
// allow_nonloopback no longer relaxes this. A loopback bind needs no token (the
// hostGuard middleware defends it against DNS rebinding).
func requireTokenForNonLoopback(addr, token string) error {
	if !config.IsLoopbackHost(addr) && token == "" {
		return fmt.Errorf("refusing to bind non-loopback address %q without authentication: set %s (run `warden token generate`) to require a bearer token", addr, auth.TokenEnv)
	}
	return nil
}

// requireReadonlyHasPrimary rejects a read-only token configured without a
// primary token. That combination is a footgun: with no primary token auth is
// off entirely, so a "read-only" token would silently grant full access to every
// caller. We fail safe and refuse to start.
func requireReadonlyHasPrimary(token, readonlyToken string) error {
	if readonlyToken != "" && token == "" {
		return fmt.Errorf("%s is set but %s is not: read-only access requires a primary token (run `warden token generate`)", auth.ReadonlyTokenEnv, auth.TokenEnv)
	}
	return nil
}

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the warden hub (HTTP API + poller; the single writer to the file store)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := configPathFor(cmd)
			// Create/migrate the config file before loading, covering installs
			// (e.g. goreleaser tarballs) that never ran install.sh.
			if err := config.Reconcile(cfgPath); err != nil {
				slog.Warn("daemon: config reconcile failed", "path", cfgPath, "err", err)
			}
			config.WarnIfLegacyEnv(cfgPath)
			cfg := config.Load(cfgPath)

			// Install the structured logger before any further daemon work, so
			// the chosen level/format applies to the rest of startup. Flags win
			// over config; an invalid flag value is a hard error.
			level, format := cfg.Log.Level, cfg.Log.Format
			if v, _ := cmd.Flags().GetString("log-level"); v != "" {
				if !logging.ValidLevel(v) {
					return fmt.Errorf("invalid --log-level %q (want one of %s)", v, strings.Join(logging.Levels, ", "))
				}
				level = v
			}
			if v, _ := cmd.Flags().GetString("log-format"); v != "" {
				if !logging.ValidFormat(v) {
					return fmt.Errorf("invalid --log-format %q (want one of %s)", v, strings.Join(logging.Formats, ", "))
				}
				format = v
			}
			if _, err := logging.Setup(level, format); err != nil {
				return err
			}

			if a, _ := cmd.Flags().GetString("addr"); a != "" {
				cfg.Addr = a
			}
			authToken := auth.TokenFromEnv()
			if err := requireTokenForNonLoopback(cfg.Addr, authToken); err != nil {
				return err
			}
			readonlyToken := auth.ResolveReadonlyToken()
			if err := requireReadonlyHasPrimary(authToken, readonlyToken); err != nil {
				return err
			}
			trustedProxies, err := daemon.ParseTrustedProxies(cfg.TrustedProxies)
			if err != nil {
				return err
			}
			// allow_nonloopback used to bypass this token requirement; that hole is
			// closed (audit #7) — a bearer token is now mandatory for any
			// non-loopback bind. The field is retained as an inert, deprecated
			// no-op so existing configs still parse; warn so operators drop it.
			if cfg.AllowNonLoopback {
				slog.Warn("config allow_nonloopback is deprecated and no longer bypasses authentication; a bearer token is required for any non-loopback bind — remove it and set " + auth.TokenEnv + " instead")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			st, err := store.NewFileStore(cfg.DataDir)
			if err != nil {
				return err
			}
			defer st.Close(context.Background())

			cstore, err := ctxstore.New(filepath.Join(cfg.DataDir, "context"))
			if err != nil {
				return err
			}

			// ctxstore is now an embedded ScrivaDB store with background goroutines;
			// close it on shutdown to flush its index and stop the compaction loop.
			defer cstore.Close()

			mbox, err := mailbox.New(filepath.Join(cfg.DataDir, "inbox"))
			if err != nil {
				return err
			}

			// mailbox is an embedded ScrivaDB store (see cstore above); close on shutdown.
			defer mbox.Close()

			runner := lifecycle.HintingExecRunner{Inner: lifecycle.ExecRunner{}}
			lc := lifecycle.New(runner, cfg)
			lc.ProjectsDir = cfg.ClaudeProjectsDir
			// Prompt-mode agents launch in the caller's cwd; their initial prompt
			// file goes in this single shared dir (keyed by agent id), not a
			// per-agent directory.
			lc.PromptsDir = filepath.Join(cfg.DataDir, "prompts")
			// A flag-based backend's collab/git/pipeline addendum is written here
			// (keyed by agent id) and referenced via --append-system-prompt "$(cat …)"
			// so the launch line stays short — a long inline addendum overruns the tty
			// canonical-mode line limit (1024 B on macOS/BSD) and the agent won't start.
			lc.HintsDir = filepath.Join(cfg.DataDir, "hints")
			lc.ExitsDir = filepath.Join(cfg.DataDir, "exits")
			lc.SettingsDir = filepath.Join(cfg.DataDir, "settings")
			// The isolation-guard PreToolUse hook is the warden binary itself
			// (`<warden> hook guard`); resolve its absolute path for the generated
			// settings file. On failure the guard injection silently no-ops.
			if exe, err := os.Executable(); err == nil {
				lc.WardenBin = exe
			}
			// Optional local-model provider (Phase 1): only constructed when the
			// operator opts in, so the default build never reaches out to Ollama.
			// Classify routes through it first and falls back to Claude on any error.
			if cfg.LocalLLM.Enabled {
				o := llm.NewOllama(cfg.LocalLLM.URL, cfg.LocalLLM.Model, cfg.LocalLLMTimeoutDuration())
				lc.LLM = o
				slog.Info("local LLM enabled", "url", cfg.LocalLLM.URL, "model", cfg.LocalLLM.Model)
				// Validate the configured model is actually pulled: if it isn't, every
				// classify/summarize call 404s and silently escalates to a full Claude
				// process (a steady poller-driven load spike). Log a loud, actionable
				// ERROR at startup so the operator fixes it (pull the model or change
				// local_llm.model) — `wd doctor` also flags this. Best-effort: an
				// unreachable ollama here is not fatal (the model may still be pulled).
				vctx, vcancel := context.WithTimeout(context.Background(), 3*time.Second)
				if installed, err := o.InstalledModels(vctx); err != nil {
					slog.Warn("local LLM: could not verify configured model is installed", "model", cfg.LocalLLM.Model, "err", err)
				} else if !llm.ModelInstalled(cfg.LocalLLM.Model, installed) {
					slog.Error("local LLM: configured model is NOT installed in ollama — every classify/summarize will fall back to a full Claude process; run `ollama pull <model>` or fix local_llm.model",
						"model", cfg.LocalLLM.Model, "installed", installed)
				}
				vcancel()
			}
			life := daemon.NewLifecycleAdapter(lc, st)
			pd := daemon.NewPollerDeps(st, runner, lc)
			pl := poller.New(pd, 5*time.Minute)
			pl.TokenGuard = cfg.Tokens.Guard
			pl.TokenWarn = cfg.Tokens.Warn
			pl.TokenCrit = cfg.Tokens.Critical
			pl.WarnAlert = cfg.Tokens.WarnAlert
			pl.AutoCompact = cfg.Tokens.AutoCompact
			pl.ForceCompact = cfg.Tokens.ForceCompact
			pl.CompactResumePrompt = cfg.Tokens.CompactResumePrompt
			pl.AutoApprovePolicy = cfg.AutoApprove
			pl.RateLimitAutoResume = cfg.RateLimit.AutoResume
			pstore, err := pipeline.NewStore(filepath.Join(cfg.DataDir, "pipelines"))
			if err != nil {
				return err
			}
			// pstore is now an embedded ScrivaDB store with background goroutines
			// (see cstore above); close it on shutdown to flush and stop compaction.
			defer pstore.Close()
			if err := daemon.HardenDataDir(cfg.DataDir); err != nil {
				return err
			}
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.ApprovalsEnabled, cstore, mbox, nil)
			srv.SetAuth(authToken, readonlyToken)
			srv.SetWriteTimeouts(cfg.HTTPTimeoutFastDuration(), cfg.HTTPTimeoutSlowDuration())
			// Persist auto-approve policy changes (PUT /auto-approve/policy) back to
			// the config file so runtime rule edits survive a restart.
			srv.SetAutoApprovePersist(func(p approval.Policy) error {
				return config.WriteAutoApprove(cfgPath, p)
			})
			if cfg.Collab.Enabled {
				srv.SetCollabInterval(cfg.CollabIntervalDuration())
			} else {
				srv.SetCollabInterval(0)
			}
			snapStore, err := snapshot.NewStore(filepath.Join(cfg.DataDir, "snapshots"))
			if err != nil {
				return err
			}
			// snapshot metadata is now an embedded ScrivaDB store (see cstore above);
			// close it on shutdown to flush its index and stop the compaction loop.
			defer snapStore.Close()
			srv.SetSnapshots(cfg.Snapshots, snapshot.New(runner, snapStore))
			// Token-savings ledger: the store is created regardless of the gate so
			// toggling savings on later doesn't lose prior data; the gate decides
			// whether features record and GET /savings serves.
			savStore, err := savings.NewStore(filepath.Join(cfg.DataDir, "savings"))
			if err != nil {
				return err
			}
			srv.SetSavings(cfg.Tokens.Savings, savStore, cfg.Tokens.SavingsSamples)
			// Apply a previously-derived calibration factor (wd savings --calibrate)
			// so a freshly started daemon prices new events by this workload's measured
			// bytes-per-token ratio rather than the 4-bytes/token heuristic. Absent or
			// unreadable ⇒ the heuristic stands; report time refreshes it thereafter.
			if cal, ok, cerr := savStore.Calibration(); cerr == nil && ok {
				savings.SetCalibration(cal.BytesPerToken)
			}
			// Real-spend tracker: cumulative billed input+output tokens per session,
			// read from agents' transcripts, feeding the savings report's denominator.
			// Created regardless of the gate (like the ledger) so toggling savings on
			// later doesn't lose prior data; recording is gate-aware on the Server.
			spendStore, err := spend.NewStore(filepath.Join(cfg.DataDir, "spend"))
			if err != nil {
				return err
			}
			srv.SetSpend(spendStore)
			pl.OnSpend = srv.RecordSpend
			// Let the LLM-offload sites (Classify/Summarize) inside lifecycle record
			// their savings through the same gate-aware, fail-open path. Those calls run
			// off Claude entirely, so the saving is already net (cost 0); the offload
			// passes its prompt as the raw provenance sample. The Server holds the gate,
			// so the hook is safe to set unconditionally.
			lc.SavingsHook = func(feature, agent string, rawTokens, keptTokens int, rawSample, keptSample string) {
				srv.RecordLifecycleSaving(feature, agent, rawTokens, keptTokens, 0, rawSample, keptSample)
			}
			// The poller credits the auto-/compact reclaim to the same ledger: when a
			// compaction it issued lands, the reclaimed context tokens are recorded as a
			// FeatureCompact saving NET of the measured summary-generation cost, through
			// the gate-aware, fail-open hook. Context readings carry no text, so the
			// compact path passes no provenance sample.
			pl.OnSaving = func(feature, agent string, rawTokens, keptTokens, costTokens int) {
				srv.RecordLifecycleSaving(feature, agent, rawTokens, keptTokens, costTokens, "", "")
			}

			// Native scheduler (#15): opt-in (scheduler_enabled, default off). The
			// store file is created regardless so toggling the gate on doesn't lose a
			// prior schedules.json; the gate decides whether the routes + loop run.
			schedStore, err := schedule.NewStore(filepath.Join(cfg.DataDir, "schedules.json"))
			if err != nil {
				return err
			}
			defer schedStore.Close()
			srv.SetScheduler(cfg.SchedulerEnabled, schedStore, time.Minute)

			// Backend registry (docs/specs/2026-08-06-backend-registry.md): the DB
			// is the source of truth for which backends exist, their tier, and the
			// default. Reconcile a startup detection sweep into it — detection fields
			// only, preserving the user's tier/default/enabled marks — then hand it
			// to the Server. The local-model row is seeded from local_llm config
			// (configured ⇒ Installed); actual reachability probing is left to later
			// stages.
			backendStore, err := backendstore.NewStore(filepath.Join(cfg.DataDir, "backends"))
			if err != nil {
				return err
			}
			defer backendStore.Close()
			localConfigured := cfg.LocalLLM.Enabled && strings.TrimSpace(cfg.LocalLLM.URL) != ""
			if rerr := backendstore.Reconcile(backendStore, agentbackend.Detect(), localConfigured, time.Now()); rerr != nil {
				return rerr
			}
			srv.SetBackends(backendStore)
			lc.Resolver = router.NewResolver(backendStore)

			// First-class project store (docs/specs/2026-08-28-project-centric-ui.md
			// Phase 1): the parent entity agents and pipelines group under via
			// ProjectID. A fresh embedded ScrivaDB store; the /api/v1/projects routes
			// guard on it.
			projectStore, err := projectstore.NewStore(filepath.Join(cfg.DataDir, "projects"))
			if err != nil {
				return err
			}
			defer projectStore.Close()
			srv.SetProjects(projectStore)
			// Project Groups Phase 3 (peer awareness): wire the daemon-side peer-context
			// provider into lifecycle so a grouped per-project orchestrator learns its
			// Project Group and sibling orchestrators (recomputed from live store state)
			// at every fresh (re)launch. The closure holds the project + session stores,
			// keeping lifecycle store-free; it degrades to "" for any non-grouped agent.
			lc.PeerContextFn = srv.PeerContext
			if handoverSettings, err := backendStore.GetHandoverSettings(); err == nil {
				pl.HandoverEnabled = handoverSettings.Enabled
			}
			pl.OnHotSwap = func(sess *store.Session, tokens int) {
				settings, err := backendStore.GetHandoverSettings()
				if err != nil {
					settings = backendstore.DefaultHandoverSettings()
				}
				if !settings.Enabled {
					return
				}
				in := lifecycle.ThresholdInput{
					Settings:      settings,
					ContextTokens: tokens,
					ContextLimit:  cfg.Tokens.Critical,
					ContextKnown:  tokens > 0 && cfg.Tokens.Critical > 0,
				}
				if _, used, limit, _, qerr := backendStore.GetHeadroom(sess.Backend, time.Now()); qerr == nil && limit > 0 {
					in.QuotaUsed = used
					in.QuotaLimit = limit
					in.QuotaKnown = true
				}
				sig := lifecycle.DecideHotSwap(in)
				if !sig.Trigger {
					return
				}
				slog.Info("poller: triggering mid-session hot-swap", "agent", sess.ID, "reason", sig.Reason, "detail", sig.Detail)
				swapReq := lifecycle.SwapRequest{
					Role:   sess.Role,
					Reason: sig.Reason,
				}
				res, swapErr := lc.HotSwap(context.Background(), sess, swapReq)
				if swapErr != nil {
					slog.Error("hot-swap failed", "agent", sess.ID, "err", swapErr)
					return
				}
				_ = st.Update(context.Background(), sess.ID, func(s *store.Session) error {
					s.Backend = sess.Backend
					s.Model = sess.Model
					s.ClaudeSessionID = sess.ClaudeSessionID
					s.UpdatedAt = sess.UpdatedAt
					return nil
				})
				srv.Notify()
				slog.Info("hot-swap completed", "agent", sess.ID, "from_backend", res.FromBackend, "to_backend", res.ToBackend, "to_model", res.ToModel, "handoff", res.HandoffPath)
			}
			// Autopilot cost-tier ladder unification (docs/specs/
			// 2026-08-06-backend-registry.md §8): fold the deprecated
			// autopilot.brain.backends ladder + allow_pay_per_use gate into the
			// registry ONCE, on the first boot after upgrade, so the store becomes the
			// single source of truth. A sentinel guards re-runs — later user tier / gate
			// edits in the store are authoritative and never re-clobbered by config.
			apLadder := cfg.AutopilotBrainBackends()
			if ran, merr := backendstore.MigrateAutopilotLadder(
				backendStore,
				filepath.Join(cfg.DataDir, "backends", backendstore.AutopilotLadderMarker),
				apLadder.Free, apLadder.Subscription, apLadder.PayPerUse,
				cfg.AutopilotAllowPayPerUse(),
			); merr != nil {
				slog.Warn("autopilot: backend-ladder migration failed (will retry next boot)", "err", merr)
			} else if ran {
				slog.Info("autopilot: imported cost-tier ladder from config into the backend registry (store is now authoritative)")
			}
			// Internal-thinking router (docs/specs/2026-08-06-backend-registry.md
			// §7): warden's own thinking — classify / summarize / name (lifecycle),
			// digest narration, and memory curation — routes STRICTLY through the
			// registry's free-CLI-then-local candidate walk and degrades gracefully
			// when exhausted, so it NEVER makes a paid call. It is the single seam
			// replacing every prior hardcoded `claude -p` internal offload. The local
			// model (lc.LLM, nil when local_llm is off) is the terminal candidate; the
			// runner executes a free CLI backend's HeadlessCmd; backends.limit_retry
			// is the per-backend skip TTL after a rate-limit / spend signal.
			internalRouter := internalrouter.New(backendStore, lc.LLM, runner, cfg.BackendsLimitRetryDuration())
			lc.Internal = internalRouter
			// Autopilot (docs/specs/autopilot.md): construct the master-switch
			// Controller from config. S1 is inert — the switch + preflight exist on
			// every surface but no brain spawns yet. baseDir anchors relative plan
			// paths to the daemon's working directory.
			apBaseDir, _ := os.Getwd()
			apCtrl := autopilot.NewController(buildAutopilotControllerConfig(cfg, apBaseDir, lc.Resolver), nil)
			srv.SetAutopilotController(apCtrl)
			// Boot re-enable: the on/off bit is persisted per-repo, so bring every
			// previously-enabled repo back up across a daemon restart. Enable is
			// per-repo and best-effort here — a repo whose preflight now fails (e.g.
			// gh logged out) is logged and skipped rather than blocking startup; a
			// later `warden autopilot on` re-enables it. (Runtime is wired above, so
			// this respects the current inert-or-live state.)
			for _, repo := range apCtrl.PersistedEnabled() {
				if _, err := apCtrl.Enable(ctx, repo); err != nil {
					slog.Warn("autopilot: boot re-enable skipped", "repo", repo, "err", err)
				}
			}
			// Plugin system (#47): only wired when the operator opts in (plugins
			// execute external code). On a config error we log and continue with
			// plugins off rather than refusing to start the daemon. Once loaded,
			// the registry is installed into store so the closed-type-enum logic
			// (Valid/DefaultWorktree/NormalizeType) recognizes custom task types,
			// and the fail-open dispatcher is wired into the spawn/commit/check
			// lifecycle points.
			if cfg.Plugins.Enabled {
				reg, perr := plugin.Load(cfg.Plugins.Registry)
				if perr != nil {
					slog.Error("plugins: config invalid, running with plugins disabled", "err", perr)
				} else {
					store.SetCustomTypeLookup(reg.Lookup)
					srv.SetPlugins(plugin.NewDispatcher(reg))
					slog.Info("plugin system enabled", "plugins", len(reg.Plugins()))
				}
			}
			srv.SetAPIDocs(cfg.ApiDocs)
			exec := daemon.NewExecutor(pstore, st, life, cstore, srv.Notify)
			srv.SetExecutor(exec)
			// Digest narration is internal thinking too: route it through the same
			// free/local walk. On an exhausted walk Complete errors and the narrator
			// returns "" so the digest skips its summary line (never a paid call).
			srv.SetNarrator(digest.ClaudeNarrator{Run: internalRouter.Complete})
			srv.SetSpawnGate(cfg.Worktree.SpawnGate, cfg.Worktree.SpawnGateMax)
			srv.SetBudget(cfg.Tokens.BudgetGate, cfg.Tokens.BudgetDailyUSD, cfg.Tokens.BudgetWeeklyUSD)
			srv.SetWorktreeRetention(cfg.Worktree.KeepDone, cfg.Worktree.AutoPrune)
			srv.SetAudit(audit.NewWriter(filepath.Join(cfg.DataDir, "audit.jsonl")))
			srv.SetAuditTrustedProxies(trustedProxies)
			mcol := metrics.NewCollector(runner, daemon.NewAgentLister(st), srv.PressureName)
			mrec, err := metrics.NewRecorder(filepath.Join(cfg.DataDir, "metrics"))
			if err != nil {
				return err
			}
			srv.SetMetrics(mcol, mrec, cfg.MetricsEnabled, cfg.Tokens.Warn, cfg.Tokens.Critical)
			exec.SetDigestFn(srv.BuildDigest)
			exec.SetKeepDoneAgents(cfg.Pipeline.KeepDone)
			// Memory auto-curation (#53 PR-2), opt-in via memory.curate (default OFF).
			// The proposer routes through the internal-thinking router (free/local
			// candidate walk); an exhausted walk yields no proposal (Run left nil), so
			// curation never makes a paid call. The curator debounces per repo and
			// writes UNVERIFIED proposals to the working tree only — never commits.
			if cfg.Memory.Curate {
				proposer := curate.LLMProposer{
					LLM:    internalRouter,
					Record: lc.RecordOffload,
				}
				exec.SetCurator(curate.New(&memory.Store{}, proposer))
				slog.Info("memory auto-curation enabled (proposals only; committed diff is the review gate)")
			}

			// One notifier seam drives every alert channel: the platform
			// (desktop) notifier, plus the webhook when configured. It is wrapped in
			// a notify.Switch so a config hot-reload of notify.* / webhook.* can
			// rebuild the delivery chain and swap it in (buildNotifier) without
			// re-wiring the hooks below that capture it. Both the status-transition
			// hook and the context-size alert deliver through it.
			notifSwitch := notify.NewSwitch(buildNotifier(cfg))
			// Branch tracker (#44): opt-in. When enabled it fans CI failures out
			// through the same operator notifier seam (desktop + webhook) and
			// scans on the configured interval; left disabled its Run returns
			// immediately (interval 0). Wired to the switch so a notify reload
			// reaches it too (enabling/disabling the tracker itself needs a restart).
			srv.SetBranchTrackNotifier(notifSwitch)
			if cfg.BranchTrack.Enabled {
				srv.SetBranchTrackInterval(cfg.BranchTrackIntervalDuration())
			} else {
				srv.SetBranchTrackInterval(0)
			}
			notifyHook := daemon.NotifyOnTransition(notifSwitch)
			restarter := daemon.NewRestarter(life, st, cfg.AutoRestart.Max, cfg.AutoRestartResetDuration())
			rateLimitSched := daemon.NewRateLimitScheduler(life, st, cfg.RateLimitRetryIntervalDuration(), cfg.RateLimitSpendRetryIntervalDuration(), cfg.RateLimitBufferDuration(), cfg.RateLimit.AutoResume, cfg.RateLimit.ResumePrompt)
			// Fixture-capture aid: snapshot the raw pane on each real limit hit so a
			// future parser gap can be fixed from ground-truth bytes (bounded, newest-N).
			rateLimitSched.CaptureDir = filepath.Join(cfg.DataDir, "ratelimit-captures")
			// Autopilot guardian escalations (§2.3) fan out through the same
			// operator notifier seam (desktop + webhook).
			srv.SetAutopilotNotifier(notifSwitch)
			// Autopilot cost-tier selection (§7): feed the guardian's per-backend
			// limit tracking from the poller's rate-limit detection, so a limited
			// backend drops out of selection until its parsed reset (else the
			// configured retry/spend fallback) elapses and it climbs back up.
			rateLimitSched.OnLimit = func(sess *store.Session, until time.Time) {
				apCtrl.MarkBackendLimited(sess.Backend, until)
			}
			// Auto-handover on a hard rate limit (Feature #4): when handover is
			// enabled, hot-swap the limited agent to a fresh backend in the same
			// worktree instead of parking it until the limit clears. The exhausted
			// backend is marked limited in the registry FIRST so the router excludes it
			// from successor resolution; when no other backend is eligible the swap
			// fails and we return false to fall through to the normal pause/resume path.
			rateLimitSched.OnHardLimit = func(sess *store.Session, until time.Time) bool {
				settings, herr := backendStore.GetHandoverSettings()
				if herr != nil {
					settings = backendstore.DefaultHandoverSettings()
				}
				if !settings.Enabled {
					return false
				}
				// Exclude the exhausted backend from successor resolution.
				_ = backendStore.SetBackendLimited(sess.Backend, until)
				res, swapErr := lc.HotSwap(context.Background(), sess, lifecycle.SwapRequest{
					Role:   sess.Role,
					Reason: lifecycle.SwapReasonQuota,
				})
				if swapErr != nil {
					slog.Warn("rate-limit hot-swap skipped; falling back to pause/resume", "agent", sess.ID, "err", swapErr)
					return false
				}
				_ = st.Update(context.Background(), sess.ID, func(s *store.Session) error {
					s.Backend = sess.Backend
					s.Model = sess.Model
					s.ClaudeSessionID = sess.ClaudeSessionID
					s.UpdatedAt = sess.UpdatedAt
					return nil
				})
				// A different backend now drives the session: leave the rate-limited
				// state, clear the persisted limit + any pending resume timer, and let
				// the poller re-classify the fresh agent.
				_, _ = st.UpdateStatusIf(context.Background(), sess.ID, store.StatusRateLimited, store.StatusSpawning)
				_ = st.ClearRateLimit(context.Background(), sess.ID)
				rateLimitSched.CancelTimer(sess.ID)
				_ = st.AppendEvent(context.Background(), sess.ID, store.Event{
					TS:     time.Now().UTC(),
					Type:   "rate-limit-hotswap",
					Detail: "hard limit on " + res.FromBackend + "; hot-swapped to " + res.ToBackend,
				})
				srv.Notify()
				slog.Info("rate-limit hot-swap completed", "agent", sess.ID, "from_backend", res.FromBackend, "to_backend", res.ToBackend, "to_model", res.ToModel, "handoff", res.HandoffPath)
				return true
			}
			pl.OnTransition = func(sess *store.Session, from, to store.Status) {
				notifyHook(sess, from, to)
				exec.OnTransition(sess, from, to)
				restarter.OnTransition(sess, from, to)
				rateLimitSched.OnTransition(sess, from, to)
			}
			pl.OnContextAlert = func(sess *store.Session, state ctxtokens.State, tokens int) {
				title, body := daemon.ContextAlertMessage(sess, state, tokens)
				go notifSwitch.Notify(title, body)
			}
			// Anomaly alerts share the same swappable switch so a notify reload
			// reaches them too (previously a second, independent notifier).
			pl.OnAnomaly = daemon.NotifyOnAnomaly(notifSwitch)

			// Reconstruct rate limit timers from persisted state
			if err := rateLimitSched.ReconstructTimers(ctx); err != nil {
				slog.Warn("daemon: failed to reconstruct rate limit timers", "err", err)
			}

			// Config hot-reload (feature 3): re-apply ~/.warden/config.yaml live on
			// every good edit — no daemon restart. The Server's ApplyConfig fan-out
			// pushes the reloaded config into the subsystems it owns (poller
			// auto-approve + context guard, api_docs, scheduler route gate) and runs
			// these reload hooks for the rest. A BAD edit never reaches ApplyConfig:
			// the watcher keeps the last-good config, logs it, and alerts the owner.
			srv.SetBaselineConfig(cfg)
			srv.AddReloadHook(func(c config.Config) {
				// rails toggles, model_default, default permission mode, hint gates.
				lc.SetConfig(c)
			})
			srv.AddReloadHook(func(c config.Config) {
				// autopilot plan/manager/merge template + per-repo reconcile (the
				// persisted enable set is preserved — config only carries the template).
				apCtrl.Reconfigure(ctx, buildAutopilotControllerConfig(c, apBaseDir, lc.Resolver))
			})
			srv.AddReloadHook(func(c config.Config) {
				// notify.* + webhook: rebuild the delivery chain and swap it in.
				notifSwitch.Set(buildNotifier(c))
			})
			if watcher, werr := config.NewWatcher(cfgPath, config.DefaultReloadDebounce, srv.ApplyConfig, func(err error) {
				// A malformed edit: last-good config is kept; alert the owner so the
				// broken file gets fixed (mirrors the plan mid-run-edit philosophy).
				go notifSwitch.Notify("warden config", "config reload failed — keeping last-good settings: "+err.Error())
			}); werr != nil {
				slog.Warn("config: live-reload watcher disabled", "path", cfgPath, "err", werr)
			} else {
				defer watcher.Close()
				go func() {
					if err := watcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
						slog.Warn("config: live-reload watcher stopped", "err", err)
					}
				}()
				slog.Info("config: live-reload watching", "path", cfgPath)
			}

			slog.Info("warden daemon listening", "addr", cfg.Addr)
			if err := srv.ListenAndServe(ctx, cfg.Addr); err != nil {
				// Check for port already in use
				if strings.Contains(err.Error(), "address already in use") {
					return fmt.Errorf("%w\n\nPort %s already in use.\nCheck if daemon is running: ps aux | grep 'warden daemon'\nOr specify a different port: warden daemon --addr localhost:8766 (or set addr: in %s)", err, cfg.Addr, cfgPath)
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().String("log-level", "", "log verbosity: debug | info | warn | error (overrides log.level config)")
	cmd.Flags().String("log-format", "", "log output format: text | json (overrides log.format config)")
	return cmd
}

// buildAutopilotControllerConfig derives the autopilot ControllerConfig from the
// live config. It is used at boot AND by the config hot-reload hook (feature 3):
// on reload the daemon rebuilds this from the new config and calls
// Controller.Reconfigure to swap the plan/manager/merge template and re-run the
// per-repo enable reconcile. baseDir anchors relative plan paths to the daemon cwd.
// res is the daemon's shared router.Resolver (wrapping the backend registry store).
func buildAutopilotControllerConfig(cfg config.Config, baseDir string, res autopilot.Resolver) autopilot.ControllerConfig {
	return autopilot.ControllerConfig{
		Plans:             cfg.AutopilotPlanFiles(),
		IntegrationBranch: cfg.AutopilotIntegrationBranch(),
		Gate:              cfg.AutopilotGate(),
		Strategy:          cfg.AutopilotMergeStrategy(),
		DeleteBranch:      cfg.AutopilotDeleteBranch(),
		BaseDir:           baseDir,
		DataDir:           cfg.DataDir,
		Resolver:          res,
		Guardian: autopilot.GuardianParams{
			Interval:         cfg.AutopilotGuardianInterval(),
			HeartbeatTimeout: cfg.AutopilotGuardianHeartbeatTimeout(),
			BackoffMin:       cfg.AutopilotGuardianBackoffMin(),
			BackoffMax:       cfg.AutopilotGuardianBackoffMax(),
			RotateAtContext:  cfg.AutopilotGuardianRotateAtContext(),
			NotifyEach:       cfg.AutopilotGuardianNotifyEach(),
		},
	}
}

// buildNotifier assembles the operator notifier delivery chain from config: the
// platform (desktop / log) notifier, plus the webhook when configured. Used at
// boot and by the config hot-reload hook (feature 3) to rebuild the chain, which
// is then swapped into the live notify.Switch.
func buildNotifier(cfg config.Config) notify.Notifier {
	n := notify.New(cfg.Notify.Enabled)
	if cfg.Notify.WebhookEnabled && cfg.Notify.WebhookURL != "" {
		n = notify.Multi(n, notify.NewWebhook(cfg.Notify.WebhookURL))
	}
	return n
}
