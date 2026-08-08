import assert from 'node:assert/strict';
import test from 'node:test';

import {run} from './a11y-precommit.mjs';

const repoRoot = '/repo/go';

test('passes staged web paths and no-update-baseline', () => {
  const calls = [];
  const code = run({
    repoRoot,
    staged: ['go/web/templates/base.html', 'README.md'],
    unstaged: [],
    exec: (command, args, options) => {
      calls.push([command, args, options]);
      return 0;
    },
  });

  assert.equal(code, 0);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0][1], [
    'scan',
    '--repo-root',
    repoRoot,
    '--changed-files',
    'web/templates/base.html',
    '--no-update-baseline',
    '--format',
    'text',
  ]);
  assert.equal(calls[0][2].env.A11Y_ALLOWED_ROOTS, repoRoot);
});

test('passes each applicable staged path as a separate changed-files argument', () => {
  const calls = [];
  const code = run({
    repoRoot,
    staged: ['go/web/templates/base.html', 'go/internal/web/pages.go'],
    unstaged: [],
    exec: (command, args) => {
      calls.push([command, args]);
      return 0;
    },
  });

  assert.equal(code, 0);
  assert.deepEqual(calls[0][1].slice(3, 7), [
    '--changed-files',
    'web/templates/base.html',
    '--changed-files',
    'internal/web/pages.go',
  ]);
});

test('exits cleanly without invoking the scanner when no staged file is applicable', () => {
  let invoked = false;
  const code = run({
    repoRoot,
    staged: ['README.md', 'go/internal/workflow/store.go'],
    unstaged: [],
    exec: () => {
      invoked = true;
      return 0;
    },
  });

  assert.equal(code, 0);
  assert.equal(invoked, false);
});

test('scans the destination path reported for a staged rename', () => {
  const calls = [];
  const code = run({
    repoRoot,
    staged: ['go/web/templates/new-name.html'],
    unstaged: [],
    exec: (command, args) => {
      calls.push([command, args]);
      return 0;
    },
  });

  assert.equal(code, 0);
  assert.equal(calls[0][1][4], 'web/templates/new-name.html');
});

for (const [label, unsafePath] of [
  ['spaces', 'go/web/templates/unsafe name.html'],
  ['commas', 'go/web/templates/unsafe,name.html'],
  ['leading whitespace', ' go/web/templates/base.html'],
  ['trailing whitespace', 'go/web/templates/base.html '],
]) {
  test(`rejects applicable staged paths containing ${label} before spawning`, () => {
    let invoked = false;
    const errors = [];
    const code = run({
      repoRoot,
      staged: [unsafePath],
      unstaged: [],
      exec: () => {
        invoked = true;
        return 0;
      },
      error: (message) => errors.push(message),
    });

    assert.equal(code, 2);
    assert.equal(invoked, false);
    assert.match(errors.join('\n'), /cannot safely scan/i);
  });
}

test('rejects an applicable partially staged file with actionable remediation', () => {
  let invoked = false;
  const errors = [];
  const code = run({
    repoRoot,
    staged: ['go/web/templates/base.html'],
    unstaged: ['go/web/templates/base.html'],
    exec: () => {
      invoked = true;
      return 0;
    },
    error: (message) => errors.push(message),
  });

  assert.equal(code, 2);
  assert.equal(invoked, false);
  assert.match(errors.join('\n'), /stage the full file or move its unstaged edit/i);
});

for (const scannerExit of [1, 2]) {
  test(`propagates scanner exit ${scannerExit}`, () => {
    const code = run({
      repoRoot,
      staged: ['go/web/templates/base.html'],
      unstaged: [],
      exec: () => scannerExit,
    });

    assert.equal(code, scannerExit);
  });
}
