import type { ApprovalView } from './types';

// AttnAction is the render decision for one attention-queue item: either show
// answerable option buttons (recognized waiting prompt) or fall back to a
// click-to-attach button.
export type AttnAction =
  | { kind: 'answer'; options: string[]; fingerprint: string }
  | { kind: 'attach'; label: string };

// approvalActionFor decides how to render a queue item. A recognized prompt for
// a waiting agent is answerable inline; everything else (unrecognized prompt,
// errored/orphaned agent) routes to attach.
export function approvalActionFor(status: string, view?: ApprovalView): AttnAction {
  if (status === 'waiting_for_input' && view && view.recognized) {
    return { kind: 'answer', options: view.options, fingerprint: view.fingerprint };
  }
  const label = status === 'waiting_for_input' ? 'attach to answer' : 'open';
  return { kind: 'attach', label };
}
