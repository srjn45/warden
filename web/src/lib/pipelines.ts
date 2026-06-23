import type { Pipeline, PipelineJob, PipelineJobStatus } from './types';

// jobStatusClass returns the CSS class for a job card (styled in app.css).
export function jobStatusClass(s: PipelineJobStatus): string {
  return `job-${s}`;
}

// isJobRetryable reports whether `pipeline retry` applies to this job.
export function isJobRetryable(s: PipelineJobStatus): boolean {
  return s === 'failed' || s === 'needs_attention';
}

// jobDigestSummary returns the digest summary for a job, or empty string if absent.
export function jobDigestSummary(j: PipelineJob): string {
  return j.digest?.summary ?? '';
}

// pipelineHasLiveJobs reports whether any job is still running or awaiting input.
// A pipeline can only be deleted once none of its jobs is live (mirrors the
// daemon's DELETE /pipelines/{pid} guard).
export function pipelineHasLiveJobs(p: Pipeline): boolean {
  return p.jobs.some((j) => j.status === 'running' || j.status === 'needs_attention');
}

// pipelineIsCancelable reports whether a pipeline can still be canceled. A
// finished pipeline (done/canceled) can only be deleted, not canceled. A
// stalled pipeline (a job failed) stays cancelable only while it has live jobs
// to terminate on parallel branches. Mirrors the daemon's POST cancel guard.
export function pipelineIsCancelable(p: Pipeline): boolean {
  if (p.status === 'done' || p.status === 'canceled') return false;
  if (p.status === 'stalled') return pipelineHasLiveJobs(p);
  return true; // pending | running | paused
}
