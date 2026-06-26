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
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/daemon"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/logging"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/plugin"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/store"
)

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
			level, format := cfg.LogLevel, cfg.LogFormat
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
			if !config.IsLoopbackHost(cfg.Addr) && authToken == "" && !cfg.AllowNonLoopback {
				return fmt.Errorf("refusing to bind non-loopback address %q without authentication: set %s (run `warden token generate`) to require a bearer token, or set allow_nonloopback: true in %s to bind without auth (not recommended)", cfg.Addr, auth.TokenEnv, cfgPath)
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

			mbox, err := mailbox.New(filepath.Join(cfg.DataDir, "inbox"))
			if err != nil {
				return err
			}

			runner := lifecycle.HintingExecRunner{Inner: lifecycle.ExecRunner{}}
			lc := lifecycle.New(runner, cfg)
			lc.ProjectsDir = cfg.ClaudeProjectsDir
			// Prompt-mode agents launch in the caller's cwd; their initial prompt
			// file goes in this single shared dir (keyed by agent id), not a
			// per-agent directory.
			lc.PromptsDir = filepath.Join(cfg.DataDir, "prompts")
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
			if cfg.LocalLLM {
				lc.LLM = llm.NewOllama(cfg.LocalLLMURL, cfg.LocalLLMModel, cfg.LocalLLMTimeoutDuration())
				slog.Info("local LLM enabled", "url", cfg.LocalLLMURL, "model", cfg.LocalLLMModel)
			}
			life := daemon.NewLifecycleAdapter(lc, st)
			pd := daemon.NewPollerDeps(st, runner, lc)
			pl := poller.New(pd, 5*time.Minute)
			pl.TokenGuard = cfg.TokenGuard
			pl.TokenWarn = cfg.TokenWarn
			pl.TokenCrit = cfg.TokenCritical
			pl.WarnAlert = cfg.TokenWarnAlert
			pl.AutoCompact = cfg.TokenAutoCompact
			pl.AutoApprovePolicy = cfg.AutoApprove
			pstore, err := pipeline.NewStore(filepath.Join(cfg.DataDir, "pipelines"))
			if err != nil {
				return err
			}
			if err := daemon.HardenDataDir(cfg.DataDir); err != nil {
				return err
			}
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.ApprovalsEnabled, cstore, mbox, nil)
			srv.SetAuth(authToken)
			if cfg.CollabEnabled {
				srv.SetCollabInterval(cfg.CollabIntervalDuration())
			} else {
				srv.SetCollabInterval(0)
			}
			snapStore, err := snapshot.NewStore(filepath.Join(cfg.DataDir, "snapshots"))
			if err != nil {
				return err
			}
			srv.SetSnapshots(cfg.Snapshots, snapshot.New(runner, snapStore))
			// Plugin system (#47): only wired when the operator opts in (plugins
			// execute external code). On a config error we log and continue with
			// plugins off rather than refusing to start the daemon. Once loaded,
			// the registry is installed into store so the closed-type-enum logic
			// (Valid/DefaultWorktree/NormalizeType) recognizes custom task types,
			// and the fail-open dispatcher is wired into the spawn/commit/check
			// lifecycle points.
			if cfg.PluginsEnabled {
				reg, perr := plugin.Load(cfg.Plugins)
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
			srv.SetSpawnGate(cfg.SpawnGateEnabled, cfg.SpawnGateMaxAgents)
			srv.SetWorktreeRetention(cfg.WorktreeKeepDone, cfg.WorktreeAutoPrune)
			srv.SetAudit(audit.NewWriter(filepath.Join(cfg.DataDir, "audit.jsonl")))
			mcol := metrics.NewCollector(runner, daemon.NewAgentLister(st), srv.PressureName)
			mrec, err := metrics.NewRecorder(filepath.Join(cfg.DataDir, "metrics"))
			if err != nil {
				return err
			}
			srv.SetMetrics(mcol, mrec, cfg.MetricsEnabled, cfg.TokenWarn, cfg.TokenCritical)
			exec.SetDigestFn(srv.BuildDigest)
			exec.SetKeepDoneAgents(cfg.PipelineKeepDone)

			// One notifier seam drives every alert channel: the platform
			// (desktop) notifier, plus the webhook when configured. Both the
			// status-transition hook and the context-size alert deliver through it.
			notifier := notify.New(cfg.NotifyEnabled)
			if cfg.WebhookEnabled && cfg.WebhookURL != "" {
				notifier = notify.Multi(notifier, notify.NewWebhook(cfg.WebhookURL))
			}
			// Branch tracker (#44): opt-in. When enabled it fans CI failures out
			// through the same operator notifier seam (desktop + webhook) and
			// scans on the configured interval; left disabled its Run returns
			// immediately (interval 0).
			if cfg.BranchTrackEnabled {
				srv.SetBranchTrackNotifier(notifier)
				srv.SetBranchTrackInterval(cfg.BranchTrackIntervalDuration())
			} else {
				srv.SetBranchTrackInterval(0)
			}
			notifyHook := daemon.NotifyOnTransition(notifier)
			restarter := daemon.NewRestarter(life, st, cfg.AutoRestartMax, cfg.AutoRestartResetDuration())
			rateLimitSched := daemon.NewRateLimitScheduler(life, st, cfg.RateLimitRetryIntervalDuration(), cfg.RateLimitBufferDuration(), cfg.RateLimitAutoResume, cfg.RateLimitResumePrompt)
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
			pl.OnAnomaly = daemon.NotifyOnAnomaly(notify.New(cfg.NotifyEnabled))

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
	cmd.Flags().String("log-level", "", "log verbosity: debug | info | warn | error (overrides log_level config)")
	cmd.Flags().String("log-format", "", "log output format: text | json (overrides log_format config)")
	return cmd
}
