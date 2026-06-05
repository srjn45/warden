import { describe, it, expect } from 'vitest';
import { jobStatusClass, isJobRetryable, pipelineHasLiveJobs, jobDigestSummary } from './pipelines';
import type { Pipeline, PipelineJobStatus } from './types';

function pipe(statuses: PipelineJobStatus[]): Pipeline {
  return {
    id: 'demo',
    name: 'demo',
    repo: '',
    status: 'running',
    jobs: statuses.map((status, i) => ({ id: `j${i}`, status })),
  } as Pipeline;
}

describe('pipelines helpers', () => {
  it('jobStatusClass maps each status to a distinct class', () => {
    const statuses = ['pending', 'running', 'done', 'failed', 'skipped', 'needs_attention'] as const;
    const classes = statuses.map(jobStatusClass);
    expect(new Set(classes).size).toBe(statuses.length); // all distinct
    expect(classes.every((c) => c.startsWith('job-'))).toBe(true);
  });

  it('isJobRetryable is true only for failed/needs_attention', () => {
    expect(isJobRetryable('failed')).toBe(true);
    expect(isJobRetryable('needs_attention')).toBe(true);
    expect(isJobRetryable('running')).toBe(false);
    expect(isJobRetryable('pending')).toBe(false);
    expect(isJobRetryable('done')).toBe(false);
  });

  it('pipelineHasLiveJobs is true when any job is running or needs_attention', () => {
    expect(pipelineHasLiveJobs(pipe(['done', 'running']))).toBe(true);
    expect(pipelineHasLiveJobs(pipe(['needs_attention']))).toBe(true);
  });

  it('pipelineHasLiveJobs is false when all jobs are stopped', () => {
    expect(pipelineHasLiveJobs(pipe(['done', 'skipped', 'failed']))).toBe(false);
    expect(pipelineHasLiveJobs(pipe([]))).toBe(false);
  });

  it('jobDigestSummary returns the digest summary when present', () => {
    expect(jobDigestSummary({ id: 'a', status: 'done', digest: { summary: 'did it' } } as any)).toBe('did it');
  });

  it('jobDigestSummary returns empty string when absent', () => {
    expect(jobDigestSummary({ id: 'a', status: 'running' } as any)).toBe('');
  });
});
