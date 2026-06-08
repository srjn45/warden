package cli

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/warden/internal/config"
	"github.com/srajanpathak/warden/internal/ctxstore"
	"github.com/srajanpathak/warden/internal/daemon"
	"github.com/srajanpathak/warden/internal/digest"
	"github.com/srajanpathak/warden/internal/lifecycle"
	"github.com/srajanpathak/warden/internal/mailbox"
	"github.com/srajanpathak/warden/internal/notify"
	"github.com/srajanpathak/warden/internal/pipeline"
	"github.com/srajanpathak/warden/internal/poller"
	"github.com/srajanpathak/warden/internal/store"
)

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the warden hub (HTTP API + poller; the single writer to the file store)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()
			if a, _ := cmd.Flags().GetString("addr"); a != "" {
				cfg.Addr = a
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

			runner := lifecycle.ExecRunner{}
			lc := lifecycle.New(runner)
			lc.ProjectsDir = cfg.ClaudeProjectsDir
			// Prompt-mode agents launch in the caller's cwd; their initial prompt
			// file goes in this single shared dir (keyed by agent id), not a
			// per-agent directory.
			lc.PromptsDir = filepath.Join(cfg.DataDir, "prompts")
			life := daemon.NewLifecycleAdapter(lc, st)
			pd := daemon.NewPollerDeps(st, runner, lc)
			pl := poller.New(pd, 5*time.Minute)
			pstore, err := pipeline.NewStore(filepath.Join(cfg.DataDir, "pipelines"))
			if err != nil {
				return err
			}
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.ApprovalsEnabled, cstore, mbox, nil)
			exec := daemon.NewExecutor(pstore, st, life, cstore, srv.Notify)
			srv.SetExecutor(exec)
			srv.SetNarrator(digest.ClaudeNarrator{Run: lc.RunClaudeP})
			srv.SetSpawnGate(cfg.SpawnGateEnabled, cfg.SpawnGateMaxAgents)
			exec.SetDigestFn(srv.BuildDigest)
			exec.SetKeepDoneAgents(os.Getenv("WARDEN_PIPELINE_KEEP_DONE") != "" || os.Getenv("AGENTCTL_PIPELINE_KEEP_DONE") != "")

			notifyHook := daemon.NotifyOnTransition(notify.New(cfg.NotifyEnabled))
			pl.OnTransition = func(sess *store.Session, from, to store.Status) {
				notifyHook(sess, from, to)
				exec.OnTransition(sess, from, to)
			}
			log.Printf("warden daemon listening on %s", cfg.Addr)
			return srv.ListenAndServe(ctx, cfg.Addr)
		},
	}
}
