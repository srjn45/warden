// Pure client-side authoring model for pipelines. Mirrors the daemon's
// pipeline.Validate rules (internal/pipeline/pipeline.go) so the web form can
// give instant feedback, but the daemon stays the authority: the structured
// draft is serialized to JSON (a valid YAML subset) and POSTed to the existing
// POST /pipelines, which re-parses and re-validates it.

export type WorktreeMode = 'none' | 'fresh' | string; // string also covers `from:<jobId>`

export type RunIf = 'success' | 'failure' | 'always';

export interface JobDraft {
  id: string;
  prompt: string;
  depends_on: string[];
  handoff: string;
  worktree: WorktreeMode;
  supervised: boolean;
  type: string;
  run_if: RunIf;
}

export interface PipelineDraft {
  name: string;
  repo: string;
  jobs: JobDraft[];
}

export function emptyJob(): JobDraft {
  return { id: '', prompt: '', depends_on: [], handoff: '', worktree: 'none', supervised: false, type: '', run_if: 'success' };
}

export function emptyDraft(): PipelineDraft {
  return { name: '', repo: '', jobs: [emptyJob()] };
}

// isSafeId mirrors store.SafeID: non-empty, no '/', '\', ':' and no '..'.
export function isSafeId(id: string): boolean {
  return id !== '' && !/[/\\:]/.test(id) && !id.includes('..');
}

// parseWorktree mirrors pipeline.ParseWorktree.
export function parseWorktree(s: string): { mode: string; fromJob: string } {
  if (s.startsWith('from:')) return { mode: 'from', fromJob: s.slice('from:'.length) };
  return { mode: s, fromJob: '' };
}

// cleanDeps drops blank/whitespace-only dependency entries, preserving order.
function cleanDeps(deps: string[]): string[] {
  return deps.map((d) => d.trim()).filter((d) => d !== '');
}

// validateDraft returns a list of human-readable error messages (empty == valid).
// It mirrors the daemon's Validate so the two never disagree on what's accepted.
export function validateDraft(d: PipelineDraft): string[] {
  const errs: string[] = [];

  if (!isSafeId(d.name)) {
    errs.push("pipeline name is required and must have no '/', '\\', ':', or '..'");
  }
  if (d.repo.trim() === '') errs.push('pipeline repo is required');
  if (d.jobs.length === 0) errs.push('pipeline has no jobs');

  const ids = new Set<string>();
  for (const j of d.jobs) {
    if (!isSafeId(j.id)) {
      errs.push(`invalid job id ${JSON.stringify(j.id)}: required, no '/', '\\', ':', or '..'`);
    } else if (ids.has(j.id)) {
      errs.push(`duplicate job id ${JSON.stringify(j.id)}`);
    } else {
      ids.add(j.id);
    }
    if (j.prompt.trim() === '') errs.push(`job ${JSON.stringify(j.id)}: prompt is required`);
    const { mode } = parseWorktree(j.worktree);
    if (mode !== 'none' && mode !== 'fresh' && mode !== 'from') {
      errs.push(`job ${JSON.stringify(j.id)}: invalid worktree ${JSON.stringify(j.worktree)} (want none|fresh|from:<job>)`);
    }
    if (j.run_if !== 'success' && j.run_if !== 'failure' && j.run_if !== 'always') {
      errs.push(`job ${JSON.stringify(j.id)}: invalid run_if ${JSON.stringify(j.run_if)} (want success|failure|always)`);
    }
  }

  // Dependency + worktree-ref integrity (only meaningful once ids are known).
  for (const j of d.jobs) {
    for (const dep of cleanDeps(j.depends_on)) {
      if (dep === j.id) {
        errs.push(`job ${JSON.stringify(j.id)} depends on itself`);
      } else if (!ids.has(dep)) {
        errs.push(`job ${JSON.stringify(j.id)} depends on unknown job ${JSON.stringify(dep)}`);
      }
    }
    const { mode, fromJob } = parseWorktree(j.worktree);
    if (mode === 'from' && !ids.has(fromJob)) {
      errs.push(`job ${JSON.stringify(j.id)} worktree references unknown job ${JSON.stringify(fromJob)}`);
    }
  }

  const cyc = findCycle(d.jobs);
  if (cyc) errs.push(`dependency cycle through job ${JSON.stringify(cyc)}`);

  return errs;
}

// findCycle returns the id of a job involved in a depends_on cycle, or null.
function findCycle(jobs: JobDraft[]): string | null {
  const byId = new Map(jobs.map((j) => [j.id, j]));
  const WHITE = 0, GRAY = 1, BLACK = 2;
  const color: Record<string, number> = {};
  let found: string | null = null;

  function visit(id: string): void {
    if (found) return;
    color[id] = GRAY;
    const j = byId.get(id);
    for (const dep of cleanDeps(j?.depends_on ?? [])) {
      if (!byId.has(dep)) continue;
      if (color[dep] === GRAY) { found = dep; return; }
      if ((color[dep] ?? WHITE) === WHITE) visit(dep);
      if (found) return;
    }
    color[id] = BLACK;
  }

  for (const j of jobs) {
    if ((color[j.id] ?? WHITE) === WHITE) visit(j.id);
    if (found) break;
  }
  return found;
}

// draftToSpec converts the draft into the plain object the daemon's YAML schema
// expects, applying the same defaults (worktree=none) and omitting empty
// optional fields so the produced spec is minimal and round-trips cleanly.
export function draftToSpec(d: PipelineDraft): Record<string, unknown> {
  return {
    name: d.name,
    repo: d.repo,
    jobs: d.jobs.map((j) => {
      const job: Record<string, unknown> = {
        id: j.id,
        prompt: j.prompt,
        worktree: j.worktree || 'none',
      };
      const deps = cleanDeps(j.depends_on);
      if (deps.length > 0) job.depends_on = deps;
      if (j.handoff.trim() !== '') job.handoff = j.handoff;
      if (j.supervised) job.supervised = true;
      if (j.type.trim() !== '') job.type = j.type;
      if (j.run_if && j.run_if !== 'success') job.run_if = j.run_if;
      return job;
    }),
  };
}

// specToYaml renders the spec as the request body for POST /pipelines. JSON is a
// valid YAML subset, so the daemon's yaml.Unmarshal accepts it verbatim — this
// avoids hand-rolling YAML escaping for arbitrary prompt text.
export function specToYaml(spec: Record<string, unknown>): string {
  return JSON.stringify(spec, null, 2);
}
