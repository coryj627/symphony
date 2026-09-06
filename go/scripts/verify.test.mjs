import assert from 'node:assert/strict';
import {existsSync} from 'node:fs';
import test from 'node:test';
import path from 'node:path';

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

test('runs every gate in order on macOS including vulnerabilities, disabled profiles, and race', () => {
  const calls = [];
  const code = run({
    platform: 'darwin',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.6'},
    exec: (command, args) => {
      calls.push(['exec', command, args]);
      return 0;
    },
    capture: (command, args, options) => {
      calls.push(['capture', command, args]);
      return successfulCapture(command, args, options);
    },
    repoRoot: '/fixture/repository',
  });

  assert.equal(code, 0);
  const buildArgs = calls[1][2];
  assert.deepEqual(buildArgs.slice(0, 2), ['build', '-o']);
  assert.equal(buildArgs.at(-1), './cmd/symphony');
  assert.notEqual(path.dirname(buildArgs[2]), '/fixture/repository');
  assert.equal(existsSync(path.dirname(buildArgs[2])), false, 'temporary build directory was not cleaned');
  assert.deepEqual(calls.map(([kind, , args]) => [kind, args]), [
    ['exec', ['--test', 'scripts/a11y-precommit.test.mjs', 'scripts/a11y-scan-all.test.mjs', 'scripts/check-local-assets.test.mjs', 'scripts/check-secret-patterns.test.mjs', 'scripts/check-upstream-trace.test.mjs', 'scripts/ci-structure.test.mjs', 'scripts/git-attributes.test.mjs', 'scripts/go-tool.test.mjs', 'scripts/verify.test.mjs']],
    ['exec', buildArgs],
    ['exec', ['test', './...']],
    ['exec', ['test', '-race', './...']],
    ['exec', ['vet', './...']],
    ['exec', ['tool', 'govulncheck', '-show=version', '-db=https://vuln.go.dev', './...']],
    ['capture', ['test', '-v', '-tags=integration_live', '-count=1', '-timeout=2m', './internal/tracker/github']],
    ['capture', ['test', '-v', '-tags=integration_live', '-count=1', '-timeout=2m', './internal/tracker/linear']],
    ['exec', ['ci']],
    ['exec', ['run', 'security:assets']],
    ['exec', ['run', 'security:secrets']],
    ['exec', ['run', 'conformance:upstream']],
    ['exec', ['run', 'html:validate']],
    ['exec', ['run', 'test:a11y']],
    ['exec', ['scripts/a11y-scan-all.mjs']],
  ]);
});

test('runs verification gates on Windows without claiming race support', () => {
  const calls = [];
  const code = run({
    platform: 'win32',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.6'},
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
  assert.equal(calls.some(([, , args]) => args[0] === 'build' && args[1] === '-o' && args.at(-1) === './cmd/symphony'), true);
  assert.equal(calls.filter(([kind]) => kind === 'capture').length, 2);
  assert.ok(calls.some(([, command, args]) => command === 'go' && args.join(' ') === 'tool govulncheck -show=version -db=https://vuln.go.dev ./...'));
});

for (const platform of ['darwin', 'win32']) {
  for (const status of [1, 2, 3]) {
    test(`${platform} stops verification when govulncheck exits ${status}`, () => {
      const calls = [];
      let captured = false;
      const code = run({
        platform,
        nodeVersion: 'v24.18.0',
        goTool: {command: 'go', prefix: [], version: '1.26.6'},
        exec: (command, args) => {
          calls.push(args);
          return args.includes('govulncheck') ? status : 0;
        },
        capture: (command, args, options) => {
          captured = true;
          return successfulCapture(command, args, options);
        },
      });

      assert.equal(code, status, 'scanner findings and infrastructure failures must remain failures');
      assert.equal(captured, false, 'later gates must not run after a failed scan');
      assert.deepEqual(calls.at(-1), ['tool', 'govulncheck', '-show=version', '-db=https://vuln.go.dev', './...']);
    });
  }
}

test('disabled profile gates remove every live variable from their child environment', () => {
  const inherited = {
    ...process.env,
    SYMPHONY_RUN_GITHUB_LIVE: '1',
    SYMPHONY_GITHUB_TEST_REPO: 'owner/repository',
    SYMPHONY_GITHUB_TEST_TOKEN: 'github-token-canary',
    SYMPHONY_RUN_LINEAR_LIVE: '1',
    SYMPHONY_LINEAR_TEST_PROJECT: 'project-slug',
    SYMPHONY_LINEAR_TEST_TOKEN: 'linear-token-canary',
    SYMPHONY_REAL_CODEX_SMOKE: '1',
    SYMPHONY_REAL_CODEX_WORKFLOW: '/unsafe/ambient/workflow',
    SYMPHONY_REAL_CODEX_COMMAND: 'unsafe-ambient-command',
    SYMPHONY_REAL_CODEX_LOGIN_COMMAND: 'unsafe-ambient-login',
    symphony_run_github_live: '1',
    Symphony_Linear_Test_Token: 'case-variant-token-canary',
    symphony_real_codex_smoke: '1',
  };
  const environments = [];
  const code = run({
    platform: 'darwin',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.6'},
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
      'SYMPHONY_REAL_CODEX_SMOKE',
      'SYMPHONY_REAL_CODEX_WORKFLOW',
      'SYMPHONY_REAL_CODEX_COMMAND',
      'SYMPHONY_REAL_CODEX_LOGIN_COMMAND',
      'symphony_run_github_live',
      'Symphony_Linear_Test_Token',
      'symphony_real_codex_smoke',
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
    goTool: {command: 'go', prefix: [], version: '1.26.6'},
    exec: () => {
      ordinaryCalls += 1;
      return 0;
    },
    capture: () => ({status: 0, stdout: 'ok without a disabled sentinel', stderr: ''}),
    error: (message) => errors.push(message),
  });

  assert.equal(code, 2);
  assert.equal(ordinaryCalls, 6);
  assert.match(errors.join('\n'), /GitHub.*SKIPPED: GitHub live profile not enabled/i);
});

test('propagates a disabled provider test failure before later gates', () => {
  let captures = 0;
  let ordinaryCalls = 0;
  const code = run({
    platform: 'darwin',
    nodeVersion: 'v24.18.0',
    goTool: {command: 'go', prefix: [], version: '1.26.6'},
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
  assert.equal(ordinaryCalls, 6);
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
    goTool: {command: 'go', prefix: [], version: '1.26.6'},
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
    goTool: {command: 'mise', prefix: ['exec', '--', 'go'], version: '1.26.6'},
    exec: (command, args) => {
      calls.push([command, args]);
      return 0;
    },
    capture: successfulCapture,
  });

  assert.equal(code, 0);
  assert.equal(calls[1][0], 'mise');
  assert.deepEqual(calls[1][1].slice(0, 5), ['exec', '--', 'go', 'build', '-o']);
  assert.equal(calls[1][1].at(-1), './cmd/symphony');
  assert.deepEqual(calls[2], ['mise', ['exec', '--', 'go', 'test', './...']]);
  assert.deepEqual(calls[3], ['mise', ['exec', '--', 'go', 'vet', './...']]);
  assert.deepEqual(calls[4], ['mise', ['exec', '--', 'go', 'tool', 'govulncheck', '-show=version', '-db=https://vuln.go.dev', './...']]);
});

for (const nodeVersion of ['v24.17.0', 'v24.18.1', '24.18.0', 'malformed']) {
  test(`rejects Node runtime ${JSON.stringify(nodeVersion)}`, () => {
    let invoked = false;
    const errors = [];
    const code = run({
      platform: 'darwin',
      nodeVersion,
      goTool: {command: 'go', prefix: [], version: '1.26.6'},
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

for (const goVersion of ['1.25.9', '1.26.5', '1.26.7', 'malformed']) {
  test(`rejects selected Go runtime ${JSON.stringify(goVersion)}`, () => {
    let invoked = false;
    const code = run({
      platform: 'darwin',
      nodeVersion: 'v24.18.0',
      goTool: {command: 'go', prefix: [], version: goVersion},
      capture: successfulCapture,
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
