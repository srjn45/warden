import type { PipelineJobStatus } from './types';

// jobStatusClass returns the CSS class for a job card (styled in app.css).
export function jobStatusClass(s: PipelineJobStatus): string {
  return `job-${s}`;
}

// isJobRetryable reports whether `pipeline retry` applies to this job.
export function isJobRetryable(s: PipelineJobStatus): boolean {
  return s === 'failed' || s === 'needs_attention';
}
