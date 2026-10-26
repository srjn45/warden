// Frozen at approved schema-2 design commit dfe0d081. Do not derive from runtime types.
package schema3

var sourceCollections = map[string]bool{
	"agents":                     true,
	"archived_agents":            true,
	"terminals":                  true,
	"projects":                   true,
	"project_groups":             true,
	"plans":                      true,
	"plan_exports":               true,
	"pipelines":                  true,
	"pipeline_jobs":              true,
	"autopilot_runs":             true,
	"autopilots":                 true,
	"context_entries":            true,
	"messages":                   true,
	"backends":                   true,
	"backend_models":             true,
	"backend_roles":              true,
	"backend_settings":           true,
	"backend_quotas":             true,
	"backend_rl_cooldowns":       true,
	"model_catalog_scopes":       true,
	"model_catalog_snapshots":    true,
	"model_catalog_observations": true,
	"model_catalog_proposals":    true,
	"model_catalog_revisions":    true,
	"usage_snapshots":            true,
	"quota_impact_fences":        true,
	"snapshots":                  true,
	"savings_events":             true,
	"savings_calibration":        true,
	"spend_records":              true,
	"schedules":                  true,
	"known_prompts":              true,
	"plan_sync_envelopes":        true,
	"metrics_samples":            true,
	"id_sequences":               true,
	"operations":                 true,
	"plan_events":                true,
	"plan_notes":                 true,
	"audit_events":               true,
}
var frozenFields = map[string]string{
	"agents":         "id name type ticket tmux_session ai_cli ai_cli_session_id repo worktree branch base_branch worktree_created branch_created pr prompt workdir subject activity tags status pid exit_code created_at updated_at events last_pane_excerpt auto_restart restart_count last_restart_at permission_mode permission_posture execution_profile role task auto_approve force_compact pipeline_id job_id historical_pipeline plan_id schedule_id schedule_name parent_id child_agents child_pipelines child_autopilots seed_status seed_error autopilot_run_id autopilot_slot autopilot_task_id model quota_binding project_id hibernated context_tokens context_state context_checked_at last_compact_at rate_limited_at rate_limit_restore_at rate_limit_retry_count backend_recovery_generation backend_recovery runtime backend claude_session_id scope lifecycle archived_at closed_at",
	"terminals":      "id project_id name tmux_session workdir shell pid status exit_code created_at updated_at runtime",
	"projects":       "id name permission_posture path status agents pipelines terminals plans autopilots created_at updated_at",
	"project_groups": "id name project_ids created_at updated_at",
	"plans":          "id project_id name file_path goal constraints done_when tasks status execution_mode autopilot_run_id pipeline_id historical_pipeline orchestrator_id task_progress plan_branches created_at updated_at started_at completed_at archived_at archived_from revision content_hash repo_export synced_at remote_id active_execution execution_history task_outcomes branch_summaries execution_summary cleanup_evidence outcome",
	"pipelines":      "id name repo status jobs tags schedule_id schedule_name plan_id project_id parent_agent_id",
	"autopilots":     "id project_id plan_id name manager_agent_id brain_agent_id parent_agent_id diagnostics created_at updated_at",
	"autopilot_runs": "run_id name repo plan_file plan_id project_id state integration_branch gate strategy delete_branch brain_id slot_scope last_progress_at progress_fp created_at updated_at",
}

const jobFields = "id prompt depends_on handoff worktree supervised type run_if role tier backend model auto_retry_count agent_id session_id status output branch restart_branch workdir system digest synthetic"

// Frozen top-level JSON wire kinds; nested business objects remain verbatim.
var frozenKinds = map[string]string{
	"agents":         "id:string name:string type:string ticket:string tmux_session:string ai_cli:string ai_cli_session_id:string repo:string worktree:string branch:string base_branch:string worktree_created:bool branch_created:bool pr:string prompt:string workdir:string subject:string activity:string tags:array status:string pid:number exit_code:number created_at:string updated_at:string events:array last_pane_excerpt:string auto_restart:bool restart_count:number last_restart_at:string permission_mode:string permission_posture:string execution_profile:object role:string task:string auto_approve:bool force_compact:bool pipeline_id:string job_id:string historical_pipeline:object plan_id:string schedule_id:string schedule_name:string parent_id:string child_agents:array child_pipelines:array child_autopilots:array seed_status:string seed_error:string autopilot_run_id:string autopilot_slot:string autopilot_task_id:string model:string quota_binding:object project_id:string hibernated:bool context_tokens:number context_state:string context_checked_at:string last_compact_at:string rate_limited_at:string rate_limit_restore_at:string rate_limit_retry_count:number backend_recovery_generation:number backend_recovery:object runtime:object",
	"terminals":      "id:string project_id:string name:string tmux_session:string workdir:string shell:string pid:number status:string exit_code:number created_at:string updated_at:string runtime:object",
	"projects":       "id:string name:string permission_posture:string path:string status:string agents:array pipelines:array terminals:array plans:array autopilots:array created_at:string updated_at:string",
	"project_groups": "id:string name:string project_ids:array created_at:string updated_at:string",
	"plans":          "id:string project_id:string name:string file_path:string goal:string constraints:array done_when:array tasks:array status:string execution_mode:string autopilot_run_id:string pipeline_id:string historical_pipeline:object orchestrator_id:string task_progress:object plan_branches:array created_at:string updated_at:string started_at:string completed_at:string archived_at:string archived_from:string revision:number content_hash:string repo_export:object synced_at:string remote_id:string active_execution:object execution_history:array task_outcomes:object branch_summaries:array execution_summary:object cleanup_evidence:object outcome:object",
	"pipelines":      "id:string name:string repo:string status:string jobs:array tags:array schedule_id:string schedule_name:string plan_id:string project_id:string parent_agent_id:string",
	"autopilots":     "id:string project_id:string plan_id:string name:string manager_agent_id:string brain_agent_id:string parent_agent_id:string diagnostics:object created_at:string updated_at:string",
	"autopilot_runs": "run_id:string name:string repo:string plan_file:string plan_id:string project_id:string state:string integration_branch:string gate:string strategy:string delete_branch:bool brain_id:string slot_scope:string last_progress_at:string progress_fp:string created_at:string updated_at:string",
	"job":            "id:string prompt:string depends_on:array handoff:string worktree:string supervised:bool type:string run_if:string role:string tier:string backend:string model:string auto_retry_count:number agent_id:string session_id:string status:string output:string branch:string restart_branch:string workdir:string system:bool digest:object",
}
