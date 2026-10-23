// Single source for the landing-page feature grid. Keep in sync with
// docs/FEATURES.md: every entry cites the section it is drawn from in
// `section`, and only shipped features belong here. Do not add claims that
// have no section in that catalog.
export interface Feature {
  title: string;
  desc: string;
  /** docs/FEATURES.md section this entry is drawn from, e.g. "§34.7". */
  section: string;
  /** Docs page path under the site base, no leading or trailing slash. */
  href: string;
}

export interface FeatureGroup {
  id: string;
  title: string;
  blurb: string;
  features: Feature[];
}

export const featureGroups: FeatureGroup[] = [
  {
    id: 'run-work',
    title: 'Run work',
    blurb: 'Spawn agents, isolate their changes, and keep them coordinated.',
    features: [
      { title: 'Spawn and classify', desc: 'Start agents from the CLI, TUI, web or MCP, tagged by task type.', section: '§2', href: 'guides/spawn-and-watch' },
      { title: 'Worktrees and task types', desc: 'Each agent works in its own git worktree, so parallel changes stay apart.', section: '§2', href: 'concepts/worktrees-task-types' },
      { title: 'Pipelines', desc: 'Chain dependent agent jobs into a DAG of stages.', section: '§6', href: 'multi-agent/pipelines' },
      { title: 'Shared context and messages', desc: 'Agents coordinate through a shared blackboard and directed messages.', section: '§6', href: 'multi-agent/shared-context-messages' },
      { title: 'Handoff and fork', desc: 'Rotate an agent into a fresh session with a digest, or fork a session sideways.', section: '§7', href: 'guides/rotation-digests' },
      { title: 'Scheduling', desc: 'Run agents or pipelines on a recurring schedule with the native scheduler.', section: '§28', href: 'guides/scheduling' },
    ],
  },
  {
    id: 'autopilot-and-plans',
    title: 'Autopilot and plans',
    blurb: 'Describe a goal; a manager delegates and lands the work.',
    features: [
      { title: 'Autopilot runs', desc: 'A manager splits a plan into tasks and delegates each to a worker in an isolated worktree.', section: '§34', href: 'guides/autopilot' },
      { title: 'CI-gated land', desc: 'Each worker PR is gated through CI and landed on a per-plan integration branch, never directly on main.', section: '§34.7', href: 'concepts/autopilot' },
      { title: 'Guardian and overwatch', desc: 'A guardian loop restarts a wedged manager and nudges an idle one.', section: '§34.3', href: 'concepts/autopilot' },
      { title: 'Tracked plans', desc: 'Plans have a lifecycle, execution modes and recovery, driven from the CLI, MCP and TUI.', section: '§37', href: 'guides/using-plans' },
    ],
  },
  {
    id: 'backends-and-quota',
    title: 'Backends and quota',
    blurb: 'Use the agent CLI you prefer, and keep spend in check.',
    features: [
      { title: 'Backend registry', desc: 'Detected agent CLIs are registered with tiers and a default; run them side by side.', section: '§35', href: 'guides/backend-registry' },
      { title: 'Cost-tier routing', desc: 'Autopilot prefers free, then subscription, then opt-in pay-per-use backends.', section: '§34.4', href: 'concepts/agent-backends' },
      { title: 'Limit recovery', desc: 'Recover an agent onto another backend when a hard limit is hit.', section: '§3', href: 'guides/backend-recovery' },
      { title: 'Savings ledger', desc: 'Measure the tokens warden saves with `wd usage savings`.', section: '§29', href: 'reference/savings' },
      { title: 'Cost governance', desc: 'Track spend and enforce a budget gate with `wd usage spend`.', section: '§30', href: 'reference/savings' },
    ],
  },
  {
    id: 'observe-and-approve',
    title: 'Observe and approve',
    blurb: 'See what every agent is doing and answer prompts in one place.',
    features: [
      { title: 'TUI cockpit', desc: 'Framed panes for the fleet, terminals and live agent detail in your terminal.', section: '§8', href: 'guides/tui-cockpit' },
      { title: 'Web mission control', desc: 'A browser dashboard with a live fleet view and an attention queue.', section: '§9', href: 'guides/web-mission-control' },
      { title: 'Approvals inbox', desc: 'Answer agent prompts centrally, or enable auto-approve with a policy.', section: '§5', href: 'guides/approvals-supervised' },
      { title: 'Metrics and notifications', desc: 'Context fill, CPU and memory per agent, with notifications.', section: '§11', href: 'reference/observability' },
      { title: 'Search and history', desc: 'Full-text search across agent output, plus an archive of past sessions.', section: '§14', href: 'guides/fleet-operations' },
      { title: 'Insights', desc: 'AI-powered analysis of how your fleet is used.', section: '§25', href: 'reference/insights' },
    ],
  },
  {
    id: 'remote-and-mobile',
    title: 'Remote and mobile',
    blurb: 'Reach your fleet away from the desk.',
    features: [
      { title: 'Remote access', desc: 'Serve the daemon beyond loopback behind bearer-token authentication.', section: '§13', href: 'guides/remote-access' },
      { title: 'Mobile-responsive dashboard', desc: 'Use the web dashboard from a phone or tablet.', section: '§13', href: 'guides/remote-access' },
    ],
  },
  {
    id: 'operate',
    title: 'Operate',
    blurb: 'Checkpoint, configure, audit and extend.',
    features: [
      { title: 'Snapshots and rollback', desc: 'Checkpoint an agent worktree and transcript, and restore it.', section: '§23', href: 'guides/snapshots' },
      { title: 'Audit log', desc: 'Inspect a record of daemon actions with `warden inspect audit`.', section: '§18', href: 'guides/fleet-operations' },
      { title: 'Configuration with hot-reload', desc: 'A YAML config that reloads live.', section: '§12', href: 'reference/env-vars' },
      { title: 'Plugins', desc: 'Extend warden with the plugin system.', section: '§26', href: 'reference/plugins' },
      { title: 'MCP and the /warden skill', desc: 'Drive warden from an agent over MCP and the bundled skill.', section: '§10', href: 'multi-agent/mcp-and-skill' },
    ],
  },
];
