import { describe, it, expect } from 'vitest';
import { approvalActionFor } from './approvals';
import type { ApprovalView } from './types';

const recognized: ApprovalView = { id: 'a1', action: 'Bash(ls)', question: 'Do you want to proceed?', options: ['Yes', 'No'], fingerprint: 'ff', recognized: true };
const unrecognized: ApprovalView = { id: 'a1', action: '', question: '', options: [], fingerprint: '', recognized: false };

describe('approvalActionFor', () => {
  it('recognized waiting prompt is answerable', () => {
    const a = approvalActionFor('waiting_for_input', recognized);
    expect(a).toEqual({ kind: 'answer', options: ['Yes', 'No'], fingerprint: 'ff' });
  });
  it('unrecognized waiting prompt falls back to attach', () => {
    expect(approvalActionFor('waiting_for_input', unrecognized)).toEqual({ kind: 'attach', label: 'attach to answer' });
  });
  it('waiting with no view falls back to attach', () => {
    expect(approvalActionFor('waiting_for_input', undefined)).toEqual({ kind: 'attach', label: 'attach to answer' });
  });
  it('errored agent routes to open', () => {
    expect(approvalActionFor('errored', undefined)).toEqual({ kind: 'attach', label: 'open' });
  });
});
