import assert from 'node:assert/strict';
import test from 'node:test';

import {run} from './verify.mjs';

test('runs every Phase 1 gate in order on macOS including the race detector', () => {
  const calls = [];
  const code = run({
    platform: 'darwin',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.5'},
    exec: (command, args) => {
      calls.push([command, args]);
      return 0;
    },
  });

  assert.equal(code, 0);
  assert.deepEqual(calls.map(([, args]) => args), [
    ['--test', 'scripts/a11y-precommit.test.mjs', 'scripts/a11y-scan-all.test.mjs', 'scripts/ci-structure.test.mjs', 'scripts/go-tool.test.mjs', 'scripts/verify.test.mjs'],
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
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.5'},
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
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.5'},
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
    nodeVersion: 'v24.18.0',
    goTool: {command: 'mise', prefix: ['exec', '--', 'go'], version: '1.26.5'},
    exec: (command, args) => {
      calls.push([command, args]);
      return 0;
    },
  });

  assert.equal(code, 0);
  assert.deepEqual(calls[1], ['mise', ['exec', '--', 'go', 'test', './...']]);
  assert.deepEqual(calls[2], ['mise', ['exec', '--', 'go', 'vet', './...']]);
});

for (const nodeVersion of ['v24.17.0', 'v24.18.1', '24.18.0', 'malformed']) {
  test(`rejects Node runtime ${JSON.stringify(nodeVersion)}`, () => {
    let invoked = false;
    const errors = [];
    const code = run({
      platform: 'darwin',
      nodeVersion,
      goTool: {command: 'go', prefix: [], version: '1.26.5'},
      exec: () => {
        invoked = true;
        return 0;
      },
      error: (message) => errors.push(message),
    });

    assert.equal(code, 2);
    assert.equal(invoked, false);
    assert.match(errors.join('\n'), /Node 24\.18\.0 is required/);
  });
}

for (const goVersion of ['1.25.9', '1.26.6', 'malformed']) {
  test(`rejects selected Go runtime ${JSON.stringify(goVersion)}`, () => {
    let invoked = false;
    const code = run({
      platform: 'darwin',
      nodeVersion: 'v24.18.0',
      goTool: {command: 'go', prefix: [], version: goVersion},
      exec: () => {
        invoked = true;
        return 0;
      },
      error: () => {},
    });

    assert.equal(code, 2);
    assert.equal(invoked, false);
  });
}
