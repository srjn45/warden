package planstore

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// ExportStatus is a computed view of whether the last repository replica matches
// the canonical Plan revision. It is heuristic-friendly metadata for list/detail
// surfaces — never an authority signal for execution.
type ExportStatus string

const (
	ExportStatusNone    ExportStatus = "none"    // never exported
	ExportStatusCurrent ExportStatus = "current" // last export matches revision+hash
	ExportStatusStale   ExportStatus = "stale"   // export exists but lags the DB revision
)

// TaskSummary is a compact task-progress rollup for list/detail projections.
type TaskSummary struct {
	Total      int `json:"total"`
	Done       int `json:"done"`
	InProgress int `json:"in_progress"`
	Pending    int `json:"pending"`
	Skipped    int `json:"skipped"`
}

// String returns "done/total" for compact UI labels.
func (s TaskSummary) String() string {
	return fmt.Sprintf("%d/%d", s.Done, s.Total)
}

// RelatedPlanHit is one scored overlap candidate. The score and reasons are
// heuristics for discovery — not authoritative identity or conflict detection.
type RelatedPlanHit struct {
	PlanID  string   `json:"plan_id"`
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Score   int      `json:"score"`
	Reasons []string `json:"reasons"`
}

// RelatedPlansResult wraps heuristic overlap hits with an explicit disclaimer.
type RelatedPlansResult struct {
	Heuristic  bool             `json:"heuristic"`
	Disclaimer string           `json:"disclaimer"`
	AnchorID   string           `json:"anchor_id"`
	Hits       []RelatedPlanHit `json:"hits"`
}

const relatedPlansDisclaimer = "Related-plan hits are heuristic (project, title, goal, and linked code artifacts). They are not authoritative identity, conflict, or duplicate detection."

// ComputeTaskSummary rolls up Tasks + TaskProgress into a list-friendly summary.
// Task IDs come from the canonical Tasks DAG when present; otherwise from
// TaskProgress / TaskOutcomes keys so historical plans without Tasks still show counts.
func ComputeTaskSummary(p *Plan) TaskSummary {
	if p == nil {
		return TaskSummary{}
	}
	ids := make([]string, 0, len(p.Tasks))
	seen := map[string]bool{}
	for _, t := range p.Tasks {
		id := strings.TrimSpace(t.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		for id := range p.TaskProgress {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		for id := range p.TaskOutcomes {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
	}
	var s TaskSummary
	s.Total = len(ids)
	for _, id := range ids {
		status := ""
		if p.TaskProgress != nil {
			status = p.TaskProgress[id]
		}
		if status == "" && p.TaskOutcomes != nil {
			if o, ok := p.TaskOutcomes[id]; ok {
				status = o.Status
			}
		}
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "done", "completed":
			s.Done++
		case "in_progress", "running":
			s.InProgress++
		case "skipped":
			s.Skipped++
		default:
			s.Pending++
		}
	}
	return s
}

// ComputeExportStatus derives export freshness from Plan.RepoExport vs the
// live revision/hash. Does not consult the filesystem — a deleted or stale
// YAML replica cannot change this value.
func ComputeExportStatus(p *Plan) ExportStatus {
	if p == nil || p.RepoExport == nil {
		return ExportStatusNone
	}
	ex := p.RepoExport
	if ex.Revision == p.Revision && ex.ContentHash != "" && ex.ContentHash == p.ContentHash {
		return ExportStatusCurrent
	}
	// Match on revision alone when hash is absent on older export meta.
	if ex.Revision == p.Revision && (ex.ContentHash == "" || p.ContentHash == "") {
		return ExportStatusCurrent
	}
	return ExportStatusStale
}

// ExecutorID returns the best-known live or linked executor identifier for
// list/detail projections (active execution preferred, then mode link fields).
func ExecutorID(p *Plan) string {
	if p == nil {
		return ""
	}
	if p.ActiveExecution != nil && strings.TrimSpace(p.ActiveExecution.ExecutorID) != "" {
		return p.ActiveExecution.ExecutorID
	}
	switch {
	case p.AutopilotRunID != "":
		return p.AutopilotRunID
	case p.PipelineID != "":
		return p.PipelineID
	case p.OrchestratorID != "":
		return p.OrchestratorID
	}
	return ""
}

// LinkedArtifacts collects branch names and PR URLs/numbers for overlap scoring.
func LinkedArtifacts(p *Plan) (branches map[string]bool, prs map[string]bool) {
	branches = map[string]bool{}
	prs = map[string]bool{}
	if p == nil {
		return branches, prs
	}
	for _, b := range p.Branches {
		if b = strings.TrimSpace(b); b != "" {
			branches[strings.ToLower(b)] = true
		}
	}
	for _, bs := range p.BranchSummaries {
		if n := strings.TrimSpace(bs.Name); n != "" {
			branches[strings.ToLower(n)] = true
		}
		if bs.PR != nil {
			addPRKeys(prs, *bs.PR)
		}
	}
	for _, o := range p.TaskOutcomes {
		if b := strings.TrimSpace(o.Branch); b != "" {
			branches[strings.ToLower(b)] = true
		}
		for _, pr := range o.PullRequests {
			addPRKeys(prs, pr)
		}
	}
	return branches, prs
}

func addPRKeys(prs map[string]bool, pr PullRequestSummary) {
	if u := strings.TrimSpace(pr.URL); u != "" {
		prs[strings.ToLower(u)] = true
	}
	if pr.Number > 0 {
		prs[fmt.Sprintf("#%d", pr.Number)] = true
	}
}

// FindRelatedPlans scores candidates against anchor using project membership,
// title/name tokens, goal tokens, and linked code artifacts. Results are sorted
// by descending score (then name). The returned disclaimer must travel with the
// hits on every surface — this is not authoritative duplicate detection.
//
// Candidates outside the anchor's project are ignored. The anchor itself is
// excluded. Completed/archived plans remain eligible so historical capability
// lookup works. limit <= 0 defaults to 10.
func FindRelatedPlans(anchor *Plan, candidates []*Plan, limit int) RelatedPlansResult {
	out := RelatedPlansResult{
		Heuristic:  true,
		Disclaimer: relatedPlansDisclaimer,
		Hits:       []RelatedPlanHit{},
	}
	if anchor == nil {
		return out
	}
	out.AnchorID = anchor.ID
	if limit <= 0 {
		limit = 10
	}
	anchorName := tokenize(anchor.Name)
	anchorGoal := tokenize(anchor.Goal)
	anchorBranches, anchorPRs := LinkedArtifacts(anchor)

	type scored struct {
		hit RelatedPlanHit
	}
	var ranked []scored
	for _, c := range candidates {
		if c == nil || c.ID == "" || c.ID == anchor.ID {
			continue
		}
		if c.ProjectID != "" && anchor.ProjectID != "" && c.ProjectID != anchor.ProjectID {
			continue
		}
		var reasons []string
		score := 0
		// Same project is the primary scope signal.
		if c.ProjectID != "" && c.ProjectID == anchor.ProjectID {
			score += 10
			reasons = append(reasons, "same_project")
		}
		if n := tokenOverlap(anchorName, tokenize(c.Name)); n > 0 {
			score += 5 * n
			reasons = append(reasons, fmt.Sprintf("title_overlap:%d", n))
		}
		if n := tokenOverlap(anchorGoal, tokenize(c.Goal)); n > 0 {
			score += 3 * n
			reasons = append(reasons, fmt.Sprintf("goal_overlap:%d", n))
		}
		cBranches, cPRs := LinkedArtifacts(c)
		if n := setOverlap(anchorBranches, cBranches); n > 0 {
			score += 8 * n
			reasons = append(reasons, fmt.Sprintf("branch_overlap:%d", n))
		}
		if n := setOverlap(anchorPRs, cPRs); n > 0 {
			score += 8 * n
			reasons = append(reasons, fmt.Sprintf("pr_overlap:%d", n))
		}
		if score <= 10 && len(reasons) <= 1 {
			// Only same_project with no other signal — skip noise.
			continue
		}
		ranked = append(ranked, scored{hit: RelatedPlanHit{
			PlanID:  c.ID,
			Name:    c.Name,
			Status:  string(c.Status),
			Score:   score,
			Reasons: reasons,
		}})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].hit.Score != ranked[j].hit.Score {
			return ranked[i].hit.Score > ranked[j].hit.Score
		}
		return ranked[i].hit.Name < ranked[j].hit.Name
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out.Hits = make([]RelatedPlanHit, len(ranked))
	for i, r := range ranked {
		out.Hits[i] = r.hit
	}
	return out
}

// FormatPlanDefinition renders the canonical Plan definition for planning-agent
// prompts. It never reads repository YAML — FilePath/RepoExport are mentioned
// only as optional last-export metadata.
func FormatPlanDefinition(p *Plan) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan ID: %s\n", p.ID)
	fmt.Fprintf(&b, "Name: %s\n", p.Name)
	fmt.Fprintf(&b, "Status: %s\n", p.Status)
	fmt.Fprintf(&b, "Revision: %d\n", p.Revision)
	if p.ContentHash != "" {
		fmt.Fprintf(&b, "Content-Hash: %s\n", p.ContentHash)
	}
	fmt.Fprintf(&b, "Updated: %s\n", p.UpdatedAt.UTC().Format(time.RFC3339))
	if exec := ExecutorID(p); exec != "" {
		fmt.Fprintf(&b, "Executor: %s\n", exec)
	}
	ts := ComputeTaskSummary(p)
	fmt.Fprintf(&b, "Tasks: %s done\n", ts.String())
	fmt.Fprintf(&b, "Export-Status: %s\n", ComputeExportStatus(p))
	if p.RepoExport != nil && p.RepoExport.FilePath != "" {
		fmt.Fprintf(&b, "Last-Export-Path: %s (inert replica; not authority)\n", p.RepoExport.FilePath)
	} else if p.FilePath != "" {
		fmt.Fprintf(&b, "Last-Export-Path: %s (inert replica; not authority)\n", p.FilePath)
	}
	if p.Goal != "" {
		fmt.Fprintf(&b, "\nGoal:\n%s\n", p.Goal)
	}
	if len(p.Constraints) > 0 {
		b.WriteString("\nConstraints:\n")
		for _, c := range p.Constraints {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if len(p.DoneWhen) > 0 {
		b.WriteString("\nDone when:\n")
		for _, d := range p.DoneWhen {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}
	if len(p.Tasks) > 0 {
		b.WriteString("\nTasks:\n")
		for _, t := range p.Tasks {
			line := fmt.Sprintf("- %s: %s", t.ID, t.Prompt)
			if len(t.After) > 0 {
				line += " (after " + strings.Join(t.After, ", ") + ")"
			}
			b.WriteString(line + "\n")
		}
	}
	if len(p.Branches) > 0 {
		fmt.Fprintf(&b, "\nBranches: %s\n", strings.Join(p.Branches, ", "))
	}
	return b.String()
}

// FormatRelatedPlansContext appends a short related-plan block for agent prompts.
func FormatRelatedPlansContext(res RelatedPlansResult) string {
	if len(res.Hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nRelated plans (heuristic, not authoritative):\n")
	b.WriteString(res.Disclaimer + "\n")
	for _, h := range res.Hits {
		fmt.Fprintf(&b, "- %s (%s) score=%d reasons=%s\n",
			h.PlanID, h.Name, h.Score, strings.Join(h.Reasons, ","))
	}
	return b.String()
}

func tokenize(s string) map[string]bool {
	out := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		tok := strings.ToLower(cur.String())
		cur.Reset()
		if len(tok) < 2 {
			return
		}
		switch tok {
		case "the", "and", "for", "with", "from", "into", "plan", "that", "this", "a", "an", "of", "to", "in", "on":
			return
		}
		out[tok] = true
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func tokenOverlap(a, b map[string]bool) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := 0
	for t := range a {
		if b[t] {
			n++
		}
	}
	return n
}

func setOverlap(a, b map[string]bool) int {
	return tokenOverlap(a, b)
}
