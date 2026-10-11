package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/srjn45/warden/internal/wardendb"
)

// ErrStagingExportRefused means the legacy input cannot be mapped without
// guessing, merging, overwriting, or violating the schema-2 relationship rules.
var ErrStagingExportRefused = errors.New("migrate: schema-2 staging export refused")

// StagingExportOptions configures ExportSchema2Staging.
type StagingExportOptions struct {
	// TargetDir defaults to <dataDir>/warden-db-staging. It is deliberately not
	// wardendb.Dir(dataDir): staging is review material, never cutover.
	TargetDir string
	Now       time.Time
}

// StagingExportReport is the deterministic summary of one staging export run.
type StagingExportReport struct {
	DataDir   string            `json:"data_dir"`
	TargetDir string            `json:"target_dir"`
	StartedAt time.Time         `json:"started_at"`
	Records   map[string]int    `json:"records"`
	Findings  []Finding         `json:"findings,omitempty"`
	Checksums map[string]string `json:"checksums"`
	Blocked   bool              `json:"blocked"`
}

type stagingRow struct {
	collection string
	key        string
	body       map[string]any
}

type stagingSet struct {
	rows     []stagingRow
	agents   map[string]map[string]any
	terms    map[string]map[string]any
	projects map[string]map[string]any
	pipes    map[string]map[string]any
	jobs     map[string]map[string]any
	findings []Finding
}

// ExportSchema2Staging maps the legacy schema-1 stores into a fresh schema-2
// staging directory. It never writes legacy sources and never advances the
// schema ledger. A rerun against the same target is a no-op if every existing
// target record is byte-identical; divergent target records refuse.
func ExportSchema2Staging(ctx context.Context, dataDir string, opts StagingExportOptions) (*StagingExportReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.TargetDir == "" {
		opts.TargetDir = filepath.Join(dataDir, "warden-db-staging")
	}
	rep := &StagingExportReport{
		DataDir: dataDir, TargetDir: opts.TargetDir, StartedAt: opts.Now.UTC(),
		Records: map[string]int{}, Checksums: map[string]string{},
	}
	if opts.TargetDir == wardendb.Dir(dataDir) {
		rep.Blocked = true
		rep.Findings = append(rep.Findings, Finding{Severity: SeverityBlocking, Class: string(ClassUnsupportedLayout), Store: wardendb.DirName, Message: "staging export target must not be the cutover warden-db directory"})
		return rep, fmt.Errorf("%w: target is cutover directory", ErrStagingExportRefused)
	}

	set, err := buildStagingSet(ctx, dataDir)
	if err != nil {
		return rep, err
	}
	rep.Findings = append(rep.Findings, set.findings...)
	for _, f := range rep.Findings {
		if f.Severity == SeverityBlocking {
			rep.Blocked = true
		}
	}
	if rep.Blocked {
		return rep, fmt.Errorf("%w: %d blocking finding(s)", ErrStagingExportRefused, countBlocking(rep.Findings))
	}

	if err := os.MkdirAll(opts.TargetDir, 0o700); err != nil {
		return rep, err
	}
	db, err := wardendb.OpenOfflineStagingDir(opts.TargetDir)
	if err != nil {
		return rep, err
	}
	defer db.Close()

	byCollection := map[string][]stagingRow{}
	for _, row := range set.rows {
		byCollection[row.collection] = append(byCollection[row.collection], row)
	}
	for _, name := range sortedKeys(byCollection) {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		col, err := db.Collection(name, scriva.WithUniqueIndex("id"))
		if err != nil {
			return rep, err
		}
		if err := ensureStagingIndexes(col, name); err != nil {
			return rep, err
		}
		rows := byCollection[name]
		sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
		h := sha256.New()
		for _, row := range rows {
			body := cloneMap(row.body)
			delete(body, "_key")
			if ex, err := col.GetByKey(row.key); err == nil {
				if !canonicalEqual(ex.Data, body) {
					rep.Blocked = true
					rep.Findings = append(rep.Findings, Finding{Severity: SeverityBlocking, Class: string(ClassAmbiguity), Store: name, Message: row.key + ": existing staging record differs; refusing to overwrite"})
					return rep, fmt.Errorf("%w: target %s/%s differs", ErrStagingExportRefused, name, row.key)
				}
			} else if errors.Is(err, engine.ErrKeyNotFound) {
				if _, _, err := col.InsertWithKey(row.key, body); err != nil {
					return rep, err
				}
			} else {
				return rep, err
			}
			raw, err := json.Marshal(body)
			if err != nil {
				return rep, err
			}
			_, _ = h.Write([]byte(row.key))
			_, _ = h.Write(raw)
			rep.Records[name]++
		}
		rep.Checksums[name] = hex.EncodeToString(h.Sum(nil))
	}
	return rep, nil
}

func buildStagingSet(ctx context.Context, dataDir string) (*stagingSet, error) {
	set := &stagingSet{
		agents: map[string]map[string]any{}, terms: map[string]map[string]any{},
		projects: map[string]map[string]any{}, pipes: map[string]map[string]any{},
		jobs: map[string]map[string]any{},
	}
	if err := loadProjects(dataDir, set); err != nil {
		return nil, err
	}
	if err := loadAgents(ctx, dataDir, set); err != nil {
		return nil, err
	}
	if err := loadTerminals(ctx, dataDir, set); err != nil {
		return nil, err
	}
	if err := loadPipelines(dataDir, set); err != nil {
		return nil, err
	}
	validateStagingRelationships(set)
	return set, nil
}

func loadProjects(dataDir string, set *stagingSet) error {
	db, err := openIfExists(filepath.Join(dataDir, "projects"))
	if err != nil || db == nil {
		return err
	}
	defer db.Close()
	groups, err := scanCollection[projectstore.ProjectGroup](filepath.Join(dataDir, "projects"), db, "project_groups")
	if err != nil {
		return err
	}
	groupFor := map[string]struct {
		id    string
		order int
		at    time.Time
	}{}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].CreatedAt.Equal(groups[j].CreatedAt) {
			return groups[i].ID < groups[j].ID
		}
		return groups[i].CreatedAt.Before(groups[j].CreatedAt)
	})
	for _, g := range groups {
		row, err := toMap(g)
		if err != nil {
			return err
		}
		delete(row, "project_ids")
		set.add("project_groups", g.ID, row)
		for i, pid := range g.ProjectIDs {
			if pid == "" {
				continue
			}
			if prior, ok := groupFor[pid]; ok {
				set.findings = append(set.findings, Finding{Severity: SeverityRepairable, Class: string(ClassAmbiguity), Store: "project_groups", Message: fmt.Sprintf("%s appears in both %s and %s; keeping earliest group", pid, prior.id, g.ID)})
				continue
			}
			groupFor[pid] = struct {
				id    string
				order int
				at    time.Time
			}{g.ID, i, g.CreatedAt}
		}
	}
	projects, err := scanCollection[projectstore.Project](filepath.Join(dataDir, "projects"), db, "projects")
	if err != nil {
		return err
	}
	for _, p := range projects {
		row, err := toMap(p)
		if err != nil {
			return err
		}
		for _, k := range []string{"agents", "pipelines", "terminals", "plans", "autopilots"} {
			delete(row, k)
		}
		if g, ok := groupFor[p.ID]; ok {
			row["group_id"] = g.id
			row["group_order"] = g.order
		}
		set.projects[p.ID] = row
		set.add("projects", p.ID, row)
	}
	return nil
}

func loadAgents(ctx context.Context, dataDir string, set *stagingSet) error {
	if err := scanAgentCollection(ctx, dataDir, "agents-db", "agents", false, set); err != nil {
		return err
	}
	return scanAgentCollection(ctx, dataDir, "agents-db", "archived", true, set)
}

func scanAgentCollection(ctx context.Context, dataDir, storeDir, colName string, archived bool, set *stagingSet) error {
	db, err := openIfExists(filepath.Join(dataDir, storeDir))
	if err != nil || db == nil {
		return err
	}
	defer db.Close()
	agents, err := scanCollection[agentstore.Agent](filepath.Join(dataDir, storeDir), db, colName)
	if err != nil {
		return err
	}
	for _, a := range agents {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := toMap(a)
		if err != nil {
			return err
		}
		for _, k := range []string{"child_agents", "child_pipelines", "child_autopilots"} {
			delete(row, k)
		}
		if archived {
			if _, ok := row["archived_from_status"]; !ok && a.Status != "" {
				row["archived_from_status"] = string(a.Status)
			}
			row["status"] = "archived"
			if _, ok := row["archived_at"]; !ok {
				row["archived_at"] = timeOr(a.UpdatedAt, a.CreatedAt).UTC().Format(time.RFC3339)
			}
		}
		if _, dup := set.agents[a.ID]; dup {
			set.block("agents", a.ID+": duplicate agent identity across active/archived stores", ClassDuplicateIdentity)
			continue
		}
		set.agents[a.ID] = row
		set.add("agents", a.ID, row)
	}
	return nil
}

func loadTerminals(ctx context.Context, dataDir string, set *stagingSet) error {
	db, err := openIfExists(filepath.Join(dataDir, "terminals-db"))
	if err != nil || db == nil {
		return err
	}
	defer db.Close()
	terms, err := scanCollection[terminalstore.Terminal](filepath.Join(dataDir, "terminals-db"), db, "terminals")
	if err != nil {
		return err
	}
	for _, term := range terms {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := toMap(term)
		if err != nil {
			return err
		}
		set.terms[term.ID] = row
		set.add("terminals", term.ID, row)
	}
	return nil
}

func loadPipelines(dataDir string, set *stagingSet) error {
	db, err := openIfExists(filepath.Join(dataDir, "pipelines-db"))
	if err != nil || db == nil {
		return err
	}
	defer db.Close()
	pipes, err := scanCollection[pipeline.Pipeline](filepath.Join(dataDir, "pipelines-db"), db, "pipelines")
	if err != nil {
		return err
	}
	for _, p := range pipes {
		row, err := toMap(p)
		if err != nil {
			return err
		}
		delete(row, "jobs")
		set.pipes[p.ID] = row
		set.add("pipelines", p.ID, row)
		seenJobs := map[string]bool{}
		for i, j := range p.Jobs {
			if j.ID == "" {
				set.block("pipeline_jobs", p.ID+": job has empty id", ClassUnsupportedLayout)
				continue
			}
			if seenJobs[j.ID] {
				set.block("pipeline_jobs", p.ID+":"+j.ID+": duplicate job id in pipeline", ClassDuplicateIdentity)
				continue
			}
			seenJobs[j.ID] = true
			jrow, err := toMap(j)
			if err != nil {
				return err
			}
			jrow["id"] = p.ID + ":" + j.ID
			jrow["pipeline_id"] = p.ID
			jrow["job_id"] = j.ID
			jrow["seq"] = i
			key := p.ID + ":" + j.ID
			set.jobs[key] = jrow
			set.add("pipeline_jobs", key, jrow)
		}
	}
	return nil
}

func validateStagingRelationships(set *stagingSet) {
	tmux := map[string]string{}
	for id, a := range set.agents {
		addUnique(set, tmux, "agents", id, str(a["tmux_session"]))
		if pid := str(a["project_id"]); pid != "" {
			if _, ok := set.projects[pid]; !ok {
				set.findings = append(set.findings, Finding{Severity: SeverityRepairable, Class: string(ClassMissingArtifactRef), Store: "agents", Message: id + ": project_id " + pid + " is unscoped legacy reference"})
			}
		}
		if parent := str(a["parent_id"]); parent != "" {
			if _, ok := set.agents[parent]; !ok {
				set.block("agents", id+": parent_id "+parent+" does not exist", ClassAmbiguity)
			}
		}
		if pipeID := str(a["pipeline_id"]); pipeID != "" {
			jobID := str(a["job_id"])
			if _, ok := set.pipes[pipeID]; !ok {
				set.block("agents", id+": pipeline_id "+pipeID+" does not exist", ClassAmbiguity)
			} else if jobID != "" {
				key := pipeID + ":" + jobID
				j, ok := set.jobs[key]
				if !ok {
					set.block("agents", id+": job binding "+key+" does not exist", ClassAmbiguity)
				} else if ja := str(j["agent_id"]); ja != "" && ja != id {
					set.block("pipeline_jobs", key+": agent_id disagrees with agent binding "+id, ClassAmbiguity)
				}
			}
		}
	}
	for id, t := range set.terms {
		addUnique(set, tmux, "terminals", id, str(t["tmux_session"]))
		if pid := str(t["project_id"]); pid != "" {
			if _, ok := set.projects[pid]; !ok {
				set.block("terminals", id+": project_id "+pid+" does not exist", ClassAmbiguity)
			}
		}
	}
	for id, p := range set.pipes {
		if pid := str(p["project_id"]); pid != "" {
			if _, ok := set.projects[pid]; !ok {
				set.block("pipelines", id+": project_id "+pid+" does not exist", ClassAmbiguity)
			}
		}
		if parent := str(p["parent_agent_id"]); parent != "" {
			if _, ok := set.agents[parent]; !ok {
				set.block("pipelines", id+": parent_agent_id "+parent+" does not exist", ClassAmbiguity)
			}
		}
	}
	for key, j := range set.jobs {
		pipeID := str(j["pipeline_id"])
		if _, ok := set.pipes[pipeID]; !ok {
			set.block("pipeline_jobs", key+": pipeline_id "+pipeID+" does not exist", ClassAmbiguity)
		}
		for _, dep := range stringSlice(j["depends_on"]) {
			if _, ok := set.jobs[pipeID+":"+dep]; !ok {
				set.block("pipeline_jobs", key+": depends_on "+dep+" does not exist in pipeline", ClassAmbiguity)
			}
		}
		if aid := str(j["agent_id"]); aid != "" {
			if _, ok := set.agents[aid]; !ok {
				set.block("pipeline_jobs", key+": agent_id "+aid+" does not exist", ClassAmbiguity)
			}
		}
	}
}

func openIfExists(path string) (*scriva.DB, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return wardendb.OpenOfflineLegacyDir(path)
}

func scanCollection[T any](dbDir string, db *scriva.DB, name string) ([]T, error) {
	if _, err := os.Stat(filepath.Join(dbDir, name)); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	col, err := db.Collection(name)
	if err != nil {
		return nil, err
	}
	rows, err := col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(rows))
	for _, row := range rows {
		var v T
		raw, err := json.Marshal(row.Data)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *stagingSet) add(collection, key string, body map[string]any) {
	s.rows = append(s.rows, stagingRow{collection: collection, key: key, body: body})
}

func (s *stagingSet) block(store, msg string, class FindingClass) {
	s.findings = append(s.findings, Finding{Severity: SeverityBlocking, Class: string(class), Store: store, Message: msg, Details: strictRemediation(class)})
}

func addUnique(set *stagingSet, seen map[string]string, coll, id, tmux string) {
	if tmux == "" {
		return
	}
	ref := coll + "/" + id
	if prior, ok := seen[tmux]; ok {
		set.block(coll, ref+": tmux_session "+tmux+" duplicates "+prior, ClassDuplicateIdentity)
		return
	}
	seen[tmux] = ref
}

func ensureStagingIndexes(col *engine.Collection, name string) error {
	indexes := map[string][]string{
		"agents":         {"project_id", "pipeline_id", "parent_id", "tmux_session", "status"},
		"terminals":      {"project_id", "tmux_session", "status"},
		"projects":       {"status", "group_id"},
		"project_groups": {"name"},
		"pipelines":      {"project_id", "parent_agent_id", "status"},
		"pipeline_jobs":  {"pipeline_id", "agent_id", "status"},
	}
	for _, f := range indexes[name] {
		if err := col.EnsureIndex(f); err != nil {
			return err
		}
	}
	return nil
}

func toMap(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	delete(out, "_key")
	return out, nil
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func canonicalEqual(a, b map[string]any) bool {
	ca := cloneMap(a)
	cb := cloneMap(b)
	delete(ca, "_key")
	delete(cb, "_key")
	ra, errA := json.Marshal(ca)
	rb, errB := json.Marshal(cb)
	if errA != nil || errB != nil {
		return reflect.DeepEqual(ca, cb)
	}
	var na, nb any
	if err := json.Unmarshal(ra, &na); err != nil {
		return reflect.DeepEqual(ca, cb)
	}
	if err := json.Unmarshal(rb, &nb); err != nil {
		return reflect.DeepEqual(ca, cb)
	}
	return reflect.DeepEqual(na, nb)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func countBlocking(findings []Finding) int {
	var n int
	for _, f := range findings {
		if f.Severity == SeverityBlocking {
			n++
		}
	}
	return n
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func stringSlice(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s := str(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func timeOr(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}
