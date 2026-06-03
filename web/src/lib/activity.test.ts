import { describe, it, expect } from 'vitest';
import { mergeEvents } from './activity';
import type { Session } from './types';

const sess = (id: string, events: Session['events']): Session =>
  ({ id, events } as Session);

describe('mergeEvents', () => {
  it('merges events across agents, newest first, tagged with the agent id', () => {
    const sessions = [
      sess('A-1', [
        { ts: '2026-06-03T10:00:00Z', type: 'spawned', detail: '' },
        { ts: '2026-06-03T10:05:00Z', type: 'tool', detail: 'edit' },
      ]),
      sess('B-2', [
        { ts: '2026-06-03T10:03:00Z', type: 'working', detail: '' },
      ]),
    ];
    const feed = mergeEvents(sessions);
    expect(feed.map((e) => [e.id, e.type])).toEqual([
      ['A-1', 'tool'],
      ['B-2', 'working'],
      ['A-1', 'spawned'],
    ]);
  });

  it('tolerates null event arrays and applies the limit', () => {
    const sessions = [
      sess('A-1', null),
      sess('B-2', [
        { ts: '2026-06-03T10:00:00Z', type: 'a', detail: '' },
        { ts: '2026-06-03T10:01:00Z', type: 'b', detail: '' },
        { ts: '2026-06-03T10:02:00Z', type: 'c', detail: '' },
      ]),
    ];
    expect(mergeEvents(sessions, 2).map((e) => e.type)).toEqual(['c', 'b']);
  });
});
