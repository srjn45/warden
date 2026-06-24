import { describe, it, expect } from 'vitest';
import { emptyDraft, emptyJob, validateDraft, draftToSpec, specToYaml } from './pipeline-spec';
import type { PipelineDraft } from './pipeline-spec';

function draft(over: Partial<PipelineDraft> = {}): PipelineDraft {
  return {
    name: 'demo',
    repo: '/repo',
    jobs: [{ ...emptyJob(), id: 'a', prompt: 'do a' }],
    ...over,
  };
}

describe('validateDraft', () => {
  it('accepts a minimal valid draft', () => {
    expect(validateDraft(draft())).toEqual([]);
  });

  it('requires a pipeline name', () => {
    expect(validateDraft(draft({ name: '' }))).toContainEqual(expect.stringMatching(/name/i));
  });

  it('rejects an unsafe pipeline name', () => {
    expect(validateDraft(draft({ name: 'a/b' }))).toContainEqual(expect.stringMatching(/name/i));
    expect(validateDraft(draft({ name: 'a:b' }))).toContainEqual(expect.stringMatching(/name/i));
    expect(validateDraft(draft({ name: '..' }))).toContainEqual(expect.stringMatching(/name/i));
  });

  it('requires a repo', () => {
    expect(validateDraft(draft({ repo: '' }))).toContainEqual(expect.stringMatching(/repo/i));
  });

  it('requires at least one job', () => {
    expect(validateDraft(draft({ jobs: [] }))).toContainEqual(expect.stringMatching(/job/i));
  });

  it('requires a non-empty prompt on each job', () => {
    expect(validateDraft(draft({ jobs: [{ ...emptyJob(), id: 'a', prompt: '   ' }] })))
      .toContainEqual(expect.stringMatching(/prompt/i));
  });

  it('requires a safe job id', () => {
    expect(validateDraft(draft({ jobs: [{ ...emptyJob(), id: 'a/b', prompt: 'x' }] })))
      .toContainEqual(expect.stringMatching(/id/i));
    expect(validateDraft(draft({ jobs: [{ ...emptyJob(), id: '', prompt: 'x' }] })))
      .toContainEqual(expect.stringMatching(/id/i));
  });

  it('rejects duplicate job ids', () => {
    const jobs = [
      { ...emptyJob(), id: 'a', prompt: 'x' },
      { ...emptyJob(), id: 'a', prompt: 'y' },
    ];
    expect(validateDraft(draft({ jobs }))).toContainEqual(expect.stringMatching(/duplicate/i));
  });

  it('rejects a dependency on an unknown job', () => {
    const jobs = [{ ...emptyJob(), id: 'a', prompt: 'x', depends_on: ['ghost'] }];
    expect(validateDraft(draft({ jobs }))).toContainEqual(expect.stringMatching(/unknown/i));
  });

  it('rejects a self-dependency', () => {
    const jobs = [{ ...emptyJob(), id: 'a', prompt: 'x', depends_on: ['a'] }];
    expect(validateDraft(draft({ jobs }))).toContainEqual(expect.stringMatching(/itself|self/i));
  });

  it('rejects a dependency cycle', () => {
    const jobs = [
      { ...emptyJob(), id: 'a', prompt: 'x', depends_on: ['b'] },
      { ...emptyJob(), id: 'b', prompt: 'y', depends_on: ['a'] },
    ];
    expect(validateDraft(draft({ jobs }))).toContainEqual(expect.stringMatching(/cycle/i));
  });

  it('rejects a from:<job> worktree that references an unknown job', () => {
    const jobs = [{ ...emptyJob(), id: 'a', prompt: 'x', worktree: 'from:ghost' }];
    expect(validateDraft(draft({ jobs }))).toContainEqual(expect.stringMatching(/unknown|worktree/i));
  });

  it('accepts a valid from:<job> worktree', () => {
    const jobs = [
      { ...emptyJob(), id: 'a', prompt: 'x' },
      { ...emptyJob(), id: 'b', prompt: 'y', depends_on: ['a'], worktree: 'from:a' },
    ];
    expect(validateDraft(draft({ jobs }))).toEqual([]);
  });

  it('rejects an invalid run_if', () => {
    const jobs = [{ ...emptyJob(), id: 'a', prompt: 'x', run_if: 'maybe' as any }];
    expect(validateDraft(draft({ jobs }))).toContainEqual(expect.stringMatching(/run_if/i));
  });

  it('accepts failure and always run_if', () => {
    const jobs = [
      { ...emptyJob(), id: 'a', prompt: 'x' },
      { ...emptyJob(), id: 'b', prompt: 'y', depends_on: ['a'], run_if: 'failure' as const },
      { ...emptyJob(), id: 'c', prompt: 'z', depends_on: ['a'], run_if: 'always' as const },
    ];
    expect(validateDraft(draft({ jobs }))).toEqual([]);
  });
});

describe('draftToSpec', () => {
  it('produces the YAML schema shape with defaults applied', () => {
    const spec = draftToSpec(draft());
    expect(spec).toEqual({
      name: 'demo',
      repo: '/repo',
      jobs: [{ id: 'a', prompt: 'do a', worktree: 'none' }],
    });
  });

  it('omits empty optional fields and includes set ones', () => {
    const d = draft({
      jobs: [{
        id: 'a', prompt: 'p', depends_on: ['x'], handoff: 'hand',
        worktree: 'fresh', supervised: true, type: 'review', run_if: 'success',
      }],
    });
    // depends_on must be sorted-stable and present; handoff/supervised/type included.
    // run_if 'success' is the default and is omitted.
    const spec = draftToSpec(d) as any;
    expect(spec.jobs[0]).toEqual({
      id: 'a', prompt: 'p', depends_on: ['x'], handoff: 'hand',
      worktree: 'fresh', supervised: true, type: 'review',
    });
  });

  it('includes a non-default run_if', () => {
    const d = draft({ jobs: [{ ...emptyJob(), id: 'a', prompt: 'p', run_if: 'failure' }] });
    const spec = draftToSpec(d) as any;
    expect(spec.jobs[0].run_if).toBe('failure');
  });

  it('drops empty depends_on entries', () => {
    const d = draft({ jobs: [{ ...emptyJob(), id: 'a', prompt: 'p', depends_on: ['', 'x', '  '] }] });
    const spec = draftToSpec(d) as any;
    expect(spec.jobs[0].depends_on).toEqual(['x']);
  });
});

describe('specToYaml', () => {
  it('serializes to JSON, which is valid YAML the daemon accepts', () => {
    const spec = draftToSpec(draft());
    const text = specToYaml(spec);
    expect(JSON.parse(text)).toEqual(spec);
  });
});

describe('emptyDraft / emptyJob', () => {
  it('emptyDraft starts with a single blank job', () => {
    const d = emptyDraft();
    expect(d.jobs.length).toBe(1);
    expect(d.jobs[0].worktree).toBe('none');
  });
});
