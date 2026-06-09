import { spawn, ConfirmationRequiredError } from './api';

// QuickAddResult is the discriminated outcome of a one-click pane spawn. The
// button maps each variant to UI state; quickAdd never throws.
export type QuickAddResult =
  | { kind: 'created'; id: string }
  | { kind: 'confirm'; reason: string } // 428 memory pressure — needs force
  | { kind: 'error'; message: string };

// quickAdd spawns a no-prompt, unsupervised agent in `dir`. Pass force=true to
// proceed past a memory-pressure 428 (a prior call returned { kind: 'confirm' }).
export async function quickAdd(dir: string, force = false): Promise<QuickAddResult> {
  try {
    const s = await spawn({ prompt: '', cwd: dir, supervised: false, force });
    return { kind: 'created', id: s.id };
  } catch (e) {
    if (e instanceof ConfirmationRequiredError) {
      return { kind: 'confirm', reason: e.verdict.reason };
    }
    const message = e instanceof Error ? e.message : String(e);
    return { kind: 'error', message };
  }
}
