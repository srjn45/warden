package migrate

// The equivalence verifier deliberately works on the staging output rather
// than the live cutover directory.  It is therefore safe to run repeatedly as
// part of a plan, CI, or an operator's review before the ledger is advanced.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/srjn45/scriva/query"

	"github.com/srjn45/warden/internal/wardendb"
)

// EquivalenceReport is durable, machine-readable evidence that a staging
// export represents precisely the records the importer intended to write.
// Hashes include the stable key, so an identical payload under a different
// identity cannot pass verification.
type EquivalenceReport struct {
	DataDir      string              `json:"data_dir"`
	TargetDir    string              `json:"target_dir"`
	SourceCounts map[string]int      `json:"source_counts"`
	TargetCounts map[string]int      `json:"target_counts"`
	SourceHashes map[string]string   `json:"source_hashes"`
	TargetHashes map[string]string   `json:"target_hashes"`
	Findings     []Finding           `json:"findings,omitempty"`
	Passed       bool                `json:"passed"`
	Quarantined  []QuarantinedRecord `json:"quarantined,omitempty"`
}

// VerifySchema2Equivalence compares a legacy source with an existing staging
// target. It never writes either directory.  A missing, extra, or divergent
// target record is a blocking finding, as are invalid normalized references.
func VerifySchema2Equivalence(ctx context.Context, dataDir, targetDir string) (*EquivalenceReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	set, err := buildStagingSet(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	rep := &EquivalenceReport{
		DataDir: dataDir, TargetDir: targetDir,
		SourceCounts: map[string]int{}, TargetCounts: map[string]int{},
		SourceHashes: map[string]string{}, TargetHashes: map[string]string{},
		Findings:    append([]Finding(nil), set.findings...),
		Quarantined: append([]QuarantinedRecord(nil), set.quarantined...),
	}
	if countBlocking(rep.Findings) != 0 {
		return rep, fmt.Errorf("%w: source has blocking relationship finding", ErrStagingExportRefused)
	}
	want := map[string]map[string]map[string]any{}
	for _, row := range set.rows {
		if want[row.collection] == nil {
			want[row.collection] = map[string]map[string]any{}
		}
		want[row.collection][row.key] = row.body
	}
	db, err := wardendb.OpenOfflineStagingDir(targetDir)
	if err != nil {
		return rep, err
	}
	defer db.Close()
	for _, name := range sortedKeys(want) {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.SourceCounts[name], rep.SourceHashes[name] = len(want[name]), equivalentHash(want[name])
		col, err := db.Collection(name)
		if err != nil {
			return rep, fmt.Errorf("equivalence: target collection %s: %w", name, err)
		}
		rows, err := col.Scan(query.MatchAll)
		if err != nil {
			return rep, err
		}
		got := map[string]map[string]any{}
		for _, row := range rows {
			key := str(row.Data["_key"])
			if key == "" {
				rep.Findings = append(rep.Findings, Finding{Severity: SeverityBlocking, Class: string(ClassAmbiguity), Store: name, Message: "target row is missing its storage key"})
				continue
			}
			got[key] = row.Data
		}
		rep.TargetCounts[name], rep.TargetHashes[name] = len(got), equivalentHash(got)
		if rep.SourceCounts[name] != rep.TargetCounts[name] || rep.SourceHashes[name] != rep.TargetHashes[name] {
			rep.Findings = append(rep.Findings, Finding{Severity: SeverityBlocking, Class: string(ClassAmbiguity), Store: name, Message: "source/target count or canonical hash differs"})
			continue
		}
		for key, body := range want[name] {
			if actual, ok := got[key]; !ok || !canonicalEqual(actual, body) {
				rep.Findings = append(rep.Findings, Finding{Severity: SeverityBlocking, Class: string(ClassAmbiguity), Store: name, Message: key + ": target record differs"})
			}
		}
	}
	// Reject target-only collections too: they are evidence of a stale or mixed
	// staging directory, not harmless noise.
	for _, name := range wardendb.Catalogue() {
		if _, expected := want[name]; expected {
			continue
		}
		if _, err := os.Stat(filepath.Join(targetDir, name)); err == nil {
			rep.Findings = append(rep.Findings, Finding{Severity: SeverityBlocking, Class: string(ClassAmbiguity), Store: name, Message: "unexpected target collection"})
		}
	}
	if err := verifyEquivalentProjectViews(want); err != nil {
		rep.Findings = append(rep.Findings, Finding{Severity: SeverityBlocking, Class: string(ClassAmbiguity), Store: "project_views", Message: err.Error()})
	}
	rep.Passed = countBlocking(rep.Findings) == 0
	if !rep.Passed {
		return rep, fmt.Errorf("%w: equivalence verification failed", ErrStagingExportRefused)
	}
	return rep, nil
}

// verifyEquivalentProjectViews proves that child membership comes from the
// normalized FKs and that rebuilding the derived view is deterministic.  The
// exporter still carries schema-1 status names, so map them to the schema-2
// lifecycle field solely for this derived-data proof.
func verifyEquivalentProjectViews(rows map[string]map[string]map[string]any) error {
	s := wardendb.RelationshipState{Projects: map[string]wardendb.ProjectRecord{}, Agents: map[string]wardendb.AgentRecord{}, Terminals: map[string]wardendb.TerminalRecord{}, Plans: map[string]wardendb.PlanRecord{}, Pipelines: map[string]wardendb.PipelineRecord{}, Jobs: map[string]wardendb.PipelineJobRecord{}, Runs: map[string]wardendb.AutopilotRunRecord{}}
	decode := func(body map[string]any, out any) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, out)
	}
	for id, body := range rows["projects"] {
		var v wardendb.ProjectRecord
		if err := decode(body, &v); err != nil {
			return err
		}
		s.Projects[id] = v
	}
	for id, body := range rows["agents"] {
		var v wardendb.AgentRecord
		if err := decode(body, &v); err != nil {
			return err
		}
		v.Lifecycle = str(body["status"])
		if v.Lifecycle == "" {
			v.Lifecycle = "archived"
		}
		s.Agents[id] = v
	}
	for id, body := range rows["terminals"] {
		var v wardendb.TerminalRecord
		if err := decode(body, &v); err != nil {
			return err
		}
		v.Lifecycle = str(body["status"])
		s.Terminals[id] = v
	}
	for id, body := range rows["pipelines"] {
		var v wardendb.PipelineRecord
		if err := decode(body, &v); err != nil {
			return err
		}
		v.Lifecycle = str(body["status"])
		s.Pipelines[id] = v
	}
	for id, body := range rows["pipeline_jobs"] {
		var v wardendb.PipelineJobRecord
		if err := decode(body, &v); err != nil {
			return err
		}
		v.Lifecycle = str(body["status"])
		s.Jobs[id] = v
	}
	a, err := wardendb.RebuildProjectViews(s)
	if err != nil {
		return err
	}
	b, err := wardendb.RebuildProjectViews(s)
	if err != nil {
		return err
	}
	if err := wardendb.CompareProjectViews(b, s); err != nil {
		return err
	}
	return wardendb.CompareProjectViews(a, s)
}

func equivalentHash(rows map[string]map[string]any) string {
	h := sha256.New()
	for _, key := range sortedKeys(rows) {
		body := cloneMap(rows[key])
		delete(body, "_key")
		raw, _ := json.Marshal(body) // all rows originated from JSON-shaped records
		h.Write([]byte(key))
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil))
}
