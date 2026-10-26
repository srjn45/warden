// Package schema3 implements the frozen schema-2 ownership mapping. It is an
// offline migration codec, never a runtime store or a bootstrap backfill.
package schema3

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const MigrationID = "0003-ownership-normalization"
const Policy = "ownership-v1"

type Object = map[string]any
type Generation map[string]map[string]Object

type Finding struct {
	Collection  string `json:"collection"`
	Key         string `json:"key"`
	Field       string `json:"field"`
	Evidence    any    `json:"evidence,omitempty"`
	Disposition string `json:"disposition"`
}
type Proof struct {
	Revision any    `json:"revision"`
	Before   string `json:"before"`
	After    string `json:"after"`
}
type Report struct {
	SourceLedgerHash   string            `json:"source_ledger_hash,omitempty"`
	SourceCounts       map[string]int    `json:"source_counts"`
	RemovedFields      map[string]string `json:"removed_fields"`
	RetiredCollections []string          `json:"retired_collections"`
	Policy             string            `json:"policy"`
	Findings           []Finding         `json:"findings"`
	Keys               map[string]string `json:"keys"`
	Executors          map[string]string `json:"executors"`
	Plans              map[string]Proof  `json:"plans"`
	Source             map[string]string `json:"source_digests"`
	Target             map[string]string `json:"target_digests"`
	Counts             map[string]int    `json:"target_counts"`
}

func (r *Report) finding(c, k, f, d string, v any) {
	r.Findings = append(r.Findings, Finding{c, k, f, v, d})
}
func (r *Report) block(c, k, f string, v any) { r.finding(c, k, f, "blocked", v) }
func (r *Report) Blocked() bool {
	for _, f := range r.Findings {
		if f.Disposition == "blocked" {
			return true
		}
	}
	return false
}
func text(v any) string   { s, _ := v.(string); return s }
func object(v any) Object { m, _ := v.(map[string]any); return m }
func list(v any) []any    { a, _ := v.([]any); return a }
func keys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func encode(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func equal(a, b any) bool { return bytes.Equal(encode(a), encode(b)) }
func clone[T any](v T) T {
	var out T
	d := json.NewDecoder(bytes.NewReader(encode(v)))
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		panic(err)
	}
	return out
}
func Tuple(owner, id string) string { return string(encode([]string{owner, id})) }
func Digest(v any) string           { return fmt.Sprintf("sha256:%x", sha256.Sum256(encode(v))) }
func Digests(g Generation) map[string]string {
	out := map[string]string{}
	for c, rows := range g {
		out[c] = Digest(rows)
	}
	return out
}
func remove(o Object, fields string) {
	for _, f := range strings.Fields(fields) {
		delete(o, f)
	}
}
func allowed(o Object, fields string, r *Report, c, k string) {
	a := map[string]bool{"_key": true}
	for _, f := range strings.Fields(fields) {
		a[f] = true
	}
	for _, f := range keys(o) {
		if !a[f] {
			r.block(c, k, f, "unknown schema-2 field")
		}
	}
}

// Map never changes its input. Every source collection is classified, including
// absent versus present-empty. Blocking findings invalidate the entire output.
func Map(source Generation) (Generation, *Report, error) {
	r := &Report{Policy: Policy, Keys: map[string]string{}, Executors: map[string]string{}, Plans: map[string]Proof{}, Source: Digests(source)}
	r.SourceCounts = map[string]int{}
	for c, rows := range source {
		r.SourceCounts[c] = len(rows)
	}
	r.RemovedFields = retiredFields()
	r.RetiredCollections = []string{"archived_agents", "autopilots"}
	g := clone(source)
	for _, k := range keys(g["archived_agents"]) {
		allowed(g["archived_agents"][k], frozenFields["agents"], r, "archived_agents", k)
		validateKinds(g["archived_agents"][k], frozenKinds["agents"], r, "archived_agents", k)
	}
	for _, c := range keys(g) {
		if !sourceCollections[c] {
			r.block(c, "", "collection", "unknown collection")
		}
	}
	for c, fields := range frozenFields {
		for _, k := range keys(g[c]) {
			allowed(g[c][k], fields, r, c, k)
			validateKinds(g[c][k], frozenKinds[c], r, c, k)
		}
	}
	if g["agents"] == nil {
		g["agents"] = map[string]Object{}
	}
	for _, k := range keys(g["archived_agents"]) {
		o := g["archived_agents"][k]
		if g["agents"][k] != nil {
			r.block("archived_agents", k, "id", "duplicate live/archive identity")
		} else {
			o["lifecycle"] = "archived"
			g["agents"][k] = o
		}
	}
	delete(g, "archived_agents")
	for _, k := range keys(g["agents"]) {
		o := g["agents"][k]
		if o["lifecycle"] == nil {
			o["lifecycle"] = "active"
		}
		if v := text(o["parent_id"]); v != "" {
			agree(o, "parent_agent_id", v, r, "agents", k)
		}
		delete(o, "parent_id")
	}
	for c, idfield := range map[string]string{"projects": "id", "project_groups": "id", "agents": "id", "terminals": "id", "plans": "id", "pipelines": "id", "autopilots": "id", "autopilot_runs": "run_id"} {
		for _, k := range keys(g[c]) {
			if text(g[c][k][idfield]) != k {
				r.block(c, k, idfield, g[c][k][idfield])
			}
		}
	}
	// Preserve the nil/empty/populated distinction while validating both directions.
	for _, spec := range []struct{ owner, field, child, ref string }{
		{"projects", "agents", "agents", "project_id"}, {"projects", "pipelines", "pipelines", "project_id"}, {"projects", "terminals", "terminals", "project_id"}, {"projects", "plans", "plans", "project_id"}, {"projects", "autopilots", "autopilots", "project_id"},
		{"agents", "child_agents", "agents", "parent_agent_id"}, {"agents", "child_pipelines", "pipelines", "parent_agent_id"}, {"agents", "child_autopilots", "autopilots", "parent_agent_id"},
	} {
		inverse(g, spec.owner, spec.field, spec.child, spec.ref, r)
	}
	for _, k := range keys(g["project_groups"]) {
		o := g["project_groups"][k]
		seen := map[string]bool{}
		seq := 0
		for _, v := range list(o["project_ids"]) {
			id := text(v)
			p := g["projects"][id]
			if p == nil {
				r.block("project_groups", k, "project_ids", v)
				continue
			}
			if seen[id] {
				r.finding("project_groups", k, "project_ids", "deduplicated", id)
				continue
			}
			seen[id] = true
			agree(p, "group_id", k, r, "projects", id)
			p["group_order"] = seq
			seq++
		}
		delete(o, "project_ids")
	}
	paths := map[string]string{}
	for _, id := range keys(g["projects"]) {
		path := text(g["projects"][id]["path"])
		if path != "" {
			if prior := paths[path]; prior != "" {
				r.block("projects", id, "path", []string{prior, id})
			}
			paths[path] = id
		}
	}
	consolidate(g, r)
	// Resolve only direct or structural-parent evidence. No path/tag heuristics.
	for i := 0; i <= len(g["agents"])+2; i++ {
		for _, c := range []string{"agents", "pipelines", "autopilot_runs"} {
			for _, k := range keys(g[c]) {
				o := g[c][k]
				for _, ref := range []struct{ f, c string }{{"parent_agent_id", "agents"}, {"plan_id", "plans"}, {"pipeline_id", "pipelines"}, {"autopilot_run_id", "autopilot_runs"}} {
					id := text(o[ref.f])
					if id == "" {
						continue
					}
					p := g[ref.c][id]
					if p == nil {
						continue
					}
					if owner := text(p["project_id"]); owner != "" {
						agree(o, "project_id", owner, r, c, k)
					}
				}
			}
		}
	}
	pruneStaleAgents(g, r)
	for _, c := range []string{"agents", "terminals", "plans", "pipelines", "autopilot_runs"} {
		for _, k := range keys(g[c]) {
			o := g[c][k]
			pid := text(o["project_id"])
			if c == "agents" && pid == "" && (text(o["scope"]) == "global" || text(o["scope"]) == "system") {
				continue
			}
			if pid == "" || g["projects"][pid] == nil {
				r.block(c, k, "project_id", pid)
			}
			for _, ref := range []struct{ f, c string }{{"parent_agent_id", "agents"}, {"plan_id", "plans"}, {"pipeline_id", "pipelines"}, {"autopilot_run_id", "autopilot_runs"}} {
				id := text(o[ref.f])
				if id == "" {
					continue
				}
				p := g[ref.c][id]
				if p == nil || text(p["project_id"]) != pid || pid == "" {
					r.block(c, k, ref.f, id)
				}
			}
		}
	}
	for _, rel := range []struct{ f, c string }{{"agents", "agents"}, {"pipelines", "pipelines"}, {"plans", "plans"}, {"terminals", "terminals"}} {
		inverse(g, "projects", rel.f, rel.c, "project_id", r)
	}
	validateCycles(g["agents"], "parent_agent_id", "agents", r)
	materializeJobs(g, r)
	materializeTasks(g, r)
	contextDispatch(g, r)
	for _, c := range []string{"plan_events", "plan_notes", "plan_exports"} {
		for _, k := range keys(g[c]) {
			o := g[c][k]
			p := g["plans"][text(o["plan_id"])]
			if p == nil {
				r.block(c, k, "plan_id", o["plan_id"])
			} else {
				agree(o, "project_id", text(p["project_id"]), r, c, k)
			}
		}
	}
	for _, k := range keys(g["operations"]) {
		state := text(g["operations"][k]["state"])
		if !strings.Contains("|succeeded|compensated|canceled|failed|", "|"+state+"|") {
			r.block("operations", k, "state", state)
		}
	}
	for _, o := range g["projects"] {
		remove(o, "agents pipelines terminals plans autopilots")
	}
	for _, o := range g["agents"] {
		remove(o, "child_agents child_pipelines child_autopilots")
	}
	if !r.Blocked() {
		if err := ValidateTarget(g); err != nil {
			r.block("target", "", "integrity", err.Error())
		}
	}
	r.Target = Digests(g)
	r.Counts = map[string]int{}
	for c, rows := range g {
		r.Counts[c] = len(rows)
	}
	sort.Slice(r.Findings, func(i, j int) bool { return string(encode(r.Findings[i])) < string(encode(r.Findings[j])) })
	if r.Blocked() {
		return nil, r, fmt.Errorf("schema3: ownership mapping blocked; inspect findings")
	}
	return g, r, nil
}
func agree(o Object, f, v string, r *Report, c, k string) {
	if v == "" {
		return
	}
	old := text(o[f])
	if old != "" && old != v {
		r.block(c, k, f, []string{old, v})
		return
	}
	if old == "" {
		o[f] = v
		r.finding(c, k, f, "initialized", v)
	}
}
func inverse(g Generation, oc, f, cc, ref string, r *Report) {
	for _, k := range keys(g[oc]) {
		o := g[oc][k]
		v, exists := o[f]
		if !exists || v == nil {
			continue
		}
		a, ok := v.([]any)
		if !ok {
			r.block(oc, k, f, v)
			continue
		}
		seen := map[string]bool{}
		for _, x := range a {
			id := text(x)
			child := g[cc][id]
			if child == nil {
				r.finding(oc, k, f, "dropped missing child", x)
				continue
			}
			if seen[id] {
				r.finding(oc, k, f, "deduplicated", id)
			}
			seen[id] = true
			agree(child, ref, k, r, cc, id)
		}
		// Every forward list mapped here is a retired field, and the child's own
		// back-reference is what schema 3 keeps. A list that missed a child is
		// therefore stale bookkeeping, not conflicting ownership.
		for _, id := range keys(g[cc]) {
			if text(g[cc][id][ref]) == k && !seen[id] {
				r.finding(oc, k, f, "derived from child", id)
			}
		}
	}
}

// pruneStaleAgents drops history schema 2 accumulated but schema 3 cannot own.
// An agent whose autopilot run no longer exists loses that run binding. An
// archived agent that still has no project after structural resolution is
// deleted, and references to it are cleared. Live agents are never deleted:
// a project-less live agent stays blocked. The paired backup keeps every
// original row.
func pruneStaleAgents(g Generation, r *Report) {
	for _, k := range keys(g["agents"]) {
		o := g["agents"][k]
		if rid := text(o["autopilot_run_id"]); rid != "" && g["autopilot_runs"][rid] == nil {
			r.finding("agents", k, "autopilot_run_id", "cleared missing run", rid)
			remove(o, "autopilot_run_id autopilot_slot autopilot_task_id")
		}
		// A task binding is scoped to a run; without one it names nothing.
		if tid := text(o["autopilot_task_id"]); tid != "" && text(o["autopilot_run_id"]) == "" {
			r.finding("agents", k, "autopilot_task_id", "cleared task without run", tid)
			delete(o, "autopilot_task_id")
		}
	}
	deleted := map[string]bool{}
	for _, k := range keys(g["agents"]) {
		o := g["agents"][k]
		scope := text(o["scope"])
		if text(o["project_id"]) == "" && scope != "global" && scope != "system" && text(o["lifecycle"]) == "archived" {
			r.finding("agents", k, "project_id", "deleted archived without project", text(o["name"]))
			delete(g["agents"], k)
			deleted[k] = true
		}
	}
	for _, c := range []string{"agents", "pipelines", "autopilot_runs"} {
		for _, k := range keys(g[c]) {
			if id := text(g[c][k]["parent_agent_id"]); deleted[id] {
				r.finding(c, k, "parent_agent_id", "cleared deleted parent", id)
				delete(g[c][k], "parent_agent_id")
			}
		}
	}
	// A plan's executor mirrors are retired fields. One naming an executor
	// that no longer exists carries no ownership and is dropped early, so it
	// is not validated as a live reference.
	for _, k := range keys(g["plans"]) {
		p := g["plans"][k]
		for _, m := range []struct{ f, c string }{{"autopilot_run_id", "autopilot_runs"}, {"pipeline_id", "pipelines"}, {"orchestrator_id", "agents"}} {
			if id := text(p[m.f]); id != "" && g[m.c][id] == nil {
				r.finding("plans", k, m.f, "dropped missing executor mirror", id)
				delete(p, m.f)
			}
		}
	}
}

func validateCycles(rows map[string]Object, ref, c string, r *Report) {
	for _, k := range keys(rows) {
		seen := map[string]bool{}
		for id := k; id != ""; id = text(rows[id][ref]) {
			if seen[id] {
				r.block(c, k, ref, "cycle at "+id)
				break
			}
			seen[id] = true
		}
	}
}

func validateKinds(o Object, spec string, r *Report, c, k string) {
	for _, pair := range strings.Fields(spec) {
		f, kind, _ := strings.Cut(pair, ":")
		v, present := o[f]
		if !present || v == nil {
			continue
		}
		valid := false
		switch kind {
		case "string":
			_, valid = v.(string)
		case "bool":
			_, valid = v.(bool)
		case "number":
			_, valid = v.(json.Number)
		case "array":
			_, valid = v.([]any)
		case "object":
			_, valid = v.(map[string]any)
		}
		if !valid {
			r.block(c, k, f, "invalid JSON type: expected "+kind)
		}
		if kind == "string" && (strings.HasSuffix(f, "_at") || f == "created_at" || f == "updated_at") && text(v) != "" {
			if _, err := time.Parse(time.RFC3339Nano, text(v)); err != nil {
				r.block(c, k, f, "invalid timestamp")
			}
		}
	}
}

func retiredFields() map[string]string {
	return map[string]string{"projects": "agents pipelines terminals plans autopilots", "project_groups": "project_ids", "agents": "parent_id child_agents child_pipelines child_autopilots", "plans": "tasks task_progress task_outcomes pipeline_id autopilot_run_id orchestrator_id active_execution.task_progress", "pipelines": "jobs job_count jobs_done", "pipeline_jobs": "agent_id session_id", "autopilot_runs": "brain_id"}
}
