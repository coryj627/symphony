import assert from 'node:assert/strict';
import test from 'node:test';

import {run} from './verify.mjs';

test('runs every Phase 1 gate in order on macOS including the race detector', () => {
  const calls = [];
  const code = run({
    platform: 'darwin',
    goTool: {command: 'go', prefix: []},
    exec: (command, args) => {
      calls.push([command, args]);
      return 0;
    },
  });

  assert.equal(code, 0);
  assert.deepEqual(calls.map(([, args]) => args), [
    ['--test', 'scripts/a11y-precommit.test.mjs', 'scripts/a11y-scan-all.test.mjs', 'scripts/go-tool.test.mjs', 'scripts/verify.test.mjs'],
    ['test', './...'],
    ['test', '-race', './...'],
    ['vet', './...'],
    ['ci'],
    ['run', 'html:validate'],
    ['run', 'test:a11y'],
    ['scripts/a11y-scan-all.mjs'],
  ]);
});

test('runs deterministic Phase 1 gates on Windows without claiming race support', () => {
  const calls = [];
  const code = run({
    platform: 'win32',
    goTool: {command: 'go', prefix: []},
    exec: (command, args) => {
      calls.push([command, args]);
      return 0;
    },
  });

  assert.equal(code, 0);
  assert.equal(calls.some(([, args]) => args.includes('-race')), false);
  assert.equal(calls.some(([, args]) => args[0] === 'test' && args[1] === './...'), true);
});

test('fails closed on unsupported operating systems', () => {
  let invoked = false;
  const errors = [];
  const code = run({
    platform: 'linux',
    exec: () => {
      invoked = true;
      return 0;
    },
    error: (message) => errors.push(message),
  });

  assert.equal(code, 2);
  assert.equal(invoked, false);
  assert.match(errors.join('\n'), /supports only macOS and Windows/i);
});

test('stops at and propagates the first failing gate', () => {
  let callCount = 0;
  const code = run({
    platform: 'darwin',
    goTool: {command: 'go', prefix: []},
    exec: () => {
      callCount += 1;
      return callCount === 3 ? 1 : 0;
    },
  });

  assert.equal(code, 1);
  assert.equal(callCount, 3);
});

test('runs Go gates through the pinned mise fallback when ambient Go is absent', () => {
  const calls = [];
  const code = run({
    platform: 'win32',
    goTool: {command: 'mise', prefix: ['exec', '--', 'go']},
    exec: (command, args) => {
      calls.push([command, args]);
      return 0;
    },
  });

  assert.equal(code, 0);
  assert.deepEqual(calls[1], ['mise', ['exec', '--', 'go', 'test', './...']]);
  assert.deepEqual(calls[2], ['mise', ['exec', '--', 'go', 'vet', './...']]);
});
