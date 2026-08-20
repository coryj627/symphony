import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import * as realFS from 'node:fs';
import {
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import test from 'node:test';

import {run, scannerExec} from './a11y-scan-all.mjs';

function withRepo(fn, {baseline = true} = {}) {
  const repoRoot = mkdtempSync(path.join(tmpdir(), 'symphony-a11y-scan-'));
  const baselinePath = path.join(repoRoot, '.a11y', 'web', 'baseline.json');
  mkdirSync(path.dirname(baselinePath), {recursive: true});
  if (baseline) writeFileSync(baselinePath, '{"findings":[]}\n');
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
      platform: 'darwin',
      exec: (command, args, options) => {
        calls.push([command, args, options]);
        return 0;
      },
    });

    assert.equal(code, 0);
    assert.equal(calls.length, 1);
    assert.equal(calls[0][0], 'a11y-check-web');
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

test('selects the Windows npm command shim without changing scan arguments', () => {
  withRepo(({repoRoot}) => {
    const calls = [];
    const code = run({
      repoRoot,
      platform: 'win32',
      exec: (command, args, options) => {
        calls.push([command, args, options]);
        return 0;
      },
    });

    assert.equal(code, 0);
    assert.equal(calls.length, 1);
    assert.equal(calls[0][0], 'a11y-check-web.cmd');
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

test('launches the Windows npm command shim through cmd.exe without shell mode', () => {
  const calls = [];
  const comSpec = 'C:\\Windows\\System32\\cmd.exe';
  const code = scannerExec('a11y-check-web.cmd', ['scan', '--format', 'text'], {
    cwd: 'C:\\repo\\go',
    env: {ComSpec: comSpec},
    platform: 'win32',
    spawn: (command, args, options) => {
      calls.push([command, args, options]);
      return {status: 0};
    },
    error: () => {},
  });

  assert.equal(code, 0);
  assert.deepEqual(calls, [[
    comSpec,
    ['/d', '/s', '/c', 'a11y-check-web.cmd', 'scan', '--format', 'text'],
    {
      cwd: 'C:\\repo\\go',
      env: {ComSpec: comSpec},
      stdio: 'inherit',
    },
  ]]);
  assert.equal('shell' in calls[0][2], false);
});

test('executes a Windows npm command shim end to end', {skip: process.platform !== 'win32'}, () => {
  withRepo(({repoRoot}) => {
    const shimPath = path.join(repoRoot, 'a11y-check-web.cmd');
    writeFileSync(shimPath, [
      '@echo off',
      'if not "%~1"=="scan" exit /b 91',
      'if not "%~2"=="--format" exit /b 92',
      'if not "%~3"=="text" exit /b 93',
      'exit /b 7',
      '',
    ].join('\r\n'));
    const pathKey = Object.keys(process.env).find((key) => key.toLowerCase() === 'path') || 'Path';
    const errors = [];
    const code = scannerExec('a11y-check-web.cmd', ['scan', '--format', 'text'], {
      cwd: repoRoot,
      env: {
        ...process.env,
        [pathKey]: `${repoRoot}${path.delimiter}${process.env[pathKey] || ''}`,
      },
      platform: 'win32',
      error: (message) => errors.push(message),
    });

    assert.equal(code, 7, errors.join('\n'));
    assert.deepEqual(errors, []);
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

test('fails closed before scanning when the reviewed baseline cannot be read', () => {
  withRepo(({repoRoot, baselinePath}) => {
    let invoked = false;
    const errors = [];
    const fs = {
      ...realFS,
      readFileSync(filePath, ...args) {
        if (filePath === baselinePath) throw new Error('snapshot unreadable');
        return realFS.readFileSync(filePath, ...args);
      },
    };

    const code = run({
      repoRoot,
      fs,
      exec: () => {
        invoked = true;
        return 0;
      },
      error: (message) => errors.push(message),
    });

    assert.equal(code, 2);
    assert.equal(invoked, false);
    assert.equal(readFileSync(baselinePath, 'utf8'), '{"findings":[]}\n');
    assert.match(errors.join('\n'), /snapshot.*unreadable/i);
  });
});

test('accepts a missing baseline when the review-only scanner leaves it missing', () => {
  withRepo(({repoRoot, baselinePath}) => {
    const code = run({repoRoot, exec: () => 0});
    assert.equal(code, 0);
    assert.throws(() => lstatSync(baselinePath), {code: 'ENOENT'});
  }, {baseline: false});
});

test('rejects a pre-existing symlink baseline before scanning without touching its target', () => {
  withRepo(({repoRoot, baselinePath}) => {
    const targetPath = path.join(repoRoot, 'outside-baseline.json');
    rmSync(baselinePath);
    writeFileSync(targetPath, 'target sentinel\n');
    symlinkSync(targetPath, baselinePath);
    let invoked = false;

    const code = run({
      repoRoot,
      exec: () => {
        invoked = true;
        return 0;
      },
      error: () => {},
    });

    assert.equal(code, 2);
    assert.equal(invoked, false);
    assert.equal(readFileSync(targetPath, 'utf8'), 'target sentinel\n');
    assert.equal(lstatSync(baselinePath).isSymbolicLink(), true);
  });
});

test('safely replaces a scanner-created symlink without writing through to its target', () => {
  withRepo(({repoRoot, baselinePath}) => {
    const targetPath = path.join(repoRoot, 'symlink-target.json');
    writeFileSync(targetPath, 'target sentinel\n');

    const code = run({
      repoRoot,
      exec: () => {
        rmSync(baselinePath);
        symlinkSync(targetPath, baselinePath);
        return 0;
      },
      error: () => {},
    });

    assert.equal(code, 2);
    assert.equal(readFileSync(targetPath, 'utf8'), 'target sentinel\n');
    assert.equal(lstatSync(baselinePath).isFile(), true);
    assert.equal(lstatSync(baselinePath).isSymbolicLink(), false);
    assert.equal(readFileSync(baselinePath, 'utf8'), '{"findings":[]}\n');
  });
});

test('fails closed without overwriting a non-regular baseline replacement', () => {
  withRepo(({repoRoot, baselinePath}) => {
    const errors = [];
    const code = run({
      repoRoot,
      exec: () => {
        rmSync(baselinePath);
        mkdirSync(baselinePath);
        return 0;
      },
      error: (message) => errors.push(message),
    });

    assert.equal(code, 2);
    assert.equal(lstatSync(baselinePath).isDirectory(), true);
    assert.match(errors.join('\n'), /restoration failed/i);
  });
});

test('treats a post-scan comparison read failure as exit 2 and restores the snapshot', () => {
  withRepo(({repoRoot, baselinePath}) => {
    let baselineReads = 0;
    const errors = [];
    const fs = {
      ...realFS,
      readFileSync(filePath, ...args) {
        if (filePath === baselinePath && ++baselineReads === 2) {
          throw new Error('comparison unreadable');
        }
        return realFS.readFileSync(filePath, ...args);
      },
    };

    const code = run({repoRoot, fs, exec: () => 0, error: (message) => errors.push(message)});

    assert.equal(code, 2);
    assert.equal(readFileSync(baselinePath, 'utf8'), '{"findings":[]}\n');
    assert.match(errors.join('\n'), /comparison.*unreadable/i);
  });
});

test('treats a post-scan metadata read failure as exit 2 and attempts restoration', () => {
  withRepo(({repoRoot, baselinePath}) => {
    let stats = 0;
    const errors = [];
    const fs = {
      ...realFS,
      lstatSync(filePath, ...args) {
        if (filePath === baselinePath && ++stats === 2) throw new Error('comparison stat failed');
        return realFS.lstatSync(filePath, ...args);
      },
    };

    const code = run({repoRoot, fs, exec: () => 0, error: (message) => errors.push(message)});

    assert.equal(code, 2);
    assert.equal(readFileSync(baselinePath, 'utf8'), '{"findings":[]}\n');
    assert.match(errors.join('\n'), /comparison stat failed/i);
  });
});

test('reports restoration failure and never claims the reviewed baseline was restored', () => {
  withRepo(({repoRoot, baselinePath}) => {
    const errors = [];
    const fs = {
      ...realFS,
      renameSync() {
        throw new Error('rename blocked');
      },
    };

    const code = run({
      repoRoot,
      fs,
      exec: () => {
        writeFileSync(baselinePath, 'mutated\n');
        return 0;
      },
      error: (message) => errors.push(message),
    });

    assert.equal(code, 2);
    assert.equal(readFileSync(baselinePath, 'utf8'), 'mutated\n');
    assert.match(errors.join('\n'), /restoration failed.*rename blocked/i);
    assert.doesNotMatch(errors.join('\n'), /restored the reviewed baseline/);
  });
});
