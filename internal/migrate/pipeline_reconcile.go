package migrate

// Pipeline reconciliation for the schema-2 staging export.
//
// Schema 1 kept two copies of every pipeline: the legacy pipelines/<id>.json
// files and the pipelines-db ScrivaDB that the one-time legacy import filled
// (legacyimport is additive and sentinel-guarded, so after the import the
// ScrivaDB is authoritative and the JSON directory is a read-only backup).
// Pipelines deleted after the import therefore survive only as stale JSON
// copies, while agents and plans that ran under them keep their pipeline_id.
//
// The exporter never guesses between the two copies:
//
//   - a legacy file identical to its pipelines-db record is deduplicated;
//   - a legacy file that differs from its record is a conflict (no winner);
//   - a legacy-only file is dropped only when pipelines-db carries a delete
//     tombstone for that id (proof it was imported and then deleted); without
//     one the deletion is unproven and the export refuses;
//   - an agent or plan that points at a pipeline that is gone is rewritten to
//     carry a wardendb.HistoricalRef instead of a dangling pipeline_id when it
//     is archived/finished, and refuses the export when it is still active.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/wardendb"
)

// Stable finding classes for pipeline reconciliation.
const (
	ClassPipelineLegacyConflict   FindingClass = "pipeline_legacy_conflict"   // legacy file != pipelines-db record
	ClassPipelineUnprovenDeletion FindingClass = "pipeline_unproven_deletion" // legacy-only, no delete tombstone
	ClassPipelineSuperseded       FindingClass = "pipeline_legacy_superseded" // note: stale copy of a deleted pipeline
	ClassPipelineHistoricalRef    FindingClass = "pipeline_historical_ref"    // note: dangling ref kept as provenance
	ClassPipelineActiveRef        FindingClass = "pipeline_active_ref"        // active record whose pipeline is gone
)

const (
	// maxRefsListed bounds how many referrers one finding names.
	maxRefsListed = 5
	// maxPipelineFindings bounds the per-pipeline findings of each kind.
	maxPipelineFindings = 25
	// maxDiagText bounds any free text copied from a source into a finding.
	maxDiagText = 200
)

// pipelineTombstone is the proof that a pipelines-db record was deleted.
type pipelineTombstone struct {
	RecordID  uint64
	DeletedAt string
	Segment   string
}

func (t pipelineTombstone) evidence() string {
	return fmt.Sprintf("pipelines-db delete tombstone (record %d, %s, %s)", t.RecordID, t.DeletedAt, t.Segment)
}

// legacyPipeline is one decoded pipelines/<id>.json file.
type legacyPipeline struct {
	file string
	row  map[string]any
}

// reconcileLegacyPipelines folds the legacy pipelines/*.json directory into the
// staging set. loadPipelines must have loaded the pipelines-db rows already.
func reconcileLegacyPipelines(dataDir string, set *stagingSet) error {
	dir := filepath.Join(dataDir, "pipelines")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		set.block("pipelines", "pipelines: cannot list legacy directory: "+boundText(err.Error()), ClassUnverifiable)
		return nil
	}
	legacy := map[string]legacyPipeline{}
	for _, de := range entries {
		name := de.Name()
		rel := "pipelines/" + name
		switch {
		case de.Type()&os.ModeSymlink != 0:
			set.block("pipelines", rel+": symlink is never followed", ClassUnsafeArtifact)
		case de.IsDir():
			set.block("pipelines", rel+": unexpected directory in the legacy pipelines layout", ClassUnsupportedLayout)
		case strings.HasPrefix(name, ".tmp-"):
			// Atomic-write temp file of the schema-1 store; never a record.
			set.findings = append(set.findings, Finding{Severity: SeverityRepairable, Class: string(ClassUnsupportedLayout), Store: "pipelines",
				Message: rel + ": leftover temp file ignored (not a pipeline record)"})
		case !strings.HasSuffix(name, ".json"):
			set.block("pipelines", rel+": file has no safe staging mapping", ClassUnsupportedLayout)
		default:
			lp, ok := readLegacyPipeline(dir, name, set)
			if !ok {
				continue
			}
			id := str(lp.row["id"])
			if prior, dup := legacy[id]; dup {
				set.block("pipelines", fmt.Sprintf("%s and %s both declare pipeline %s", prior.file, rel, id), ClassDuplicateIdentity)
				continue
			}
			legacy[id] = legacyPipeline{file: rel, row: lp.row}
		}
	}
	if len(legacy) == 0 {
		return nil
	}

	tombs, err := readPipelineTombstones(dataDir)
	if err != nil {
		set.block("pipelines-db", "pipelines-db: cannot read delete tombstones: "+boundText(err.Error()), ClassUnverifiable)
		return nil
	}
	set.pipeTombstones = tombs

	var identical, superseded []string
	conflicts, unproven := 0, 0
	for _, id := range sortedKeys(legacy) {
		lp := legacy[id]
		if live, ok := set.pipes[id]; ok {
			if canonicalEqual(live, lp.row) {
				identical = append(identical, id)
				continue
			}
			conflicts++
			if conflicts <= maxPipelineFindings {
				set.findings = append(set.findings, Finding{Severity: SeverityBlocking, Class: string(ClassPipelineLegacyConflict), Store: "pipelines",
					Message: fmt.Sprintf("%s: pipelines-db record and %s differ (%s); the cutover never picks a winner", id, lp.file, differingFields(live, lp.row)),
					Details: "the legacy file is the pre-import backup and the database record is authoritative only if you say so: if the database record is the truth, " +
						"move the legacy file aside (it is not modified by the cutover); if the legacy file is, restore it into pipelines-db with the schema-1 binary, then retry"})
			}
			continue
		}
		if _, ok := tombs[id]; ok {
			superseded = append(superseded, id)
			continue
		}
		unproven++
		set.pipeUnresolved[id] = true
		if unproven <= maxPipelineFindings {
			set.findings = append(set.findings, Finding{Severity: SeverityBlocking, Class: string(ClassPipelineUnprovenDeletion), Store: "pipelines",
				Message: fmt.Sprintf("%s: %s has no pipelines-db record and no delete tombstone, so it is not provable that the pipeline was deleted rather than never imported", id, lp.file),
				Details: "if the pipeline was deleted, move the legacy file aside; if it must be kept, import it into pipelines-db with the schema-1 binary; the cutover neither resurrects nor drops it on its own"})
		}
	}
	if conflicts > maxPipelineFindings || unproven > maxPipelineFindings {
		set.block("pipelines", fmt.Sprintf("pipelines: %d conflicting and %d unproven legacy pipeline(s) in total; only the first %d of each are listed", conflicts, unproven, maxPipelineFindings), ClassAmbiguity)
	}
	if len(identical) > 0 {
		set.findings = append(set.findings, Finding{Severity: SeverityAuto, Class: string(ClassPipelineSuperseded), Store: "pipelines",
			Message: fmt.Sprintf("%d legacy pipeline file(s) are identical to their pipelines-db record and deduplicated (%s)", len(identical), listIDs(identical))})
	}
	if len(superseded) > 0 {
		set.findings = append(set.findings, Finding{Severity: SeverityAuto, Class: string(ClassPipelineSuperseded), Store: "pipelines",
			Message: fmt.Sprintf("%d legacy pipeline file(s) are stale copies of pipelines deleted after the import and are not carried into warden-db (%s)", len(superseded), listIDs(superseded)),
			Details: "proof: pipelines-db holds a delete tombstone for each; the files stay untouched in place and in the pre-migration backup"})
	}
	return nil
}

// readLegacyPipeline decodes one legacy file. Every failure is a bounded
// blocking finding naming the file, never the file's content.
func readLegacyPipeline(dir, name string, set *stagingSet) (legacyPipeline, bool) {
	rel := "pipelines/" + name
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		set.block("pipelines", rel+": unreadable: "+boundText(err.Error()), ClassUnverifiable)
		return legacyPipeline{}, false
	}
	var p pipeline.Pipeline
	if err := json.Unmarshal(raw, &p); err != nil {
		set.block("pipelines", rel+": malformed pipeline JSON: "+boundText(err.Error()), ClassCorruption)
		return legacyPipeline{}, false
	}
	if p.ID == "" {
		set.block("pipelines", rel+": pipeline has no id", ClassCorruption)
		return legacyPipeline{}, false
	}
	if err := store.SafeID(p.ID); err != nil {
		set.block("pipelines", rel+": unsafe pipeline id", ClassCorruption)
		return legacyPipeline{}, false
	}
	if p.ID+".json" != name {
		set.block("pipelines", fmt.Sprintf("%s: file name does not match its pipeline id %q", rel, p.ID), ClassAmbiguity)
		return legacyPipeline{}, false
	}
	row, err := toMap(p)
	if err != nil {
		set.block("pipelines", rel+": cannot normalise: "+boundText(err.Error()), ClassCorruption)
		return legacyPipeline{}, false
	}
	return legacyPipeline{file: rel, row: row}, true
}

// readPipelineTombstones replays the pipelines-db segments and returns the ids
// whose last event is a delete. Segment lines are Scriva's own append-only log:
// {id, op, data}. A delete line carries only the record id, so the pipeline id
// is recovered from the insert/update that introduced that record id; deletes
// whose record id never appears (a compacted log) prove nothing and are ignored.
func readPipelineTombstones(dataDir string) (map[string]pipelineTombstone, error) {
	colDir := filepath.Join(dataDir, "pipelines-db", "pipelines")
	segs, err := filepath.Glob(filepath.Join(colDir, "seg_*.ndjson"))
	if err != nil {
		return nil, err
	}
	sort.Strings(segs)
	keyOf := map[uint64]string{}
	live := map[string]uint64{}
	tombs := map[string]pipelineTombstone{}
	for _, seg := range segs {
		f, err := os.Open(seg)
		if err != nil {
			return nil, err
		}
		br := bufio.NewReaderSize(f, 64*1024)
		for {
			line, rerr := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				var ev struct {
					ID   uint64         `json:"id"`
					Op   string         `json:"op"`
					TS   string         `json:"ts"`
					Data map[string]any `json:"data"`
				}
				// A torn or foreign line is the strict store verifier's concern;
				// here it simply proves nothing.
				if json.Unmarshal(line, &ev) == nil {
					switch ev.Op {
					case "insert", "update":
						key := str(ev.Data["_key"])
						if key == "" {
							key = str(ev.Data["id"])
						}
						if key != "" {
							keyOf[ev.ID] = key
							live[key] = ev.ID
							delete(tombs, key)
						}
					case "delete":
						if key, ok := keyOf[ev.ID]; ok && live[key] == ev.ID {
							delete(live, key)
							tombs[key] = pipelineTombstone{RecordID: ev.ID, DeletedAt: ev.TS, Segment: filepath.Base(seg)}
						}
					}
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				f.Close()
				return nil, rerr
			}
		}
		f.Close()
	}
	return tombs, nil
}

// resolveHistoricalPipelineRefs makes every agent/plan pipeline reference
// resolve or become provenance. It runs after all sources are loaded and before
// the generic relationship validation, which stays strict.
func resolveHistoricalPipelineRefs(set *stagingSet) {
	type ref struct {
		kind, id, status string
		historical       bool
		job              string
		row              map[string]any
	}
	byPipe := map[string][]ref{}
	for id, a := range set.agents {
		pid := str(a["pipeline_id"])
		if pid == "" {
			continue
		}
		if _, ok := set.pipes[pid]; ok {
			continue
		}
		status := str(a["status"])
		hist := set.archivedAgents[id] || agentFinished(status)
		byPipe[pid] = append(byPipe[pid], ref{"agent", id, status, hist, str(a["job_id"]), a})
	}
	for id, p := range set.plans {
		pid := str(p["pipeline_id"])
		if pid == "" {
			continue
		}
		if _, ok := set.pipes[pid]; ok {
			continue
		}
		status := str(p["status"])
		byPipe[pid] = append(byPipe[pid], ref{"plan", id, status, planFinished(status), "", p})
	}

	emitted := 0
	for _, pid := range sortedKeys(byPipe) {
		refs := byPipe[pid]
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].kind != refs[j].kind {
				return refs[i].kind < refs[j].kind
			}
			return refs[i].id < refs[j].id
		})
		var active []ref
		for _, r := range refs {
			if !r.historical {
				active = append(active, r)
			}
		}
		tomb, deleted := set.pipeTombstones[pid]
		switch {
		case len(active) > 0:
			for _, r := range refs {
				set.refReported[r.kind+"\x00"+r.id] = true
			}
			emitted++
			if emitted <= maxPipelineFindings {
				set.findings = append(set.findings, Finding{Severity: SeverityBlocking, Class: string(ClassPipelineActiveRef), Store: "pipelines",
					Message: fmt.Sprintf("pipeline %s does not exist but %d live record(s) still belong to it (%s)", pid, len(active), describeRefs(active, func(r ref) string { return fmt.Sprintf("%s %s [%s]", r.kind, r.id, r.status) })),
					Details: "an active agent or plan cannot be left without its owner: finish or archive them on the schema-1 binary first, or restore the pipeline; the cutover never nulls the reference or invents a parent"})
			}
		case set.pipeUnresolved[pid]:
			// The pipeline's own legacy file is already a blocking finding;
			// do not rewrite anything that may yet resolve.
			for _, r := range refs {
				set.refReported[r.kind+"\x00"+r.id] = true
			}
		default:
			reason, evidence := wardendb.HistoricalUnrecorded, "no pipeline record exists in pipelines-db or pipelines/"
			if deleted {
				reason, evidence = wardendb.HistoricalDeleted, tomb.evidence()
			}
			for _, r := range refs {
				r.row["historical_pipeline"] = map[string]any{
					"kind": wardendb.HistoricalKindPipeline, "id": pid, "reason": reason,
					"evidence": evidence, "recorded_by": Migration0002ID,
				}
				if r.job != "" {
					r.row["historical_pipeline"].(map[string]any)["job_id"] = r.job
				}
				delete(r.row, "pipeline_id")
				delete(r.row, "job_id")
			}
			for _, r := range refs {
				set.refReported[r.kind+"\x00"+r.id] = true
			}
			set.findings = append(set.findings, Finding{Severity: SeverityRepairable, Class: string(ClassPipelineHistoricalRef), Store: "pipelines",
				Message: fmt.Sprintf("pipeline %s (%s): %d historical record(s) keep it as provenance (historical_pipeline) instead of a dangling pipeline_id (%s)", pid, reason, len(refs), describeRefs(refs, func(r ref) string { return r.kind + " " + r.id })),
				Details: evidence})
		}
	}
	if emitted > maxPipelineFindings {
		set.block("pipelines", fmt.Sprintf("pipelines: %d pipelines have live records but no pipeline record in total; only the first %d are listed", emitted, maxPipelineFindings), ClassAmbiguity)
	}
}

func agentFinished(status string) bool {
	switch status {
	case "done", "errored", "orphaned":
		return true
	}
	return false
}

func planFinished(status string) bool {
	return status == string(planstore.PlanStatusCompleted) || status == string(planstore.PlanStatusArchived)
}

func describeRefs[T any](refs []T, name func(T) string) string {
	var parts []string
	for i, r := range refs {
		if i == maxRefsListed {
			parts = append(parts, fmt.Sprintf("and %d more", len(refs)-maxRefsListed))
			break
		}
		parts = append(parts, name(r))
	}
	return strings.Join(parts, ", ")
}

func listIDs(ids []string) string {
	if len(ids) > maxRefsListed {
		return strings.Join(ids[:maxRefsListed], ", ") + fmt.Sprintf(", and %d more", len(ids)-maxRefsListed)
	}
	return strings.Join(ids, ", ")
}

// differingFields names the top-level fields on which two normalised rows
// disagree (names only: never values, which may be private).
func differingFields(a, b map[string]any) string {
	seen := map[string]bool{}
	var diff []string
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	for _, k := range sortedKeys(seen) {
		ra, _ := json.Marshal(a[k])
		rb, _ := json.Marshal(b[k])
		if !bytes.Equal(ra, rb) {
			diff = append(diff, k)
		}
	}
	if len(diff) > 8 {
		diff = append(diff[:8], fmt.Sprintf("+%d more", len(diff)-8))
	}
	return "differing fields: " + strings.Join(diff, ", ")
}

func boundText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxDiagText {
		return s[:maxDiagText] + "…"
	}
	return s
}
