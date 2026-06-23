import { describe, it, expect } from 'vitest';
import { jobStatusClass, isJobRetryable, pipelineHasLiveJobs, pipelineIsCancelable, jobDigestSummary } from './pipelines';
import type { Pipeline, PipelineStatus, PipelineJobStatus } from './types';

function pipe(statuses: PipelineJobStatus[], status: PipelineStatus = 'running'): Pipeline {
  return {
    id: 'demo',
    name: 'demo',
    repo: '',
    status,
    jobs: statuses.map((s, i) => ({ id: `j${i}`, status: s })),
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

  it('pipelineIsCancelable is true while pending, running, or paused', () => {
    expect(pipelineIsCancelable(pipe(['pending'], 'pending'))).toBe(true);
    expect(pipelineIsCancelable(pipe(['running'], 'running'))).toBe(true);
    expect(pipelineIsCancelable(pipe(['running'], 'paused'))).toBe(true);
  });

  it('pipelineIsCancelable is false once finished (done/canceled)', () => {
    expect(pipelineIsCancelable(pipe(['done'], 'done'))).toBe(false);
    expect(pipelineIsCancelable(pipe(['skipped'], 'canceled'))).toBe(false);
  });

  it('pipelineIsCancelable for stalled depends on whether jobs are still live', () => {
    expect(pipelineIsCancelable(pipe(['failed', 'running'], 'stalled'))).toBe(true);
    expect(pipelineIsCancelable(pipe(['failed', 'skipped'], 'stalled'))).toBe(false);
  });

  it('jobDigestSummary returns the digest summary when present', () => {
    expect(jobDigestSummary({ id: 'a', status: 'done', digest: { summary: 'did it' } } as any)).toBe('did it');
  });

  it('jobDigestSummary returns empty string when absent', () => {
    expect(jobDigestSummary({ id: 'a', status: 'running' } as any)).toBe('');
  });
});
