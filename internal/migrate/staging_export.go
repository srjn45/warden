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
	rowKeys  map[string]string
	agents   map[string]map[string]any
	terms    map[string]map[string]any
	projects map[string]map[string]any
	plans    map[string]map[string]any
	pipes    map[string]map[string]any
	jobs     map[string]map[string]any
	runs     map[string]map[string]any
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
		rowKeys: map[string]string{},
		agents:  map[string]map[string]any{}, terms: map[string]map[string]any{},
		projects: map[string]map[string]any{}, plans: map[string]map[string]any{},
		pipes: map[string]map[string]any{}, jobs: map[string]map[string]any{},
		runs: map[string]map[string]any{},
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
	if err := loadMappedMigratedSources(ctx, dataDir, set); err != nil {
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

type collectionMapping struct {
	source string
	target string
}

var migratedCollectionMappings = map[string][]collectionMapping{
	"plans/plans-db": {
		{"plans", "plans"},
		{"plan-events", "plan_events"},
		{"plan-notes", "plan_notes"},
	},
	"plans/plan-exports-db": {{"plan-exports", "plan_exports"}},
	"autopilots-db":         {{"autopilot_runs", "autopilot_runs"}},
	"autopilot/runs-db":     {{"autopilot_runs", "autopilot_runs"}},
	"context": {
		{"context", "context_entries"},
		{"context_entries", "context_entries"},
	},
	"inbox": {
		{"messages", "messages"},
		{"id_sequences", "id_sequences"},
	},
	"backends": {
		{"backends", "backends"},
		{"models", "backend_models"},
		{"role_tiers", "backend_roles"},
		{"handover_settings", "backend_settings"},
		{"quotas", "backend_quotas"},
		{"rl_cooldowns", "backend_rl_cooldowns"},
	},
	"usage-snapshots":  {{"usage_snapshots", "usage_snapshots"}},
	"quota-impact":     {{"quota_impact_fences", "quota_impact_fences"}},
	"snapshots-db":     {{"snapshots", "snapshots"}},
	"savings":          {{"savings_events", "savings_events"}, {"savings_calibration", "savings_calibration"}},
	"spend":            {{"spend_records", "spend_records"}},
	"schedules-db":     {{"schedules", "schedules"}},
	"known-prompts-db": {{"known_prompts", "known_prompts"}},
	"metrics":          {{"metrics_samples", "metrics_samples"}},
}

var migratedFlatMappings = map[string]string{
	"plan-sync-hub":  "plan_sync_envelopes",
	"schedules.json": "schedules",
	"snapshots":      "snapshots",
}

func loadMappedMigratedSources(ctx context.Context, dataDir string, set *stagingSet) error {
	handled := map[string]bool{
		"agents-db": true, "terminals-db": true, "projects": true, "pipelines-db": true,
		"autopilot": true,
	}
	for rel, mappings := range migratedCollectionMappings {
		handled[rel] = true
		if err := loadMappedScrivaStore(ctx, dataDir, rel, mappings, set); err != nil {
			return err
		}
	}
	for rel, target := range migratedFlatMappings {
		handled[rel] = true
		if err := loadFlatJSONRecords(ctx, dataDir, rel, target, set); err != nil {
			return err
		}
	}
	for _, s := range schema2Sources {
		if s.Disposition != DispositionMigrated || handled[s.Path] {
			continue
		}
		hasData, err := migratedSourceHasData(dataDir, s.Path)
		if err != nil {
			return err
		}
		if hasData {
			set.block(s.Path, s.Path+": migrated source has data but staging export has no safe mapping to "+s.Destination, ClassUnsupportedLayout)
		}
	}
	return nil
}

func loadMappedScrivaStore(ctx context.Context, dataDir, rel string, mappings []collectionMapping, set *stagingSet) error {
	dbDir := filepath.Join(dataDir, filepath.FromSlash(rel))
	db, err := openIfExists(dbDir)
	if err != nil || db == nil {
		return err
	}
	defer db.Close()
	allowed := map[string]string{}
	for _, m := range mappings {
		allowed[m.source] = m.target
		if err := copyCollection(ctx, dbDir, db, m.source, m.target, set); err != nil {
			return err
		}
	}
	return blockUnmappedCollections(ctx, dbDir, db, rel, allowed, set)
}

func copyCollection(ctx context.Context, dbDir string, db *scriva.DB, source, target string, set *stagingSet) error {
	if _, err := os.Stat(filepath.Join(dbDir, source)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	col, err := db.Collection(source)
	if err != nil {
		return err
	}
	rows, err := col.Scan(query.MatchAll)
	if err != nil {
		return err
	}
	for _, rec := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		body := cloneMap(rec.Data)
		key := str(body["_key"])
		delete(body, "_key")
		if key == "" {
			key = str(body["id"])
		}
		if key == "" {
			set.block(target, source+": record is missing stable key and id", ClassAmbiguity)
			continue
		}
		if set.add(target, key, body) {
			rememberStagingRow(set, target, key, body)
		}
	}
	return nil
}

func blockUnmappedCollections(ctx context.Context, dbDir string, db *scriva.DB, rel string, allowed map[string]string, set *stagingSet) error {
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		return err
	}
	for _, de := range entries {
		if !de.IsDir() || allowed[de.Name()] != "" || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		col, err := db.Collection(de.Name())
		if err != nil {
			return err
		}
		rows, err := col.Scan(query.MatchAll)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			set.block(rel, rel+"/"+de.Name()+": collection has data but no safe staging mapping", ClassUnsupportedLayout)
		}
	}
	return nil
}

func loadFlatJSONRecords(ctx context.Context, dataDir, rel, target string, set *stagingSet) error {
	root := filepath.Join(dataDir, filepath.FromSlash(rel))
	fi, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if fi.IsDir() {
		return filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !strings.HasSuffix(de.Name(), ".json") {
				if !strings.HasSuffix(de.Name(), ".transcript") {
					set.block(rel, relPath(dataDir, path)+": file has no safe staging mapping", ClassUnsupportedLayout)
				}
				return nil
			}
			return loadFlatJSONFile(dataDir, path, target, set)
		})
	}
	return loadFlatJSONFile(dataDir, root, target, set)
}

func loadFlatJSONFile(dataDir, path, target string, set *stagingSet) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	add := func(body map[string]any, fallback string) {
		key := str(body["id"])
		if key == "" {
			key = str(body["run_id"])
		}
		if key == "" {
			key = fallback
		}
		rememberStagingRow(set, target, key, body)
		set.add(target, key, body)
	}
	switch x := v.(type) {
	case map[string]any:
		if target == "schedules" && str(x["id"]) == "" {
			for key, item := range x {
				body, ok := item.(map[string]any)
				if !ok {
					set.block(target, relPath(dataDir, path)+": schedule "+key+" is not an object", ClassCorruption)
					continue
				}
				add(body, key)
			}
			return nil
		}
		if records, ok := x["records"].([]any); ok {
			for i, item := range records {
				body, ok := item.(map[string]any)
				if !ok {
					set.block(target, relPath(dataDir, path)+": records entry is not an object", ClassCorruption)
					continue
				}
				add(body, fmt.Sprintf("%s:%06d", relPath(dataDir, path), i))
			}
			return nil
		}
		add(x, strings.TrimSuffix(relPath(dataDir, path), ".json"))
	case []any:
		for i, item := range x {
			body, ok := item.(map[string]any)
			if !ok {
				set.block(target, relPath(dataDir, path)+": array entry is not an object", ClassCorruption)
				continue
			}
			add(body, fmt.Sprintf("%s:%06d", relPath(dataDir, path), i))
		}
	default:
		set.block(target, relPath(dataDir, path)+": JSON value is not an object or array", ClassUnsupportedLayout)
	}
	return nil
}

func migratedSourceHasData(dataDir, rel string) (bool, error) {
	root := filepath.Join(dataDir, filepath.FromSlash(rel))
	fi, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return fi.Size() > 0, nil
	}
	has := false
	err = filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil || has || de.IsDir() {
			return err
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		has = info.Size() > 0
		return nil
	})
	return has, err
}

func rememberStagingRow(set *stagingSet, target, key string, body map[string]any) {
	switch target {
	case "plans":
		set.plans[key] = body
	case "autopilot_runs":
		set.runs[key] = body
	}
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
	for id, p := range set.plans {
		if pid := str(p["project_id"]); pid != "" {
			if _, ok := set.projects[pid]; !ok {
				set.block("plans", id+": project_id "+pid+" does not exist", ClassAmbiguity)
			}
		}
	}
	for id, r := range set.runs {
		if pid := str(r["project_id"]); pid != "" {
			if _, ok := set.projects[pid]; !ok {
				set.block("autopilot_runs", id+": project_id "+pid+" does not exist", ClassAmbiguity)
			}
		}
		if planID := str(r["plan_id"]); planID != "" {
			if _, ok := set.plans[planID]; !ok {
				set.block("autopilot_runs", id+": plan_id "+planID+" does not exist", ClassAmbiguity)
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

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
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

func (s *stagingSet) add(collection, key string, body map[string]any) bool {
	k := collection + "\x00" + key
	if prior, dup := s.rowKeys[k]; dup {
		s.block(collection, key+": duplicate staging identity from "+prior, ClassDuplicateIdentity)
		return false
	}
	s.rowKeys[k] = collection + "/" + key
	s.rows = append(s.rows, stagingRow{collection: collection, key: key, body: body})
	return true
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
		"plans":          {"project_id", "status"},
		"pipelines":      {"project_id", "parent_agent_id", "status"},
		"pipeline_jobs":  {"pipeline_id", "agent_id", "status"},
		"autopilot_runs": {"project_id", "plan_id", "state"},
		"messages":       {"to"},
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
