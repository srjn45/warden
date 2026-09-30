package projectstore

import "errors"

// Project membership helpers (docs/specs/2026-09-25-project-entity-hierarchy.md
// D2/§3.1 and docs/specs/2026-09-29-plan-execution-entity-redesign.md Rule 4):
// the authoritative agents[]/pipelines[]/terminals[]/plans[]/autopilots[] id
// lists live on the project row. Each Add/Remove/Reparent is an idempotent
// read-modify-write on that row — an add de-duplicates (a repeat add is a
// no-op) and a remove of an absent member is a no-op — mirroring the
// group-membership helpers in groups.go.
//
// These edit only the project's own lists; they deliberately do not touch the
// reverse per-session/per-pipeline/per-plan ProjectID back-ref. Keeping both
// ends in sync on user-facing spawn is the daemon's job (spec §6).

// addMember appends memberID to the list returned by get(field) on the project
// keyed projectID, de-duplicated, and persists. It is the shared RMW body of the
// three Add* helpers. Returns ErrInvalidID for a blank member id, ErrNotFound if
// the project is absent, else the updated project.
func (s *Store) addMember(projectID, memberID string, get func(*Project) *[]string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if memberID == "" {
		return Project{}, ErrInvalidID
	}
	p, err := s.get(projectID)
	if err != nil {
		return Project{}, err
	}
	list := get(&p)
	*list = dedupeIDs(append(*list, memberID))
	if err := s.upsert(p); err != nil {
		return Project{}, err
	}
	return s.get(projectID)
}

// removeMember drops every occurrence of memberID from the list returned by
// get(field) on the project keyed projectID and persists. Removing an absent
// member is a no-op (not an error). Returns ErrNotFound if the project is absent,
// else the updated project.
func (s *Store) removeMember(projectID, memberID string, get func(*Project) *[]string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(projectID)
	if err != nil {
		return Project{}, err
	}
	list := get(&p)
	filtered := (*list)[:0:0]
	for _, id := range *list {
		if id != memberID {
			filtered = append(filtered, id)
		}
	}
	*list = filtered
	if err := s.upsert(p); err != nil {
		return Project{}, err
	}
	return s.get(projectID)
}

// AddAgentToProject appends agentID to Project.Agents (RMW, de-duplicated so a
// repeat add is a no-op). Returns ErrInvalidID for a blank id, ErrNotFound if the
// project is absent, else the updated project.
func (s *Store) AddAgentToProject(projectID, agentID string) (Project, error) {
	return s.addMember(projectID, agentID, func(p *Project) *[]string { return &p.Agents })
}

// RemoveAgentFromProject drops agentID from Project.Agents (RMW). Removing an
// absent member is a no-op. Returns ErrNotFound if the project is absent, else the
// updated project.
func (s *Store) RemoveAgentFromProject(projectID, agentID string) (Project, error) {
	return s.removeMember(projectID, agentID, func(p *Project) *[]string { return &p.Agents })
}

// AddPipelineToProject appends pipelineID to Project.Pipelines (RMW, de-duplicated
// so a repeat add is a no-op). Returns ErrInvalidID for a blank id, ErrNotFound if
// the project is absent, else the updated project.
func (s *Store) AddPipelineToProject(projectID, pipelineID string) (Project, error) {
	return s.addMember(projectID, pipelineID, func(p *Project) *[]string { return &p.Pipelines })
}

// RemovePipelineFromProject drops pipelineID from Project.Pipelines (RMW).
// Removing an absent member is a no-op. Returns ErrNotFound if the project is
// absent, else the updated project.
func (s *Store) RemovePipelineFromProject(projectID, pipelineID string) (Project, error) {
	return s.removeMember(projectID, pipelineID, func(p *Project) *[]string { return &p.Pipelines })
}

// AddPlanToProject appends planID to Project.Plans (RMW, de-duplicated so a
// repeat add is a no-op). Returns ErrInvalidID for a blank id, ErrNotFound if
// the project is absent, else the updated project.
func (s *Store) AddPlanToProject(projectID, planID string) (Project, error) {
	return s.addMember(projectID, planID, func(p *Project) *[]string { return &p.Plans })
}

// RemovePlanFromProject drops planID from Project.Plans (RMW). Removing an
// absent member is a no-op. Returns ErrNotFound if the project is absent, else
// the updated project.
func (s *Store) RemovePlanFromProject(projectID, planID string) (Project, error) {
	return s.removeMember(projectID, planID, func(p *Project) *[]string { return &p.Plans })
}

// AddAutopilotToProject appends runID to Project.Autopilots (RMW, de-duplicated
// so a repeat add is a no-op). Returns ErrInvalidID for a blank id, ErrNotFound
// if the project is absent, else the updated project.
func (s *Store) AddAutopilotToProject(projectID, runID string) (Project, error) {
	return s.addMember(projectID, runID, func(p *Project) *[]string { return &p.Autopilots })
}

// RemoveAutopilotFromProject drops runID from Project.Autopilots (RMW). Removing
// an absent member is a no-op. Returns ErrNotFound if the project is absent, else
// the updated project.
func (s *Store) RemoveAutopilotFromProject(projectID, runID string) (Project, error) {
	return s.removeMember(projectID, runID, func(p *Project) *[]string { return &p.Autopilots })
}

// AddTerminalToProject appends terminalID to Project.Terminals (RMW,
// de-duplicated so a repeat add is a no-op). Returns ErrInvalidID for a blank id,
// ErrNotFound if the project is absent, else the updated project.
func (s *Store) AddTerminalToProject(projectID, terminalID string) (Project, error) {
	return s.addMember(projectID, terminalID, func(p *Project) *[]string { return &p.Terminals })
}

// RemoveTerminalFromProject drops terminalID from Project.Terminals (RMW).
// Removing an absent member is a no-op. Returns ErrNotFound if the project is
// absent, else the updated project.
func (s *Store) RemoveTerminalFromProject(projectID, terminalID string) (Project, error) {
	return s.removeMember(projectID, terminalID, func(p *Project) *[]string { return &p.Terminals })
}

// reparentMember moves memberID from fromProjectID to toProjectID under one
// lock so the detach+attach is atomic. Semantics:
//   - blank memberID → ErrInvalidID
//   - from == to (and non-empty) → ensure membership on that project (add)
//   - empty from → add to to only (attach of a project-less member)
//   - empty to → remove from from only (detach to project-less)
//   - missing from project / absent member → tolerated (dangling-id recovery)
//   - missing to project (when to is non-empty) → ErrNotFound, source untouched
//
// Order is add-then-remove so a crash mid-way never orphans the member.
func (s *Store) reparentMember(memberID, fromProjectID, toProjectID string, get func(*Project) *[]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if memberID == "" {
		return ErrInvalidID
	}
	drop := func(list *[]string) {
		filtered := (*list)[:0:0]
		for _, id := range *list {
			if id != memberID {
				filtered = append(filtered, id)
			}
		}
		*list = filtered
	}
	add := func(list *[]string) { *list = dedupeIDs(append(*list, memberID)) }

	if fromProjectID != "" && fromProjectID == toProjectID {
		p, err := s.get(fromProjectID)
		if err != nil {
			return err
		}
		add(get(&p))
		return s.upsert(p)
	}

	var (
		to       Project
		haveTo   bool
		from     Project
		haveFrom bool
	)
	if toProjectID != "" {
		p, err := s.get(toProjectID)
		if err != nil {
			return err
		}
		to, haveTo = p, true
	}
	if fromProjectID != "" {
		p, err := s.get(fromProjectID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			from, haveFrom = p, true
		}
	}
	if haveTo {
		add(get(&to))
		if err := s.upsert(to); err != nil {
			return err
		}
	}
	if haveFrom {
		drop(get(&from))
		if err := s.upsert(from); err != nil {
			return err
		}
	}
	return nil
}

// ReparentAgent moves agentID from fromProjectID to toProjectID (atomic RMW).
func (s *Store) ReparentAgent(agentID, fromProjectID, toProjectID string) error {
	return s.reparentMember(agentID, fromProjectID, toProjectID, func(p *Project) *[]string { return &p.Agents })
}

// ReparentPipeline moves pipelineID from fromProjectID to toProjectID (atomic RMW).
func (s *Store) ReparentPipeline(pipelineID, fromProjectID, toProjectID string) error {
	return s.reparentMember(pipelineID, fromProjectID, toProjectID, func(p *Project) *[]string { return &p.Pipelines })
}

// ReparentTerminal moves terminalID from fromProjectID to toProjectID (atomic RMW).
func (s *Store) ReparentTerminal(terminalID, fromProjectID, toProjectID string) error {
	return s.reparentMember(terminalID, fromProjectID, toProjectID, func(p *Project) *[]string { return &p.Terminals })
}

// ReparentPlan moves planID from fromProjectID to toProjectID (atomic RMW).
func (s *Store) ReparentPlan(planID, fromProjectID, toProjectID string) error {
	return s.reparentMember(planID, fromProjectID, toProjectID, func(p *Project) *[]string { return &p.Plans })
}

// ReparentAutopilot moves runID from fromProjectID to toProjectID (atomic RMW).
func (s *Store) ReparentAutopilot(runID, fromProjectID, toProjectID string) error {
	return s.reparentMember(runID, fromProjectID, toProjectID, func(p *Project) *[]string { return &p.Autopilots })
}
