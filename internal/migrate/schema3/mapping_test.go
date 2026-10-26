package schema3

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func fixture() Generation {
	p := Object{"id": "plan", "project_id": "p", "name": "ordered", "goal": "goal", "constraints": []any{"c"}, "done_when": []any{"d"}, "revision": json.Number("7"), "tasks": []any{Object{"id": "z", "prompt": "first"}, Object{"id": "a", "prompt": "second", "after": []any{"z"}}}, "task_progress": Object{"z": "done", "a": "in_progress"}, "task_outcomes": Object{"z": Object{"task_id": "z", "note": "verified", "verified_checks": []any{"test"}}}, "execution_history": []any{Object{"snapshot": Object{"tasks": []any{Object{"id": "old", "prompt": "original"}}}}}}
	p["content_hash"] = canonicalHash(p, list(p["tasks"]))
	return Generation{"projects": {"p": {"id": "p", "path": "", "agents": nil, "pipelines": nil, "plans": nil}}, "agents": {"worker": {"id": "worker", "project_id": "p", "pipeline_id": "pipe", "job_id": "job", "prompt": "complete prompt", "runtime": Object{"opaque": "runtime payload"}, "permission_posture": "explicit", "status": "idle"}}, "archived_agents": {"past": {"id": "past", "project_id": "p", "prompt": "preserved archived prompt", "status": "done"}}, "plans": {"plan": p}, "pipelines": {"pipe": {"id": "pipe", "project_id": "p", "jobs": []any{Object{"id": "job", "prompt": "job prompt", "status": "running", "agent_id": "worker", "session_id": "worker", "auto_retry_count": json.Number("3"), "digest": Object{"summary": "digest"}, "output": "output", "restart_branch": "resume"}}}}, "pipeline_jobs": {"pipe:job": {"id": "pipe:job", "pipeline_id": "pipe", "job_id": "job", "seq": json.Number("0"), "prompt": "stale"}}, "messages": {"m": {"value": "unchanged", "counter": json.Number("9007199254740991")}}}
}
func TestMappingPayloadOrderAndHash(t *testing.T) {
	g := fixture()
	before := Digest(g)
	out, r, err := Map(g)
	require.NoError(t, err)
	require.False(t, r.Blocked())
	require.Equal(t, before, Digest(g))
	require.Equal(t, g["agents"]["worker"]["runtime"], out["agents"]["worker"]["runtime"])
	require.Equal(t, g["messages"], out["messages"])
	require.Contains(t, out["agents"], "past")
	require.NotContains(t, out, "archived_agents")
	require.NotContains(t, out["plans"]["plan"], "tasks")
	require.NotContains(t, out["pipelines"]["pipe"], "jobs")
	require.Equal(t, "output", out["pipeline_jobs"][Tuple("pipe", "job")]["output"])
	require.NotContains(t, out["pipeline_jobs"][Tuple("pipe", "job")], "agent_id")
	require.Equal(t, 0, out["plan_tasks"][Tuple("plan", "z")]["seq"])
	require.Equal(t, 1, out["plan_tasks"][Tuple("plan", "a")]["seq"])
	require.Equal(t, r.Plans["plan"].Before, r.Plans["plan"].After)
	require.Equal(t, g["plans"]["plan"]["execution_history"], out["plans"]["plan"]["execution_history"])
	_, r2, err := Map(g)
	require.NoError(t, err)
	require.Equal(t, Digest(r), Digest(r2))
}
func TestMappingConflictsAbortWholeGeneration(t *testing.T) {
	for name, mutate := range map[string]func(Generation){
		"empty authority":        func(g Generation) { g["projects"]["p"]["agents"] = []any{} },
		"two owners":             func(g Generation) { g["projects"]["q"] = Object{"id": "q", "agents": []any{"worker"}} },
		"dangling":               func(g Generation) { g["projects"]["p"]["agents"] = []any{"absent"} },
		"parent cycle":           func(g Generation) { g["agents"]["worker"]["parent_id"] = "worker" },
		"cross project":          func(g Generation) { g["projects"]["q"] = Object{"id": "q"}; g["agents"]["worker"]["project_id"] = "q" },
		"archive duplicate":      func(g Generation) { g["archived_agents"]["worker"] = clone(g["agents"]["worker"]) },
		"job alias disagreement": func(g Generation) { object(list(g["pipelines"]["pipe"]["jobs"])[0])["session_id"] = "other" },
		"unknown job":            func(g Generation) { object(list(g["pipelines"]["pipe"]["jobs"])[0])["owner_list"] = []any{} },
		"extra task evidence":    func(g Generation) { object(g["plans"]["plan"]["task_progress"])["unknown"] = "done" },
		"wrong hash":             func(g Generation) { g["plans"]["plan"]["content_hash"] = "wrong" },
		"task cycle": func(g Generation) {
			p := g["plans"]["plan"]
			object(list(p["tasks"])[0])["after"] = []any{"a"}
			p["content_hash"] = canonicalHash(p, list(p["tasks"]))
		},
		"pending operation":        func(g Generation) { g["operations"] = map[string]Object{"op": {"state": "running"}} },
		"unknown collection":       func(g Generation) { g["new_ownership"] = map[string]Object{} },
		"unknown header ownership": func(g Generation) { g["agents"]["worker"]["owner_id"] = "other" },
		"two active attempts": func(g Generation) {
			g["agents"]["second"] = clone(g["agents"]["worker"])
			g["agents"]["second"]["id"] = "second"
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := fixture()
			mutate(g)
			before := Digest(g)
			out, r, err := Map(g)
			require.Error(t, err)
			require.Nil(t, out)
			require.True(t, r.Blocked())
			require.Equal(t, before, Digest(g))
		})
	}
}
func TestDuplicateClaimsAndMissingOwner(t *testing.T) {
	g := fixture()
	delete(g["agents"]["worker"], "project_id")
	g["projects"]["p"]["agents"] = []any{"worker", "worker", "past"}
	out, r, err := Map(g)
	require.NoError(t, err)
	require.Equal(t, "p", out["agents"]["worker"]["project_id"])
	require.Contains(t, string(encode(r)), "deduplicated")
}
func TestTupleCollisions(t *testing.T) {
	require.NotEqual(t, Tuple("a:b", "c"), Tuple("a", "b:c"))
	require.NotEqual(t, Tuple(`a","b`, "c"), Tuple("a", `b","c`))
}
func TestFrozenCanonicalHashMatchesProduction(t *testing.T) {
	g := fixture()
	raw := encode(g["plans"]["plan"])
	var p planstore.Plan
	require.NoError(t, json.Unmarshal(raw, &p))
	require.Equal(t, planstore.ComputeContentHash(&p), canonicalHash(g["plans"]["plan"], list(g["plans"]["plan"]["tasks"])))
}
func TestActualSchema2CodecsPreserveBusinessFields(t *testing.T) {
	var a agentstore.Agent
	a.ID = "worker"
	a.ProjectID = "p"
	a.Prompt = "full prompt"
	a.Model = "custom"
	a.PR = "https://example.test/1"
	a.AutoApprove = true
	a.Role = "worker"
	a.AutopilotTaskID = ""
	a.PlanID = "plan"
	var body Object
	require.NoError(t, json.Unmarshal(encode(a), &body))
	g := fixture()
	g["agents"]["worker"] = body
	j := pipeline.Job{ID: "job", Prompt: "original", Status: pipeline.JobStatus("done"), Output: "output", Model: "model", AutoRetryCount: 3, Supervised: true, Branch: "branch", RestartBranch: "restart", Workdir: "workdir", System: true}
	var jb Object
	require.NoError(t, json.Unmarshal(encode(j), &jb))
	g["pipelines"]["pipe"]["jobs"] = []any{jb}
	out, _, err := Map(g)
	require.NoError(t, err)
	for k, v := range body {
		if k != "child_agents" && k != "child_pipelines" && k != "child_autopilots" {
			require.Equal(t, Digest(v), Digest(out["agents"]["worker"][k]), k)
		}
	}
	for k, v := range jb {
		if k != "id" && k != "agent_id" && k != "session_id" {
			require.Equal(t, Digest(v), Digest(out["pipeline_jobs"][Tuple("pipe", "job")][k]), k)
		}
	}
}
func TestFullCatalogueClassified(t *testing.T) {
	require.ElementsMatch(t, wardendb.Catalogue(), keys(sourceCollections))
	g := fixture()
	for _, name := range wardendb.Catalogue() {
		if g[name] == nil {
			g[name] = map[string]Object{}
		}
	}
	_, r, err := Map(g)
	require.NoError(t, err)
	require.Len(t, r.Source, len(wardendb.Catalogue()))
}
func TestPhysicalStagingAndRawReader(t *testing.T) {
	g := fixture()
	out, r, err := Map(g)
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), ".schema3-staging-test")
	require.NoError(t, WriteStaging(context.Background(), dir, out, Indexes{"messages": {{Field: "value"}}}))
	read, _, err := Read(context.Background(), dir)
	require.NoError(t, err)
	require.Equal(t, Digest(r.Target), Digest(Digests(read)))
	require.Equal(t, json.Number("9007199254740991"), read["messages"]["m"]["counter"])
	require.Error(t, WriteStaging(context.Background(), dir, out, nil))
	for _, name := range []string{"archived_agents", "autopilots"} {
		_, err := os.Stat(filepath.Join(dir, name))
		require.True(t, os.IsNotExist(err))
	}
}
func TestUnsafeInputAndCorruptionRefused(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Symlink("/tmp", filepath.Join(dir, "link")))
	_, _, err := Read(context.Background(), dir)
	require.ErrorContains(t, err, "unsafe")
}

func TestReadDerivesStableKeysForLegacySavingsEventsOnly(t *testing.T) {
	t.Run("append-only savings event", func(t *testing.T) {
		dir := t.TempDir()
		db, err := wardendb.Open(dir)
		require.NoError(t, err)
		ledger, err := savings.NewStoreFromDB(db)
		require.NoError(t, err)
		require.NoError(t, ledger.Record(savings.Event{Feature: "check", Agent: "legacy", RawTokens: 9, KeptTokens: 3, Saved: 6}))
		require.NoError(t, db.Close())

		g, _, err := Read(context.Background(), filepath.Join(dir, "warden-db"))
		require.NoError(t, err)
		require.Equal(t, Object{"feature": "check", "agent": "legacy", "raw_tokens": json.Number("9"), "kept_tokens": json.Number("3"), "saved": json.Number("6"), "ts": g["savings_events"]["savings_events/1"]["ts"]}, g["savings_events"]["savings_events/1"])
	})

	t.Run("other collection remains strict", func(t *testing.T) {
		dir := t.TempDir()
		db, err := wardendb.Open(dir)
		require.NoError(t, err)
		messages, err := wardendb.Register[map[string]any](db, wardendb.Spec{Name: "messages", Version: 1})
		require.NoError(t, err)
		col, err := messages.Engine()
		require.NoError(t, err)
		_, _, err = col.Insert(map[string]any{"value": "missing logical key"})
		require.NoError(t, err)
		require.NoError(t, db.Close())

		_, _, err = Read(context.Background(), filepath.Join(dir, "warden-db"))
		require.ErrorContains(t, err, "keyless record messages/1")
	})
}

func TestPhysicalStagingRejectsLossyEngineNumbersBeforeWrite(t *testing.T) {
	g, _, err := Map(fixture())
	require.NoError(t, err)
	g["messages"]["m"]["counter"] = json.Number("9007199254740993")
	dir := filepath.Join(t.TempDir(), ".schema3-staging-lossy")
	require.ErrorContains(t, WriteStaging(context.Background(), dir, g, nil), "losslessly")
	_, err = os.Stat(dir)
	require.True(t, os.IsNotExist(err))
}
