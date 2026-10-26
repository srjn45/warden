package daemon

import (
	"context"
	"fmt"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
)

func schema3AgentStore(s agentstore.AgentStore) bool {
	store, ok := s.(interface{ IsSchema3() bool })
	return ok && store.IsSchema3()
}

// spawnOwnedManager preserves existing immutable attempt identities. A live
// matching attempt may be adopted as-is; a replacement always gets a new ID.
func (rt autopilotRuntime) spawnOwnedManager(ctx context.Context, spec autopilot.BrainSpec) (autopilot.BrainHandle, error) {
	agents, err := rt.s.store.List(ctx)
	if err != nil {
		return autopilot.BrainHandle{}, err
	}
	for _, agent := range agents {
		if agent.AutopilotRunID != spec.RunID || agent.AutopilotSlot != "autopilot" {
			continue
		}
		if agent.ProjectID != spec.ProjectID || agent.PlanID != spec.PlanID {
			return autopilot.BrainHandle{}, fmt.Errorf("manager ownership disagrees with run")
		}
		if handle, live := rt.adoptSlotSession(ctx, agent); live {
			return handle, nil
		}
		if err := rt.s.life.Teardown(ctx, agent); err != nil {
			return autopilot.BrainHandle{}, err
		}
		if err := rt.s.store.Archive(ctx, agent.ID); err != nil {
			return autopilot.BrainHandle{}, err
		}
	}
	id, err := store.NewSessionID()
	if err != nil {
		return autopilot.BrainHandle{}, err
	}
	req := SpawnRequest{
		Ticket: "agent-" + id, Name: autopilot.ManagerSlotID(spec.SlotScope), Cwd: spec.Repo, Repo: spec.Repo,
		Prompt: spec.Prompt, Role: autopilotBrainRole, Backend: spec.Backend, Tags: spec.Tags,
		PlanID: spec.PlanID, ProjectID: spec.ProjectID, AutopilotRunID: spec.RunID, AutopilotSlot: "autopilot",
	}
	if code, message := rt.s.validateSpawnRequest(ctx, req); code != 0 {
		return autopilot.BrainHandle{}, fmt.Errorf("%s", message)
	}
	agent, err := rt.s.life.Spawn(ctx, req)
	if err != nil {
		return autopilot.BrainHandle{}, err
	}
	if err := rt.s.store.Insert(ctx, agent); err != nil {
		_ = rt.s.life.Teardown(ctx, agent)
		return autopilot.BrainHandle{}, err
	}
	rt.s.notify()
	return autopilot.BrainHandle{AgentID: agent.ID, Backend: agent.AiCli}, nil
}
