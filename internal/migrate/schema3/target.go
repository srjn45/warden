package schema3

import (
	"fmt"
	"strconv"
	"strings"
)

// ValidateTarget independently checks the physical writer's input, including
// forbidden null/empty fields. It never relies on an earlier Map invocation.
func ValidateTarget(input Generation) error {
	g := clone(input)
	r := &Report{}
	for _, c := range keys(g) {
		if !sourceCollections[c] && c != "plan_tasks" || c == "archived_agents" || c == "autopilots" {
			r.block(c, "", "collection", "retired/unknown target collection")
		}
	}
	for c, fields := range frozenFields {
		if c == "autopilots" {
			continue
		}
		allowedFields := map[string]bool{}
		for _, f := range strings.Fields(fields) {
			allowedFields[f] = true
		}
		for _, f := range strings.Fields(retiredFields()[c]) {
			delete(allowedFields, f)
		}
		extras := map[string]string{"projects": "group_id group_order", "agents": "parent_agent_id lifecycle archived_at scope", "autopilot_runs": "parent_agent_id last_error"}
		for _, f := range strings.Fields(extras[c]) {
			allowedFields[f] = true
		}
		for _, k := range keys(g[c]) {
			o := g[c][k]
			allowed(o, strings.Join(keys(allowedFields), " "), r, c, k)
			if c == "plans" {
				if _, present := object(o["active_execution"])["task_progress"]; present {
					r.block(c, k, "active_execution.task_progress", "retired field")
				}
			}
		}
	}
	for _, c := range []string{"projects", "project_groups", "agents", "terminals", "plans", "pipelines", "autopilot_runs"} {
		for _, id := range keys(g[c]) {
			o := g[c][id]
			field := "id"
			if c == "autopilot_runs" {
				field = "run_id"
			}
			if text(o[field]) != id {
				r.block(c, id, field, o[field])
			}
			if c == "projects" || c == "project_groups" {
				continue
			}
			pid := text(o["project_id"])
			if c == "agents" && pid == "" && (text(o["scope"]) == "global" || text(o["scope"]) == "system") {
				continue
			}
			if g["projects"][pid] == nil {
				r.block(c, id, "project_id", pid)
			}
			for _, ref := range []struct{ f, c string }{{"parent_agent_id", "agents"}, {"plan_id", "plans"}, {"pipeline_id", "pipelines"}, {"autopilot_run_id", "autopilot_runs"}} {
				owner := text(o[ref.f])
				if owner != "" {
					p := g[ref.c][owner]
					if p == nil || text(p["project_id"]) != pid {
						r.block(c, id, ref.f, owner)
					}
				}
			}
		}
	}
	paths, orders := map[string]string{}, map[string]string{}
	for _, id := range keys(g["projects"]) {
		p := g["projects"][id]
		if path := text(p["path"]); path != "" {
			if prior := paths[path]; prior != "" {
				r.block("projects", id, "path", prior)
			}
			paths[path] = id
		}
		if group := text(p["group_id"]); group != "" {
			if g["project_groups"][group] == nil {
				r.block("projects", id, "group_id", group)
			}
			key := Tuple(group, string(encode(p["group_order"])))
			if prior := orders[key]; prior != "" {
				r.block("projects", id, "group_order", prior)
			}
			orders[key] = id
		}
	}
	for _, spec := range []struct{ c, owner, ref, local, fields, dep string }{{"pipeline_jobs", "pipelines", "pipeline_id", "job_id", jobFields + " project_id pipeline_id job_id seq", "depends_on"}, {"plan_tasks", "plans", "plan_id", "task_id", "id task_id plan_id project_id prompt after seq status outcome dispatch", "after"}} {
		owners := map[string]map[string]Object{}
		sequences := map[string]map[int]string{}
		for _, key := range keys(g[spec.c]) {
			o := g[spec.c][key]
			allowed(o, spec.fields, r, spec.c, key)
			for _, f := range []string{"agent_id", "session_id"} {
				if _, exists := o[f]; exists {
					r.block(spec.c, key, f, "retired field")
				}
			}
			owner, local := text(o[spec.ref]), text(o[spec.local])
			p := g[spec.owner][owner]
			if p == nil || text(p["project_id"]) != text(o["project_id"]) {
				r.block(spec.c, key, spec.ref, owner)
			}
			if text(o["id"]) != key || key != Tuple(owner, local) || local == "" {
				r.block(spec.c, key, "id", "invalid tuple identity")
			}
			seq, e := strconv.Atoi(string(encode(o["seq"])))
			if e != nil || seq < 0 {
				r.block(spec.c, key, "seq", o["seq"])
			}
			if owners[owner] == nil {
				owners[owner] = map[string]Object{}
				sequences[owner] = map[int]string{}
			}
			if prior := sequences[owner][seq]; prior != "" {
				r.block(spec.c, key, "seq", prior)
			}
			sequences[owner][seq] = key
			owners[owner][local] = o
		}
		for owner, rows := range owners {
			dag(rows, spec.dep, spec.c, r)
			for seq := 0; seq < len(rows); seq++ {
				if sequences[owner][seq] == "" {
					r.block(spec.c, owner, "seq", "noncontiguous sequence")
				}
			}
		}
		if spec.c == "plan_tasks" {
			for _, pid := range keys(g["plans"]) {
				tasks := []any{}
				rows := owners[pid]
				for seq := 0; seq < len(rows); seq++ {
					o := g[spec.c][sequences[pid][seq]]
					tasks = append(tasks, Object{"id": o["task_id"], "prompt": o["prompt"], "after": o["after"]})
				}
				if hash := canonicalHash(g["plans"][pid], tasks); hash != text(g["plans"][pid]["content_hash"]) {
					r.block("plans", pid, "content_hash", hash)
				}
			}
		}
	}
	validateCycles(g["agents"], "parent_agent_id", "agents", r)
	liveJobs, slots := map[string]string{}, map[string]string{}
	for _, id := range keys(g["agents"]) {
		a := g["agents"][id]
		if jid := text(a["job_id"]); jid != "" {
			key := Tuple(text(a["pipeline_id"]), jid)
			job := g["pipeline_jobs"][key]
			if job == nil || text(job["project_id"]) != text(a["project_id"]) {
				r.block("agents", id, "job_id", key)
			}
			if liveAgent(a) {
				if prior := liveJobs[key]; prior != "" {
					r.block("agents", id, "job_id", prior)
				}
				liveJobs[key] = id
			}
		}
		rid, slot := text(a["autopilot_run_id"]), text(a["autopilot_slot"])
		if rid != "" && (slot == "autopilot" || slot == "brain") && liveAgent(a) {
			key := Tuple(rid, slot)
			if prior := slots[key]; prior != "" {
				r.block("agents", id, "autopilot_slot", prior)
			}
			slots[key] = id
		}
		if tid := text(a["autopilot_task_id"]); tid != "" {
			run := g["autopilot_runs"][rid]
			row := g["plan_tasks"][Tuple(text(run["plan_id"]), tid)]
			if run == nil || row == nil || text(row["project_id"]) != text(a["project_id"]) || (text(a["plan_id"]) != "" && text(a["plan_id"]) != text(run["plan_id"])) {
				r.block("agents", id, "autopilot_task_id", tid)
			}
		}
	}
	if r.Blocked() {
		return fmt.Errorf("schema3: invalid target: %s", encode(r.Findings))
	}
	return nil
}
