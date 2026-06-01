package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/srajanpathak/agentctl/internal/poller"
	"github.com/srajanpathak/agentctl/internal/store"
)

func NewServer(st store.Store, life Lifecycle, p *poller.Poller, interval time.Duration) *Server {
	h := newHub()
	if p != nil {
		p.OnChange = h.publish
	}
	return &Server{store: st, life: life, poller: p, pollInterval: interval, hub: h}
}

// ListenAndServe blocks serving the API on addr until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if s.poller != nil {
		go s.poller.Run(ctx, s.pollInterval)
	}
	httpSrv := &http.Server{Addr: addr, Handler: s.router()}
	go func() {
		<-ctx.Done()
		_ = httpSrv.Shutdown(context.Background())
	}()
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
