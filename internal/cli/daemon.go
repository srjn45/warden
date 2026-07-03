package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/curate"
	"github.com/srjn45/warden/internal/daemon"
	"github.com/srjn45/warden/internal/digest"
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

			// ctxstore is now an embedded FileDB store with background goroutines;
			// close it on shutdown to flush its index and stop the compaction loop.
			defer cstore.Close()

			mbox, err := mailbox.New(filepath.Join(cfg.DataDir, "inbox"))
			if err != nil {
				return err
			}

			// mailbox is an embedded FileDB store (see cstore above); close on shutdown.
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
			pstore, err := pipeline.NewStore(filepath.Join(cfg.DataDir, "pipelines"))
			if err != nil {
				return err
			}
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
			srv.SetScheduler(cfg.SchedulerEnabled, schedStore, time.Minute)
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
			srv.SetNarrator(digest.ClaudeNarrator{Run: lc.RunClaudeP})
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
			// The proposer prefers the $0 local model (lc.LocalLLM), degrading to
			// headless claude -p (lc.RunClaudeP); the curator debounces per repo and
			// writes UNVERIFIED proposals to the working tree only — never commits.
			if cfg.Memory.Curate {
				proposer := curate.LLMProposer{
					Run:    lc.RunClaudeP,
					LLM:    lc.LocalLLM(),
					Record: lc.RecordOffload,
				}
				exec.SetCurator(curate.New(&memory.Store{}, proposer))
				slog.Info("memory auto-curation enabled (proposals only; committed diff is the review gate)")
			}

			// One notifier seam drives every alert channel: the platform
			// (desktop) notifier, plus the webhook when configured. Both the
			// status-transition hook and the context-size alert deliver through it.
			notifier := notify.New(cfg.Notify.Enabled)
			if cfg.Notify.WebhookEnabled && cfg.Notify.WebhookURL != "" {
				notifier = notify.Multi(notifier, notify.NewWebhook(cfg.Notify.WebhookURL))
			}
			// Branch tracker (#44): opt-in. When enabled it fans CI failures out
			// through the same operator notifier seam (desktop + webhook) and
			// scans on the configured interval; left disabled its Run returns
			// immediately (interval 0).
			if cfg.BranchTrack.Enabled {
				srv.SetBranchTrackNotifier(notifier)
				srv.SetBranchTrackInterval(cfg.BranchTrackIntervalDuration())
			} else {
				srv.SetBranchTrackInterval(0)
			}
			notifyHook := daemon.NotifyOnTransition(notifier)
			restarter := daemon.NewRestarter(life, st, cfg.AutoRestart.Max, cfg.AutoRestartResetDuration())
			rateLimitSched := daemon.NewRateLimitScheduler(life, st, cfg.RateLimitRetryIntervalDuration(), cfg.RateLimitBufferDuration(), cfg.RateLimit.AutoResume, cfg.RateLimit.ResumePrompt)
			pl.OnTransition = func(sess *store.Session, from, to store.Status) {
				notifyHook(sess, from, to)
				exec.OnTransition(sess, from, to)
				restarter.OnTransition(sess, from, to)
				rateLimitSched.OnTransition(sess, from, to)
			}
			pl.OnContextAlert = func(sess *store.Session, state ctxtokens.State, tokens int) {
				title, body := daemon.ContextAlertMessage(sess, state, tokens)
				go notifier.Notify(title, body)
			}
			pl.OnAnomaly = daemon.NotifyOnAnomaly(notify.New(cfg.Notify.Enabled))

			// Reconstruct rate limit timers from persisted state
			if err := rateLimitSched.ReconstructTimers(ctx); err != nil {
				slog.Warn("daemon: failed to reconstruct rate limit timers", "err", err)
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
