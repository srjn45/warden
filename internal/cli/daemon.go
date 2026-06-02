package cli

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/config"
	"github.com/srajanpathak/agentctl/internal/daemon"
	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/poller"
	"github.com/srajanpathak/agentctl/internal/store"
)

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the agentctl hub (HTTP API + poller; the single writer to the file store)",
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

			if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
				return err
			}
			runner := lifecycle.ExecRunner{}
			lc := lifecycle.New(runner)
			lc.ProjectsDir = cfg.ClaudeProjectsDir
			life := daemon.NewLifecycleAdapter(lc, st)
			pd := daemon.NewPollerDeps(st, runner, lc)
			pl := poller.New(pd, 5*time.Minute)
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.Workdir)
			log.Printf("agentctl daemon listening on %s", cfg.Addr)
			return srv.ListenAndServe(ctx, cfg.Addr)
		},
	}
}
