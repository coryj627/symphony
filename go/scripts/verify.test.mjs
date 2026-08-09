import assert from 'node:assert/strict';
import test from 'node:test';

import {run} from './verify.mjs';

const githubSentinel = 'SKIPPED: GitHub live profile not enabled';
const linearSentinel = 'SKIPPED: Linear live profile not enabled';

function successfulCapture(command, args, options) {
  const provider = args.at(-1);
  return {
    status: 0,
    stdout: provider.endsWith('/github') ? githubSentinel : linearSentinel,
    stderr: '',
    command,
    args,
    options,
  };
}

test('runs every Phase 2 gate in order on macOS including build, disabled profiles, and race', () => {
  const calls = [];
  const code = run({
    platform: 'darwin',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.5'},
    exec: (command, args) => {
      calls.push(['exec', command, args]);
      return 0;
    },
    capture: (command, args, options) => {
      calls.push(['capture', command, args]);
      return successfulCapture(command, args, options);
    },
  });

  assert.equal(code, 0);
  assert.deepEqual(calls.map(([kind, , args]) => [kind, args]), [
    ['exec', ['--test', 'scripts/a11y-precommit.test.mjs', 'scripts/a11y-scan-all.test.mjs', 'scripts/ci-structure.test.mjs', 'scripts/go-tool.test.mjs', 'scripts/verify.test.mjs']],
    ['exec', ['build', './cmd/symphony']],
    ['exec', ['test', './...']],
    ['exec', ['test', '-race', './...']],
    ['exec', ['vet', './...']],
    ['capture', ['test', '-v', '-tags=integration_live', '-count=1', '-timeout=2m', './internal/tracker/github']],
    ['capture', ['test', '-v', '-tags=integration_live', '-count=1', '-timeout=2m', './internal/tracker/linear']],
    ['exec', ['ci']],
    ['exec', ['run', 'html:validate']],
    ['exec', ['run', 'test:a11y']],
    ['exec', ['scripts/a11y-scan-all.mjs']],
  ]);
});

test('runs deterministic Phase 2 gates on Windows without claiming race support', () => {
  const calls = [];
  const code = run({
    platform: 'win32',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.5'},
    exec: (command, args) => {
      calls.push(['exec', command, args]);
      return 0;
    },
    capture: (command, args, options) => {
      calls.push(['capture', command, args]);
      return successfulCapture(command, args, options);
    },
  });

  assert.equal(code, 0);
  assert.equal(calls.some(([, , args]) => args.includes('-race')), false);
  assert.equal(calls.some(([, , args]) => args[0] === 'build' && args[1] === './cmd/symphony'), true);
  assert.equal(calls.filter(([kind]) => kind === 'capture').length, 2);
});

test('disabled profile gates remove every live variable from their child environment', () => {
  const inherited = {
    ...process.env,
    SYMPHONY_RUN_GITHUB_LIVE: '1',
    SYMPHONY_GITHUB_TEST_REPO: 'owner/repository',
    SYMPHONY_GITHUB_TEST_TOKEN: 'github-token-canary',
    SYMPHONY_RUN_LINEAR_LIVE: '1',
    SYMPHONY_LINEAR_TEST_PROJECT: 'project-slug',
    SYMPHONY_LINEAR_TEST_TOKEN: 'linear-token-canary',
    symphony_run_github_live: '1',
    Symphony_Linear_Test_Token: 'case-variant-token-canary',
  };
  const environments = [];
  const code = run({
    platform: 'darwin',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.5'},
    environment: inherited,
    exec: () => 0,
    capture: (command, args, options) => {
      environments.push(options.env);
      return successfulCapture(command, args, options);
    },
  });

  assert.equal(code, 0);
  assert.equal(environments.length, 2);
  for (const environment of environments) {
    for (const name of [
      'SYMPHONY_RUN_GITHUB_LIVE',
      'SYMPHONY_GITHUB_TEST_REPO',
      'SYMPHONY_GITHUB_TEST_TOKEN',
      'SYMPHONY_RUN_LINEAR_LIVE',
      'SYMPHONY_LINEAR_TEST_PROJECT',
      'SYMPHONY_LINEAR_TEST_TOKEN',
      'symphony_run_github_live',
      'Symphony_Linear_Test_Token',
    ]) {
      assert.equal(Object.hasOwn(environment, name), false, name);
    }
  }
});

test('fails closed when a disabled provider exits zero without its exact SKIP sentinel', () => {
  const errors = [];
  let ordinaryCalls = 0;
  const code = run({
    platform: 'darwin',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.5'},
    exec: () => {
      ordinaryCalls += 1;
      return 0;
    },
    capture: () => ({status: 0, stdout: 'ok without a disabled sentinel', stderr: ''}),
    error: (message) => errors.push(message),
  });

  assert.equal(code, 2);
  assert.equal(ordinaryCalls, 5);
  assert.match(errors.join('\n'), /GitHub.*SKIPPED: GitHub live profile not enabled/i);
});

test('propagates a disabled provider test failure before later gates', () => {
  let captures = 0;
  let ordinaryCalls = 0;
  const code = run({
    platform: 'darwin',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.5'},
    exec: () => {
      ordinaryCalls += 1;
      return 0;
    },
    capture: () => {
      captures += 1;
      return {status: 7, stdout: '', stderr: 'safe failure'};
    },
    error: () => {},
  });

  assert.equal(code, 7);
  assert.equal(captures, 1);
  assert.equal(ordinaryCalls, 5);
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
  assert.doesNotMatch(errors.join('\n'), /Phase 1/);
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
    capture: successfulCapture,
  });

  assert.equal(code, 0);
  assert.deepEqual(calls[1], ['mise', ['exec', '--', 'go', 'build', './cmd/symphony']]);
  assert.deepEqual(calls[2], ['mise', ['exec', '--', 'go', 'test', './...']]);
  assert.deepEqual(calls[3], ['mise', ['exec', '--', 'go', 'vet', './...']]);
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
