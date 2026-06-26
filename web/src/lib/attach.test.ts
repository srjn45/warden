import { describe, it, expect } from 'vitest';
import { attachURL, resizeMessage } from './attach';

describe('attachURL', () => {
  it('uses ws:// for an http page', () => {
    expect(attachURL({ protocol: 'http:', host: 'localhost:8765' }, 'A-1'))
      .toBe('ws://localhost:8765/api/v1/sessions/A-1/attach');
  });
  it('uses wss:// for an https page', () => {
    expect(attachURL({ protocol: 'https:', host: 'host:443' }, 'A-1'))
      .toBe('wss://host:443/api/v1/sessions/A-1/attach');
  });
  it('url-encodes the id', () => {
    expect(attachURL({ protocol: 'http:', host: 'h' }, 'a/b'))
      .toBe('ws://h/api/v1/sessions/a%2Fb/attach');
  });
});

describe('resizeMessage', () => {
  it('serializes cols/rows as JSON', () => {
    expect(resizeMessage(120, 40)).toBe('{"cols":120,"rows":40}');
  });
});
