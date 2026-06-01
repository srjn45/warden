package daemon

import (
	"context"
	"net/http"

	"github.com/srajanpathak/agentctl/internal/store"
)

func NewServer(st store.Store, life Lifecycle) *Server {
	return &Server{store: st, life: life}
}

// ListenAndServe blocks serving the API on addr until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
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
