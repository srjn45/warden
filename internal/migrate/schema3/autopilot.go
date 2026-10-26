package schema3

import (
	"encoding/json"
	"strings"
	"time"
)

func operational(s string) bool {
	return s == "starting" || s == "active" || s == "paused" || s == "degraded" || s == "healing"
}
func consolidate(g Generation, r *Report) {
	if g["autopilot_runs"] == nil {
		g["autopilot_runs"] = map[string]Object{}
	}
	paired := map[string]string{}
	for _, id := range keys(g["autopilots"]) {
		a := g["autopilots"][id]
		run := g["autopilot_runs"][id]
		rid := id
		plan := g["plans"][text(a["plan_id"])]
		active := object(plan["active_execution"])
		if run == nil && text(active["execution_mode"]) == "autopilot" {
			selection := text(active["executor_id"])
			if selection == id {
				candidates := []string{}
				for _, k := range keys(g["autopilot_runs"]) {
					rr := g["autopilot_runs"][k]
					if text(rr["plan_id"]) == text(a["plan_id"]) && operational(text(rr["state"])) {
						candidates = append(candidates, k)
					}
				}
				if len(candidates) == 1 {
					rid = candidates[0]
					run = g["autopilot_runs"][rid]
				} else if len(candidates) > 1 {
					r.block("autopilots", id, "active execution pairing", candidates)
				}
			} else if candidate := g["autopilot_runs"][selection]; candidate != nil && text(candidate["plan_id"]) == text(a["plan_id"]) {
				rid = selection
				run = candidate
			}
		}
		if prior := paired[rid]; prior != "" {
			r.block("autopilots", id, "run_id", []string{prior, id})
		}
		paired[rid] = id
		if run == nil {
			if plan == nil || text(active["execution_mode"]) != "autopilot" || text(active["executor_id"]) != id {
				r.block("autopilots", id, "active_execution", "unpaired live executor has no unique selection")
			}
			run = Object{"run_id": rid}
			g["autopilot_runs"][rid] = run
		} else if !operational(text(run["state"])) && text(run["state"]) != "registered" {
			r.block("autopilots", id, "state", "live entity paired to terminal registration")
		}
		r.Executors[id] = rid
		for _, f := range []string{"project_id", "plan_id", "parent_agent_id"} {
			agree(run, f, text(a[f]), r, "autopilot_runs", rid)
		}
		diag := object(a["diagnostics"])
		allowed(diag, "state integration_branch gate strategy slot_scope last_error repo plan_file", r, "autopilots", id)
		if s := text(diag["state"]); s != "" && !operational(s) {
			r.block("autopilots", id, "diagnostics.state", s)
		}
		for _, f := range keys(diag) {
			agree(run, f, text(diag[f]), r, "autopilot_runs", rid)
		}
		if text(run["name"]) == "" {
			run["name"] = a["name"]
		}
		for _, f := range []string{"created_at", "updated_at"} {
			v, old := text(a[f]), text(run[f])
			if v == "" {
				continue
			}
			vt, e := time.Parse(time.RFC3339Nano, v)
			ot, oe := time.Parse(time.RFC3339Nano, old)
			if e != nil || (old != "" && oe != nil) {
				r.block("autopilots", id, f, []string{old, v})
				continue
			}
			r.finding("autopilots", id, f, "timestamp provenance", []string{old, v})
			if old == "" || (f == "created_at" && vt.Before(ot)) || (f == "updated_at" && vt.After(ot)) {
				run[f] = v
			}
		}
		for _, pair := range []struct{ f, slot string }{{"manager_agent_id", "autopilot"}, {"brain_agent_id", "brain"}} {
			bindSlot(g, r, rid, text(a[pair.f]), pair.slot)
		}
	}
	delete(g, "autopilots")
	for _, rid := range keys(g["autopilot_runs"]) {
		o := g["autopilot_runs"][rid]
		state := text(o["state"])
		if !strings.Contains("|registered|disabled|starting|active|paused|stopped|degraded|healing|complete|", "|"+state+"|") {
			r.block("autopilot_runs", rid, "state", state)
		}
		bindSlot(g, r, rid, text(o["brain_id"]), "autopilot")
		delete(o, "brain_id")
	}
	for _, p := range g["plans"] {
		if mapped := r.Executors[text(p["autopilot_run_id"])]; mapped != "" {
			p["autopilot_run_id"] = mapped
		}
	}
	slots := map[string]string{}
	executors := map[string]string{}
	for _, id := range keys(g["agents"]) {
		a := g["agents"][id]
		rid := text(a["autopilot_run_id"])
		if mapped := r.Executors[rid]; mapped != "" {
			rid = mapped
			a["autopilot_run_id"] = rid
		}
		slot := text(a["autopilot_slot"])
		if rid != "" && (slot == "autopilot" || slot == "brain") && liveAgent(a) {
			key := Tuple(rid, slot)
			if prior := slots[key]; prior != "" {
				r.block("agents", id, "autopilot_slot", []string{prior, id})
			}
			slots[key] = id
		}
	}
	for _, rid := range keys(g["autopilot_runs"]) {
		o := g["autopilot_runs"][rid]
		pid := text(o["plan_id"])
		if pid != "" && operational(text(o["state"])) {
			if prior := executors[pid]; prior != "" {
				r.block("autopilot_runs", rid, "plan_id", []string{prior, rid})
			}
			executors[pid] = rid
		}
	}
}
func bindSlot(g Generation, r *Report, rid, id, slot string) {
	if id == "" {
		return
	}
	a := g["agents"][id]
	if a == nil {
		r.block("autopilot_runs", rid, slot, id)
		return
	}
	current := text(a["autopilot_run_id"])
	if mapped := r.Executors[current]; mapped != "" {
		a["autopilot_run_id"] = mapped
	}
	agree(a, "autopilot_run_id", rid, r, "agents", id)
	agree(a, "autopilot_slot", slot, r, "agents", id)
}

func contextDispatch(g Generation, r *Report) {
	// Match complete run prefixes rather than splitting IDs at dots.
	dispatch := map[string]Object{}
	retire := map[string]bool{}
	for _, key := range keys(g["context_entries"]) {
		o := g["context_entries"][key]
		if !strings.HasPrefix(key, "autopilot.") {
			continue
		}
		rid, kind := "", ""
		for _, candidate := range keys(g["autopilot_runs"]) {
			if tail, ok := strings.CutPrefix(key, "autopilot."+candidate+"."); ok {
				if rid != "" {
					r.block("context_entries", key, "key", "ambiguous run prefix")
				}
				rid, kind = candidate, tail
			}
		}
		for _, old := range keys(r.Executors) {
			if tail, ok := strings.CutPrefix(key, "autopilot."+old+"."); ok {
				mapped := r.Executors[old]
				if rid != "" && rid != mapped {
					r.block("context_entries", key, "key", "ambiguous identity")
				}
				rid, kind = mapped, tail
			}
		}
		if rid == "" {
			r.block("context_entries", key, "key", "unknown reserved run")
			continue
		}
		run := g["autopilot_runs"][rid]
		plan := g["plans"][text(run["plan_id"])]
		execution, current := dispatchExecution(plan, rid, r)
		switch {
		case kind == "tasks":
			var tasks []Object
			if err := json.Unmarshal([]byte(text(o["value"])), &tasks); err != nil {
				r.block("context_entries", key, "value", err.Error())
				continue
			}
			seen := map[string]bool{}
			for _, t := range tasks {
				allowed(t, "id state worker_id branch pr note updated_at", r, "context_entries", key)
				tid := text(t["id"])
				tk := Tuple(rid, tid)
				row := g["plan_tasks"][Tuple(text(run["plan_id"]), tid)]
				validTask := current && row != nil
				if !current {
					for _, task := range list(object(execution["snapshot"])["tasks"]) {
						if text(object(task)["id"]) == tid {
							validTask = true
						}
					}
				}
				if seen[tid] || !validTask || text(execution["id"]) == "" {
					r.block("context_entries", key, "tasks", tid)
					continue
				}
				seen[tid] = true
				s := text(t["state"])
				if !strings.Contains("|pending|assigned|in_progress|pr_open|gated|landed|", "|"+s+"|") {
					r.block("context_entries", key, "state", s)
				}
				if s == "landed" && !landingExists(g, rid, text(t["branch"]), t["pr"]) {
					r.block("context_entries", key, "landed", t)
				}
				if worker := text(t["worker_id"]); current && worker != "" {
					a := g["agents"][worker]
					if a == nil {
						r.block("context_entries", key, "worker_id", worker)
					} else {
						agree(a, "project_id", text(run["project_id"]), r, "agents", worker)
						agree(a, "plan_id", text(run["plan_id"]), r, "agents", worker)
						agree(a, "autopilot_run_id", rid, r, "agents", worker)
						agree(a, "autopilot_task_id", tid, r, "agents", worker)
					}
				}
				if outcome := object(row["outcome"]); current && outcome != nil {
					for _, pair := range []struct{ task, outcome string }{{"worker_id", "assigned_agent"}, {"branch", "branch"}} {
						a, b := text(t[pair.task]), text(outcome[pair.outcome])
						if a != "" && b != "" && a != b {
							r.block("context_entries", key, "outcome."+pair.outcome, []string{a, b})
						}
					}
				}
				d := clone(t)
				delete(d, "id")
				d["execution_id"] = execution["id"]
				if old := dispatch[tk]; old != nil && !equal(old, d) {
					r.block("context_entries", key, "dispatch", []Object{old, d})
				}
				dispatch[tk] = d
				if current {
					row["dispatch"] = d
				} else {
					evidence := object(execution["task_dispatch"])
					if evidence == nil {
						evidence = Object{}
						execution["task_dispatch"] = evidence
					}
					if prior, ok := evidence[tid]; ok && !equal(prior, d) {
						r.block("context_entries", key, "historical dispatch", tid)
					}
					evidence[tid] = d
				}
			}
			retire[key] = true
		case kind == "integration_branch":
			var branch string
			if err := json.Unmarshal([]byte(text(o["value"])), &branch); err != nil {
				r.block("context_entries", key, "value", err.Error())
			} else {
				agree(run, "integration_branch", branch, r, "autopilot_runs", rid)
			}
			retire[key] = true
		case kind == "surface":
			surfaceToRun(o, run, key, rid, r)
			retire[key] = true
		case strings.HasPrefix(kind, "tasks."):
			// Checked against the authoritative array in the second pass.
			retire[key] = true
		case kind == "landings" || kind == "journal" || kind == "restart_context" || strings.HasPrefix(kind, "fix."):
			mapped := "autopilot." + rid + "." + kind
			if mapped != key {
				if old := g["context_entries"][mapped]; old != nil && !equal(old["value"], o["value"]) {
					r.block("context_entries", key, "mapped evidence", mapped)
				} else {
					cp := clone(o)
					cp["key"] = mapped
					g["context_entries"][mapped] = cp
					retire[key] = true
					r.Keys[key] = mapped
				}
			}
		default:
			r.block("context_entries", key, "key", "unclassified reserved namespace")
		}
	}
	for _, key := range keys(retire) {
		o := g["context_entries"][key]
		for _, rid := range keys(g["autopilot_runs"]) {
			prefix := "autopilot." + rid + ".tasks."
			tail, ok := strings.CutPrefix(key, prefix)
			if !ok {
				for old, mapped := range r.Executors {
					if mapped == rid {
						tail, ok = strings.CutPrefix(key, "autopilot."+old+".tasks.")
						if ok {
							break
						}
					}
				}
			}
			if !ok {
				continue
			}
			field := ""
			tid := ""
			if x, ok := strings.CutSuffix(tail, ".state"); ok {
				tid, field = x, "state"
			} else if x, ok := strings.CutSuffix(tail, ".branch"); ok {
				tid, field = x, "branch"
			} else {
				r.block("context_entries", key, "key", "unknown overlay")
				continue
			}
			d := dispatch[Tuple(rid, tid)]
			value := text(o["value"])
			var s string
			if json.Unmarshal([]byte(value), &s) == nil {
				value = s
			}
			if d == nil || text(d[field]) != value {
				r.block("context_entries", key, "overlay", value)
			}
		}
		delete(g["context_entries"], key)
	}
}
func landingExists(g Generation, rid, branch string, pr any) bool {
	o := g["context_entries"]["autopilot."+rid+".landings"]
	var landings []Object
	if json.Unmarshal([]byte(text(o["value"])), &landings) != nil {
		return false
	}
	for _, l := range landings {
		if text(l["branch"]) == branch && branch != "" && text(l["sha"]) != "" && (pr == nil || equal(pr, l["pr"])) {
			return true
		}
	}
	return false
}

func dispatchExecution(plan Object, rid string, r *Report) (Object, bool) {
	matches := func(e Object) bool {
		id := text(e["executor_id"])
		if mapped := r.Executors[id]; mapped != "" {
			id = mapped
		}
		return id == rid && text(e["execution_mode"]) == "autopilot"
	}
	if active := object(plan["active_execution"]); matches(active) {
		return active, true
	}
	var selected Object
	for _, v := range list(plan["execution_history"]) {
		e := object(v)
		if matches(e) {
			if selected != nil {
				r.block("plans", text(plan["id"]), "execution_history", "ambiguous dispatch execution for "+rid)
				return nil, false
			}
			selected = e
		}
	}
	return selected, false
}

// surfaceToRun folds the schema-2 operator status surface ledger record
// (autopilot.<run>.surface) into first-class autopilot_runs fields. Every
// known field is carried verbatim; any unknown field, or a value that
// disagrees with one already on the run, blocks the migration.
func surfaceToRun(o Object, run Object, key, rid string, r *Report) {
	var surface Object
	if err := json.Unmarshal([]byte(text(o["value"])), &surface); err != nil {
		r.block("context_entries", key, "value", err.Error())
		return
	}
	allowed(surface, "last_diagnosis resolver final_pr awaiting_merge verified final_notified", r, "context_entries", key)
	nested := map[string]string{
		"last_diagnosis": "action confidence rationale fail_open_reason source outcome at",
		"resolver":       "attempts last_class last_task last_branch last_outcome last_at",
		"final_pr":       "number url head_sha gate fix_attempts state merged_at",
		"awaiting_merge": "since green_sha notified pr",
	}
	for _, f := range keys(surface) {
		v := surface[f]
		if fields, ok := nested[f]; ok {
			sub, isObj := v.(map[string]any)
			if !isObj {
				r.block("context_entries", key, f, "surface field is not an object")
				continue
			}
			allowed(Object(sub), fields, r, "context_entries", key+"#"+f)
		} else if _, isBool := v.(bool); !isBool && (f == "verified" || f == "final_notified") {
			r.block("context_entries", key, f, "surface field is not a boolean")
			continue
		}
		if old, present := run[f]; present && !equal(old, v) {
			r.block("autopilot_runs", rid, f, []any{old, v})
			continue
		}
		run[f] = v
		r.finding("autopilot_runs", rid, f, "surface folded into run record", v)
	}
}
