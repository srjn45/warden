package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
)

// GetStoreHealth implements GET /api/v1/store/health. It probes the active
// session store by attempting a complete read and reports the verdict in the
// body (always 200 — a degraded store is not a failed request, so a monitor can
// tell "store degraded" apart from "daemon unreachable"). A degraded scan yields
// healthy=false with structured per-failure diagnostics; a clean scan yields
// healthy=true with an empty failure list.
func (s *Server) GetStoreHealth(ctx context.Context, _ oapi.GetStoreHealthRequestObject) (oapi.GetStoreHealthResponseObject, error) {
	now := time.Now().UTC()
	_, err := s.store.List(ctx)
	if err == nil {
		return oapi.GetStoreHealth200JSONResponse{
			Healthy:      true,
			Degraded:     false,
			FailureCount: 0,
			Failures:     []oapi.StoreScanFailure{},
			CheckedAt:    now,
		}, nil
	}

	// A degraded active scan carries per-record diagnostics; any other read error
	// is reported as a single whole-scan "read" failure so the endpoint always has
	// a concrete verdict rather than bubbling a 500.
	var failures []oapi.StoreScanFailure
	if d, ok := store.IsDegraded(err); ok {
		failures = degradedFailuresToWire(d)
	} else {
		failures = []oapi.StoreScanFailure{{
			Collection: "active",
			Class:      oapi.Read,
			Detail:     err.Error(),
		}}
	}
	return oapi.GetStoreHealth200JSONResponse{
		Healthy:      false,
		Degraded:     true,
		FailureCount: len(failures),
		Failures:     failures,
		CheckedAt:    now,
	}, nil
}

// degradedFailuresToWire maps the store's typed degradation diagnostics onto the
// generated wire type.
func degradedFailuresToWire(d *store.DegradedScanError) []oapi.StoreScanFailure {
	out := make([]oapi.StoreScanFailure, 0, len(d.Failures))
	for _, f := range d.Failures {
		class := oapi.Decode
		if f.Class == store.DegradeRead {
			class = oapi.Read
		}
		out = append(out, oapi.StoreScanFailure{
			Collection: f.Collection,
			Key:        f.Key,
			Class:      class,
			Detail:     f.Detail,
		})
	}
	return out
}

// degradedLogThrottle rate-limits the "store degraded" warning so the 1-second
// fleet poll (and any burst of list traffic) cannot flood the log while the
// store stays degraded. One line per throttle window is enough to alert an
// operator; the store-health endpoint carries the full detail on demand.
var degradedLogThrottle = struct {
	sync.Mutex
	last time.Time
}{}

const degradedLogInterval = 30 * time.Second

// logStoreDegraded emits a single throttled warning for a degraded active scan.
func logStoreDegraded(d *store.DegradedScanError) {
	degradedLogThrottle.Lock()
	now := time.Now()
	if now.Sub(degradedLogThrottle.last) < degradedLogInterval {
		degradedLogThrottle.Unlock()
		return
	}
	degradedLogThrottle.last = now
	degradedLogThrottle.Unlock()
	slog.Warn("session store degraded: active fleet read is incomplete",
		"failures", len(d.Failures), "detail", d.Error())
}
