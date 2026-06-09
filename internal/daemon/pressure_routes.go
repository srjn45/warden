package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/pressure"
)

// pressureSampleInterval is how often the sampler refreshes the cached level.
// Cheap (one sysctl); kept short so the gauge and gate react quickly.
const pressureSampleInterval = 5 * time.Second

// pressureResponse is the body for GET /pressure (feeds the gauge + UI gating).
type pressureResponse struct {
	Level       int    `json:"level"`
	LevelName   string `json:"level_name"`
	AgentCount  int    `json:"agent_count"`
	MaxAgents   int    `json:"max_agents"`
	Elevated    bool   `json:"elevated"`
	GateEnabled bool   `json:"gate_enabled"`
}

// samplePressure refreshes the cached level once.
func (s *Server) samplePressure(ctx context.Context) {
	lvl, _ := s.life.MemoryPressure(ctx)
	s.pressMu.Lock()
	s.pressLevel = lvl
	s.pressMu.Unlock()
}

// runPressureSampler refreshes the cached level on a ticker until ctx is done.
func (s *Server) runPressureSampler(ctx context.Context) {
	s.samplePressure(ctx) // prime immediately
	t := time.NewTicker(pressureSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.samplePressure(ctx)
		}
	}
}

// liveAgentCount counts non-terminal sessions (reuses liveStatus).
func (s *Server) liveAgentCount(ctx context.Context) int {
	sessions, err := s.store.List(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, sess := range sessions {
		if liveStatus(sess.Status) {
			n++
		}
	}
	return n
}

// spawnVerdict reads the cached level + live count and evaluates the gate.
func (s *Server) spawnVerdict(ctx context.Context) pressure.Verdict {
	s.pressMu.RLock()
	lvl, max := s.pressLevel, s.spawnGateMax
	s.pressMu.RUnlock()
	return pressure.Evaluate(lvl, s.liveAgentCount(ctx), max)
}

func (s *Server) handlePressure(w http.ResponseWriter, r *http.Request) {
	s.pressMu.RLock()
	lvl, gate, max := s.pressLevel, s.spawnGate, s.spawnGateMax
	s.pressMu.RUnlock()
	if lvl == 0 {
		lvl = pressure.Normal
	}
	v := pressure.Evaluate(lvl, s.liveAgentCount(r.Context()), max)
	writeJSON(w, http.StatusOK, pressureResponse{
		Level:       int(lvl),
		LevelName:   lvl.String(),
		AgentCount:  v.AgentCount,
		MaxAgents:   max,
		Elevated:    v.Elevated,
		GateEnabled: gate,
	})
}
