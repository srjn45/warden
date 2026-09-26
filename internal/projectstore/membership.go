package projectstore

// Project membership helpers (docs/specs/2026-09-25-project-entity-hierarchy.md
// D2/§3.1): the authoritative agents[]/pipelines[]/terminals[] id lists live on
// the project row. Each Add/Remove is an idempotent read-modify-write on that row
// — an add de-duplicates (a repeat add is a no-op) and a remove of an absent
// member is a no-op — mirroring the group-membership helpers in groups.go.
//
// These edit only the project's own lists; they deliberately do not touch the
// reverse per-session/per-pipeline ProjectID back-ref. Keeping both ends in sync
// on user-facing spawn is a later phase (spec §6); this phase adds the fields and
// the store primitives only.

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
