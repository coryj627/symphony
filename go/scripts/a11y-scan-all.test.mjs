import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import test from 'node:test';

import {run} from './a11y-scan-all.mjs';

function withRepo(fn) {
  const repoRoot = mkdtempSync(path.join(tmpdir(), 'symphony-a11y-scan-'));
  const baselinePath = path.join(repoRoot, '.a11y', 'web', 'baseline.json');
  mkdirSync(path.dirname(baselinePath), {recursive: true});
  writeFileSync(baselinePath, '{"findings":[]}\n');
  try {
    fn({repoRoot, baselinePath});
  } finally {
    rmSync(repoRoot, {recursive: true, force: true});
  }
}

function digest(filePath) {
  return createHash('sha256').update(readFileSync(filePath)).digest('hex');
}

test('uses the canonical Go root, exact authorization root, and review-only scan flags', () => {
  withRepo(({repoRoot}) => {
    const calls = [];
    const code = run({
      repoRoot: path.join(repoRoot, '.'),
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
      '--no-update-baseline',
      '--format',
      'text',
    ]);
    assert.equal(calls[0][2].cwd, repoRoot);
    assert.equal(calls[0][2].env.A11Y_ALLOWED_ROOTS, repoRoot);
  });
});

for (const scannerExit of [1, 2]) {
  test(`propagates scanner exit ${scannerExit}`, () => {
    withRepo(({repoRoot}) => {
      const code = run({repoRoot, exec: () => scannerExit});
      assert.equal(code, scannerExit);
    });
  });
}

test('restores the reviewed baseline and fails closed if the scanner mutates it', () => {
  withRepo(({repoRoot, baselinePath}) => {
    const before = digest(baselinePath);
    const errors = [];
    const code = run({
      repoRoot,
      exec: () => {
        writeFileSync(baselinePath, '{"findings":[{"unexpected":true}]}\n');
        return 0;
      },
      error: (message) => errors.push(message),
    });

    assert.equal(code, 2);
    assert.equal(digest(baselinePath), before);
    assert.match(errors.join('\n'), /baseline changed/i);
  });
});
