package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/daemon"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
)

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the warden hub (HTTP API + poller; the single writer to the file store)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := configPathFor(cmd)
			// Create/migrate the config file before loading, covering installs
			// (e.g. goreleaser tarballs) that never ran install.sh.
			if err := config.Reconcile(cfgPath); err != nil {
				log.Printf("daemon: config reconcile %s: %v", cfgPath, err)
			}
			config.WarnIfLegacyEnv(cfgPath)
			cfg := config.Load(cfgPath)
			if a, _ := cmd.Flags().GetString("addr"); a != "" {
				cfg.Addr = a
			}
			if !config.IsLoopbackHost(cfg.Addr) && !cfg.AllowNonLoopback {
				return fmt.Errorf("refusing to bind non-loopback address %q: the warden daemon has no authentication; set allow_nonloopback: true in %s to override", cfg.Addr, cfgPath)
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
			life := daemon.NewLifecycleAdapter(lc, st)
			pd := daemon.NewPollerDeps(st, runner, lc)
			pl := poller.New(pd, 5*time.Minute)
			pl.TokenGuard = cfg.TokenGuard
			pl.TokenWarn = cfg.TokenWarn
			pl.TokenCrit = cfg.TokenCritical
			pl.WarnAlert = cfg.TokenWarnAlert
			pl.AutoCompact = cfg.TokenAutoCompact
			pl.AutoApproveGlobal = cfg.AutoApproveEnabled
			pstore, err := pipeline.NewStore(filepath.Join(cfg.DataDir, "pipelines"))
			if err != nil {
				return err
			}
			if err := daemon.HardenDataDir(cfg.DataDir); err != nil {
				return err
			}
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.ApprovalsEnabled, cstore, mbox, nil)
			exec := daemon.NewExecutor(pstore, st, life, cstore, srv.Notify)
			srv.SetExecutor(exec)
			srv.SetNarrator(digest.ClaudeNarrator{Run: lc.RunClaudeP})
			srv.SetSpawnGate(cfg.SpawnGateEnabled, cfg.SpawnGateMaxAgents)
			mcol := metrics.NewCollector(runner, daemon.NewAgentLister(st), srv.PressureName)
			mrec, err := metrics.NewRecorder(filepath.Join(cfg.DataDir, "metrics"))
			if err != nil {
				return err
			}
			srv.SetMetrics(mcol, mrec, cfg.MetricsEnabled)
			exec.SetDigestFn(srv.BuildDigest)
			exec.SetKeepDoneAgents(cfg.PipelineKeepDone)

			notifyHook := daemon.NotifyOnTransition(notify.New(cfg.NotifyEnabled))
			restarter := daemon.NewRestarter(life, st, cfg.AutoRestartMax, cfg.AutoRestartResetDuration())
			rateLimitSched := daemon.NewRateLimitScheduler(life, st, cfg.RateLimitRetryIntervalDuration(), cfg.RateLimitBufferDuration(), cfg.RateLimitAutoResume, cfg.RateLimitResumePrompt)
			pl.OnTransition = func(sess *store.Session, from, to store.Status) {
				notifyHook(sess, from, to)
				exec.OnTransition(sess, from, to)
				restarter.OnTransition(sess, from, to)
				rateLimitSched.OnTransition(sess, from, to)
			}
			ctxNotifier := notify.New(cfg.NotifyEnabled)
			pl.OnContextAlert = func(sess *store.Session, state ctxtokens.State, tokens int) {
				title, body := daemon.ContextAlertMessage(sess, state, tokens)
				go ctxNotifier.Notify(title, body)
			}

			// Reconstruct rate limit timers from persisted state
			if err := rateLimitSched.ReconstructTimers(ctx); err != nil {
				log.Printf("daemon: failed to reconstruct rate limit timers: %v", err)
			}

			log.Printf("warden daemon listening on %s", cfg.Addr)
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
}
