import { describe, it, expect } from 'vitest';
import { resolveShortcut, isTypingTarget, BINDINGS, type KeyLike } from './shortcuts';

// A non-editable target (e.g. <body>) for the common case.
const body = (): EventTarget => document.body;

describe('resolveShortcut', () => {
  it('maps the single-key bindings', () => {
    expect(resolveShortcut({ key: '?' })).toEqual({ kind: 'help' });
    expect(resolveShortcut({ key: 'n' })).toEqual({ kind: 'new' });
    expect(resolveShortcut({ key: '/' })).toEqual({ kind: 'filter' });
    expect(resolveShortcut({ key: 'r' })).toEqual({ kind: 'refresh' });
    expect(resolveShortcut({ key: 'Escape' })).toEqual({ kind: 'close' });
  });

  it('maps j/k to relative tab navigation', () => {
    expect(resolveShortcut({ key: 'j' })).toEqual({ kind: 'nav', delta: 1 });
    expect(resolveShortcut({ key: 'k' })).toEqual({ kind: 'nav', delta: -1 });
  });

  it('maps 1-9 to positional tab switching, ignoring 0 and beyond', () => {
    expect(resolveShortcut({ key: '1' })).toEqual({ kind: 'tab', index: 1 });
    expect(resolveShortcut({ key: '9' })).toEqual({ kind: 'tab', index: 9 });
    expect(resolveShortcut({ key: '0' })).toBeNull();
  });

  it('ignores unbound keys', () => {
    expect(resolveShortcut({ key: 'x' })).toBeNull();
    expect(resolveShortcut({ key: 'Enter' })).toBeNull();
  });

  it('ignores modifier combos so browser shortcuts pass through', () => {
    expect(resolveShortcut({ key: 'r', metaKey: true })).toBeNull();
    expect(resolveShortcut({ key: 'r', ctrlKey: true })).toBeNull();
    expect(resolveShortcut({ key: 'n', altKey: true })).toBeNull();
  });

  it('allows shift (needed to type ?)', () => {
    // shiftKey isn't inspected; the resulting `key` of '?' is enough.
    expect(resolveShortcut({ key: '?' })).toEqual({ kind: 'help' });
  });

  it('stays dormant while typing, except for Escape', () => {
    const input = document.createElement('input');
    expect(resolveShortcut({ key: 'n', target: input })).toBeNull();
    expect(resolveShortcut({ key: '/', target: input })).toBeNull();
    // Escape still fires so the user can bail out of the field.
    expect(resolveShortcut({ key: 'Escape', target: input })).toEqual({ kind: 'close' });
  });

  it('fires for non-editable targets', () => {
    expect(resolveShortcut({ key: 'n', target: body() })).toEqual({ kind: 'new' });
  });
});

describe('isTypingTarget', () => {
  it('is true for editable fields', () => {
    expect(isTypingTarget(document.createElement('input'))).toBe(true);
    expect(isTypingTarget(document.createElement('textarea'))).toBe(true);
    expect(isTypingTarget(document.createElement('select'))).toBe(true);
    const ce = document.createElement('div');
    ce.contentEditable = 'true';
    // jsdom doesn't compute isContentEditable from the attribute; assert the
    // explicit-property path instead.
    Object.defineProperty(ce, 'isContentEditable', { value: true });
    expect(isTypingTarget(ce)).toBe(true);
  });

  it('is false for buttons, the body, and null', () => {
    expect(isTypingTarget(document.createElement('button'))).toBe(false);
    expect(isTypingTarget(document.body)).toBe(false);
    expect(isTypingTarget(null)).toBe(false);
  });
});

describe('BINDINGS', () => {
  it('documents every shortcut shown in the help overlay', () => {
    const keys = BINDINGS.map((b) => b.keys);
    expect(keys).toContain('?');
    expect(keys).toContain('1–9');
    expect(keys).toContain('j / k');
    expect(keys).toContain('Esc');
  });
});
