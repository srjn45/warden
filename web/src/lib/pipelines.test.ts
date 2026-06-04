import { describe, it, expect } from 'vitest';
import { jobStatusClass, isJobRetryable } from './pipelines';

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
});
