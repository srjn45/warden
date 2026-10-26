package schema3

import (
	"encoding/json"
	"strings"
)

// canonicalHash is deliberately frozen to schema-2 canonical.go's byte policy.
func canonicalHash(p Object, tasks []any) string {
	type task struct {
		ID     string   `json:"id"`
		Prompt string   `json:"prompt"`
		After  []string `json:"after"`
	}
	type definition struct {
		Name        string   `json:"name"`
		Goal        string   `json:"goal"`
		Constraints []string `json:"constraints"`
		DoneWhen    []string `json:"done_when"`
		Tasks       []task   `json:"tasks"`
	}
	stringsOf := func(v any) []string {
		a := []string{}
		for _, x := range list(v) {
			a = append(a, text(x))
		}
		return a
	}
	d := definition{text(p["name"]), text(p["goal"]), stringsOf(p["constraints"]), stringsOf(p["done_when"]), []task{}}
	for _, x := range tasks {
		o := object(x)
		d.Tasks = append(d.Tasks, task{text(o["id"]), text(o["prompt"]), stringsOf(o["after"])})
	}
	return Digest(d)
}
func dag(rows map[string]Object, field, c string, r *Report) {
	state := map[string]int{}
	var visit func(string)
	visit = func(id string) {
		if state[id] == 1 {
			r.block(c, id, field, "cycle")
			return
		}
		if state[id] == 2 {
			return
		}
		state[id] = 1
		for _, v := range list(rows[id][field]) {
			peer := text(v)
			if rows[peer] == nil {
				r.block(c, id, field, v)
			} else {
				visit(peer)
			}
		}
		state[id] = 2
	}
	for _, id := range keys(rows) {
		visit(id)
	}
}
func materializeTasks(g Generation, r *Report) {
	g["plan_tasks"] = map[string]Object{}
	for _, id := range keys(g["plans"]) {
		p := g["plans"][id]
		tasks := list(p["tasks"])
		progress := object(p["task_progress"])
		outcomes := object(p["task_outcomes"])
		active := object(p["active_execution"])
		ap := object(active["task_progress"])
		local := map[string]Object{}
		before := text(p["content_hash"])
		computed := canonicalHash(p, tasks)
		if before == "" || before != computed {
			r.block("plans", id, "content_hash", []string{before, computed})
		}
		for seq, x := range tasks {
			t := object(x)
			allowed(t, "id prompt after", r, "plans", id)
			tid := text(t["id"])
			if tid == "" || local[tid] != nil {
				r.block("plans", id, "tasks", tid)
				continue
			}
			status := text(progress[tid])
			if status == "" {
				status = "pending"
			}
			if !strings.Contains("|pending|in_progress|done|skipped|", "|"+status+"|") {
				r.block("plans", id, "task_progress", progress[tid])
			}
			row := clone(t)
			row["id"] = Tuple(id, tid)
			row["task_id"] = tid
			row["plan_id"] = id
			row["project_id"] = p["project_id"]
			row["seq"] = seq
			row["status"] = status
			if v, ok := outcomes[tid]; ok {
				row["outcome"] = v
			}
			if v, ok := ap[tid]; ok && text(v) != status {
				r.block("plans", id, "active_execution.task_progress", tid)
			}
			local[tid] = row
			g["plan_tasks"][Tuple(id, tid)] = row
		}
		for _, m := range []Object{progress, outcomes, ap} {
			for _, k := range keys(m) {
				if local[k] == nil {
					r.block("plans", id, "task evidence", k)
				}
			}
		}
		dag(local, "after", "plan_tasks", r)
		hydrated := []any{}
		for _, x := range tasks {
			t := object(x)
			if row := local[text(t["id"])]; row != nil {
				hydrated = append(hydrated, Object{"id": row["task_id"], "prompt": row["prompt"], "after": row["after"]})
			}
		}
		after := canonicalHash(p, hydrated)
		r.Plans[id] = Proof{p["revision"], before, after}
		if after != before {
			r.block("plans", id, "rehydrated hash", after)
		}
		if active != nil {
			delete(active, "task_progress")
			mode := text(active["execution_mode"])
			for _, mirror := range []struct{ field, mode string }{{"pipeline_id", "pipeline"}, {"autopilot_run_id", "autopilot"}, {"orchestrator_id", "orchestrator_worker"}} {
				if id := text(p[mirror.field]); id != "" {
					selection := text(active["executor_id"])
					if mapped := r.Executors[selection]; mapped != "" {
						selection = mapped
					}
					if mirror.mode != mode || id != selection {
						r.block("plans", id, mirror.field, "mirror disagrees with active executor")
					}
				}
			}

			executor := text(active["executor_id"])
			if mode == "autopilot" {
				if v := r.Executors[executor]; v != "" {
					executor = v
					active["executor_id"] = v
				}
			}
			var child Object
			switch mode {
			case "autopilot":
				child = g["autopilot_runs"][executor]
			case "pipeline":
				child = g["pipelines"][executor]
			case "orchestrator_worker":
				child = g["agents"][executor]
			case "manual":
			default:
				r.block("plans", id, "active_execution.execution_mode", mode)
			}
			if mode != "manual" && (child == nil || text(child["project_id"]) != text(p["project_id"]) || text(child["plan_id"]) != id) {
				r.block("plans", id, "active_execution.executor_id", executor)
			}
		}
		remove(p, "tasks task_progress task_outcomes pipeline_id autopilot_run_id orchestrator_id")
	}
}
func materializeJobs(g Generation, r *Report) {
	old := g["pipeline_jobs"]
	g["pipeline_jobs"] = map[string]Object{}
	accounted := map[string]bool{}
	for _, pid := range keys(g["pipelines"]) {
		p := g["pipelines"][pid]
		// Schema-2 toRecord uses Pipeline.MarshalJSON, which also writes these
		// computed response counters. Validate their provenance against the
		// embedded job authority; they are never schema-3 header fields.
		count, done := 0, 0
		for _, x := range list(p["jobs"]) {
			j := object(x)
			if text(j["type"]) != "span-out" && text(j["type"]) != "span-in" {
				count++
				if text(j["status"]) == "done" {
					done++
				}
			}
		}
		for field, want := range map[string]int{"job_count": count, "jobs_done": done} {
			if got, present := p[field]; present {
				n, ok := got.(json.Number)
				value, err := n.Int64()
				if !ok || err != nil || value != int64(want) {
					r.block("pipelines", pid, field, "computed counter disagrees with embedded jobs")
				}
			}
		}
		local := map[string]Object{}
		for seq, x := range list(p["jobs"]) {
			j := object(x)
			allowed(j, jobFields, r, "pipelines", pid)
			validateKinds(j, frozenKinds["job"], r, "pipelines", pid)
			jid := text(j["id"])
			if jid == "" || local[jid] != nil {
				r.block("pipelines", pid, "jobs", jid)
				continue
			}
			if status := text(j["status"]); status != "" && !strings.Contains("|pending|running|done|failed|skipped|needs_attention|", "|"+status+"|") {
				r.block("pipelines", pid, "jobs.status", status)
			}
			row := clone(j)
			aid, sid := text(j["agent_id"]), text(j["session_id"])
			if aid != "" && sid != "" && aid != sid {
				r.block("pipelines", pid, "jobs.assignment", []string{aid, sid})
			}
			if aid == "" {
				aid = sid
			}
			if aid != "" {
				a := g["agents"][aid]
				if a == nil {
					r.block("pipelines", pid, "jobs.agent_id", aid)
				} else {
					agree(a, "pipeline_id", pid, r, "agents", aid)
					agree(a, "job_id", jid, r, "agents", aid)
					agree(a, "project_id", text(p["project_id"]), r, "agents", aid)
				}
			}
			key := Tuple(pid, jid)
			oldKey := pid + ":" + jid
			r.Keys["embedded:"+key] = key
			if projection := old[oldKey]; projection != nil && text(projection["pipeline_id"]) == pid && text(projection["job_id"]) == jid {
				r.Keys["pipeline_jobs/"+oldKey] = key
			}
			accounted[oldKey] = true
			projection := old[oldKey]
			if projection != nil {
				allowed(projection, jobFields+" pipeline_id job_id seq", r, "pipeline_jobs", oldKey)
			}
			expectedProjection := clone(j)
			expectedProjection["id"] = oldKey
			expectedProjection["pipeline_id"] = pid
			expectedProjection["job_id"] = jid
			expectedProjection["seq"] = seq
			if projection == nil || !equal(projection, expectedProjection) {
				r.finding("pipeline_jobs", oldKey, "projection", "rebuilt", projection)
			}
			row["id"] = key
			row["job_id"] = jid
			row["pipeline_id"] = pid
			row["project_id"] = p["project_id"]
			row["seq"] = seq
			remove(row, "agent_id session_id")
			local[jid] = row
			g["pipeline_jobs"][key] = row
		}
		dag(local, "depends_on", "pipeline_jobs", r)
		remove(p, "jobs job_count jobs_done")
	}
	for _, k := range keys(old) {
		if !accounted[k] {
			allowed(old[k], jobFields+" pipeline_id job_id seq", r, "pipeline_jobs", k)
			r.finding("pipeline_jobs", k, "projection", "retired extra projection", old[k])
		}
	}
	live := map[string]string{}
	for _, id := range keys(g["agents"]) {
		a := g["agents"][id]
		pid, jid := text(a["pipeline_id"]), text(a["job_id"])
		if jid == "" {
			continue
		}
		key := Tuple(pid, jid)
		job := g["pipeline_jobs"][key]
		if job == nil || text(job["project_id"]) != text(a["project_id"]) {
			r.block("agents", id, "job_id", key)
		}
		if liveAgent(a) {
			if prior := live[key]; prior != "" {
				r.block("agents", id, "job_id", []string{prior, id})
			}
			live[key] = id
		}
	}
}
func liveAgent(a Object) bool {
	if text(a["lifecycle"]) == "archived" || a["archived_at"] != nil || a["closed_at"] != nil {
		return false
	}
	switch text(a["status"]) {
	case "done", "closed", "terminated", "failed", "exited":
		return false
	}
	return true
}
