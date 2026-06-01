package cli

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/config"
	"github.com/srajanpathak/agentctl/internal/daemon"
	"github.com/srajanpathak/agentctl/internal/store"
)

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the agentctl hub (HTTP API + poller; the single Mongo writer)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()
			if a, _ := cmd.Flags().GetString("addr"); a != "" {
				cfg.Addr = a
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			st, err := store.NewMongoStore(ctx, cfg.MongoURI, cfg.DB)
			if err != nil {
				return err
			}
			defer st.Close(context.Background())

			// life and poller are wired in Phase 4 & 5; nil is tolerated by read routes.
			srv := daemon.NewServer(st, nil)
			log.Printf("agentctl daemon listening on %s", cfg.Addr)
			return srv.ListenAndServe(ctx, cfg.Addr)
		},
	}
}
