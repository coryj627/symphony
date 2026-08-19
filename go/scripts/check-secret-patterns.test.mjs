import assert from 'node:assert/strict';
import * as realFS from 'node:fs';
import {mkdirSync, mkdtempSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import test from 'node:test';

import {run, secretFingerprint} from './check-secret-patterns.mjs';

function writeFixtureFile(repoRoot, sourcePath, source) {
  const filePath = path.join(repoRoot, ...sourcePath.split('/'));
  mkdirSync(path.dirname(filePath), {recursive: true});
  writeFileSync(filePath, source);
}

function inventory(entries) {
  const records = entries.map((entry) => {
    const sourcePath = typeof entry === 'string' ? entry : entry.sourcePath;
    const indexState = typeof entry === 'string' || !entry.binary ? 'i/lf' : 'i/-text';
    return `${indexState} w/lf attr/\t${sourcePath}\0`;
  }).join('');
  return () => ({status: 0, stdout: Buffer.from(records), stderr: Buffer.alloc(0)});
}

function withFixture(files, fn) {
  const repoRoot = mkdtempSync(path.join(tmpdir(), 'symphony-secret-patterns-'));
  for (const [sourcePath, source] of Object.entries(files)) writeFixtureFile(repoRoot, sourcePath, source);
  try {
    fn(repoRoot);
  } finally {
    rmSync(repoRoot, {recursive: true, force: true});
  }
}

function scan(repoRoot, entries, options = {}) {
  const output = [];
  const errors = [];
  const code = run({
    repoRoot,
    git: inventory(entries),
    fixtureAllowlist: [],
    output: (message) => output.push(message),
    error: (message) => errors.push(message),
    ...options,
  });
  return {code, output, errors, messages: [...output, ...errors].join('\n')};
}

const githubFixture = () => ['github_', 'pat_', 'A'.repeat(24)].join('');
const linearFixture = () => ['lin_', 'api_', 'B'.repeat(20)].join('');
const slackFixture = () => ['xox', 'b-', 'E'.repeat(24)].join('');
const openAIFixture = () => ['sk-', 'proj-', 'F'.repeat(24)].join('');
const stripeFixture = () => ['sk', '_live_', 'G'.repeat(24)].join('');
const gitLabFixture = () => ['gl', 'pat-', 'H'.repeat(24)].join('');
const awsFixture = () => ['AK', 'IA', 'J'.repeat(16)].join('');
const googleFixture = () => ['AI', 'za', 'K'.repeat(35)].join('');
const npmFixture = () => ['npm', '_', 'L'.repeat(36)].join('');
const huggingFaceFixture = () => ['hf', '_', 'M'.repeat(24)].join('');
const privateKeyFixture = () => ['-----BEGIN ', 'PRIVATE KEY-----'].join('');
const authorizationFixture = () => ['Authorization: ', 'Bearer ', 'C'.repeat(24)].join('');
const credentialFixture = () => ['api_', 'token = "', 'D'.repeat(20), '"'].join('');

test('accepts tracked text without credential patterns and skips Git-classified binary files', () => {
  withFixture({
    'go/main.go': 'package main\n',
    'media/image.bin': Buffer.from([0, 1, 2, 3]),
  }, (repoRoot) => {
    const result = scan(repoRoot, ['go/main.go', {sourcePath: 'media/image.bin', binary: true}]);

    assert.equal(result.code, 0, result.messages);
    assert.match(result.messages, /1 text files, 1 binary files, 0 approved fixture matches/);
  });
});

for (const fixture of [
  {name: 'private key block', policy: 'private-key-block', value: privateKeyFixture()},
  {name: 'GitHub live token prefix', policy: 'live-token-prefix', value: githubFixture()},
  {name: 'Linear live token prefix', policy: 'live-token-prefix', value: linearFixture()},
  {name: 'Slack live token prefix', policy: 'live-token-prefix', value: slackFixture()},
  {name: 'OpenAI live token prefix', policy: 'live-token-prefix', value: openAIFixture()},
  {name: 'Stripe live token prefix', policy: 'live-token-prefix', value: stripeFixture()},
  {name: 'GitLab live token prefix', policy: 'live-token-prefix', value: gitLabFixture()},
  {name: 'AWS live token prefix', policy: 'live-token-prefix', value: awsFixture()},
  {name: 'Google live token prefix', policy: 'live-token-prefix', value: googleFixture()},
  {name: 'npm live token prefix', policy: 'live-token-prefix', value: npmFixture()},
  {name: 'Hugging Face live token prefix', policy: 'live-token-prefix', value: huggingFaceFixture()},
  {name: 'literal authorization value', policy: 'authorization-value', value: authorizationFixture()},
  {name: 'credential assignment', policy: 'credential-literal', value: credentialFixture()},
]) {
  test(`rejects a ${fixture.name} without copying the value to output`, () => {
    withFixture({'go/source.txt': `${fixture.value}\n`}, (repoRoot) => {
      const result = scan(repoRoot, ['go/source.txt']);

      assert.equal(result.code, 1, result.messages);
      assert.match(result.messages, new RegExp(`go/source\\.txt:1 \\[${fixture.policy}\\]`));
      assert.equal(result.messages.includes(fixture.value), false);
      assert.equal(result.messages.includes(secretFingerprint(fixture.value)), false);
    });
  });
}

test('reports lines across supported source terminators without exposing the match', () => {
  const value = credentialFixture();
  for (const [name, terminator] of [
    ['CRLF', '\r\n'],
    ['lone CR', '\r'],
    ['LF', '\n'],
    ['U+2028', '\u2028'],
    ['U+2029', '\u2029'],
  ]) {
    withFixture({'go/source.txt': `safe${terminator}${value}\n`}, (repoRoot) => {
      const result = scan(repoRoot, ['go/source.txt']);

      assert.equal(result.code, 1, `${name}: ${result.messages}`);
      assert.match(result.messages, /go\/source\.txt:2 \[credential-literal\]/, name);
      assert.equal(result.messages.includes(value), false, name);
    });
  }

  withFixture({'go/source.txt': `safe\r\nsafe\u2028${value}\n`}, (repoRoot) => {
    const result = scan(repoRoot, ['go/source.txt']);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /go\/source\.txt:3 \[credential-literal\]/);
    assert.equal(result.messages.includes(value), false);
  });
});

for (const [name, value] of [
  ['raw-string credential assignment', ['password = ', '`', 'N'.repeat(20), '`'].join('')],
  ['unquoted environment credential assignment', ['API_', 'TOKEN=', 'P'.repeat(20)].join('')],
  ['Basic authorization value', ['Authorization: ', 'Basic ', 'Q'.repeat(24)].join('')],
]) {
  test(`rejects a ${name}`, () => {
    withFixture({'go/source.txt': `${value}\n`}, (repoRoot) => {
      const result = scan(repoRoot, ['go/source.txt']);

      assert.equal(result.code, 1, result.messages);
      assert.equal(result.messages.includes(value), false);
    });
  });
}

test('rejects a credential assignment split across source lines', () => {
  const value = ['token =\n  "', 'R'.repeat(20), '"'].join('');
  withFixture({'go/source.txt': `${value}\n`}, (repoRoot) => {
    const result = scan(repoRoot, ['go/source.txt']);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /go\/source\.txt:1 \[credential-literal\]/);
    assert.equal(result.messages.includes(value), false);
  });
});

test('accepts an exact path-line-policy-fingerprint fixture allowlist entry', () => {
  const value = githubFixture();
  const allowlist = [{
    sourcePath: 'go/internal/client_test.go',
    line: 2,
    policy: 'live-token-prefix',
    fingerprint: secretFingerprint(value),
  }];
  withFixture({'go/internal/client_test.go': `safe\n${value}\n`}, (repoRoot) => {
    const result = scan(repoRoot, ['go/internal/client_test.go'], {fixtureAllowlist: allowlist});

    assert.equal(result.code, 0, result.messages);
    assert.match(result.messages, /1 approved fixture matches/);
  });
});

for (const mutation of [
  {name: 'path', sourcePath: 'go/internal/other_test.go', line: 2, value: githubFixture()},
  {name: 'line', sourcePath: 'go/internal/client_test.go', line: 3, value: githubFixture()},
  {name: 'fingerprint', sourcePath: 'go/internal/client_test.go', line: 2, value: ['github_', 'pat_', 'Z'.repeat(24)].join('')},
]) {
  test(`fixture allowlist does not broaden across a changed ${mutation.name}`, () => {
    const original = githubFixture();
    const source = mutation.line === 3 ? `safe\nsafe\n${mutation.value}\n` : `safe\n${mutation.value}\n`;
    const allowlist = [{
      sourcePath: 'go/internal/client_test.go',
      line: 2,
      policy: 'live-token-prefix',
      fingerprint: secretFingerprint(original),
    }];
    withFixture({[mutation.sourcePath]: source}, (repoRoot) => {
      const result = scan(repoRoot, [mutation.sourcePath], {fixtureAllowlist: allowlist});

      assert.equal(result.code, 1, result.messages);
      assert.match(result.messages, /\[live-token-prefix\]/);
      assert.match(result.messages, /\[stale-fixture-allowlist\]/);
      assert.equal(result.messages.includes(mutation.value), false);
    });
  });
}

test('rejects stale allowlist entries even when source contains no finding', () => {
  const allowlist = [{
    sourcePath: 'go/internal/client_test.go',
    line: 1,
    policy: 'live-token-prefix',
    fingerprint: secretFingerprint(githubFixture()),
  }];
  withFixture({'go/internal/client_test.go': 'safe\n'}, (repoRoot) => {
    const result = scan(repoRoot, ['go/internal/client_test.go'], {fixtureAllowlist: allowlist});

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /go\/internal\/client_test\.go:1 \[stale-fixture-allowlist\]/);
  });
});

test('does not scan an untracked file', () => {
  withFixture({
    'go/tracked.txt': 'safe\n',
    'go/untracked.txt': `${githubFixture()}\n`,
  }, (repoRoot) => {
    const result = scan(repoRoot, ['go/tracked.txt']);

    assert.equal(result.code, 0, result.messages);
  });
});

test('fails closed when a tracked entry is a symbolic link', () => {
  withFixture({'go/link.txt': 'safe\n'}, (repoRoot) => {
    const fs = {
      lstatSync(filePath) {
        if (filePath.endsWith(path.join('go', 'link.txt'))) {
          return {isDirectory: () => false, isFile: () => true, isSymbolicLink: () => true, size: 5};
        }
        return realFS.lstatSync(filePath);
      },
      readFileSync: realFS.readFileSync,
    };
    const result = scan(repoRoot, ['go/link.txt'], {fs});

    assert.equal(result.code, 2, result.messages);
    assert.match(result.messages, /not a regular file/);
  });
});

for (const fixture of [
  {name: 'parent traversal', sourcePath: '../outside.txt'},
  {name: 'POSIX absolute form', sourcePath: '/absolute.txt'},
  {name: 'Windows absolute form', sourcePath: 'C:/absolute.txt'},
  {name: 'backslash separator', sourcePath: 'go\\source.txt'},
  {name: 'line break', sourcePath: 'go/bad\nname.txt'},
  {name: 'bidirectional control', sourcePath: 'go/bad\u202ename.txt'},
]) {
  test(`rejects unsafe tracked path with ${fixture.name}`, () => {
    withFixture({}, (repoRoot) => {
      const result = scan(repoRoot, [fixture.sourcePath]);

      assert.equal(result.code, 2, result.messages);
      assert.match(result.messages, /unsafe|invalid tracked path/);
      assert.equal(result.messages.includes(fixture.sourcePath), false);
    });
  });
}

test('fails closed when git cannot produce the tracked inventory', () => {
  withFixture({}, (repoRoot) => {
    const result = scan(repoRoot, [], {
      git: () => ({status: 7, stdout: Buffer.alloc(0), stderr: Buffer.from('unsafe details')}),
    });

    assert.equal(result.code, 2, result.messages);
    assert.match(result.messages, /git ls-files --eol failed with exit 7/);
    assert.doesNotMatch(result.messages, /unsafe details/);
  });
});

test('fails closed on duplicate or malformed tracked inventory entries', () => {
  withFixture({'go/source.txt': 'safe\n'}, (repoRoot) => {
    const duplicate = scan(repoRoot, ['go/source.txt', 'go/source.txt']);
    const malformed = scan(repoRoot, [], {
      git: () => ({status: 0, stdout: Buffer.from('missing-tab\0'), stderr: Buffer.alloc(0)}),
    });

    assert.equal(duplicate.code, 2, duplicate.messages);
    assert.match(duplicate.messages, /duplicate tracked path/);
    assert.equal(malformed.code, 2, malformed.messages);
    assert.match(malformed.messages, /malformed tracked entry/);
  });
});

test('fails closed on non-UTF-8 tracked metadata or text', () => {
  withFixture({'go/source.txt': Buffer.from([0xff])}, (repoRoot) => {
    const invalidPath = scan(repoRoot, [], {
      git: () => ({
        status: 0,
        stdout: Buffer.concat([Buffer.from('i/lf w/lf attr/\tgo/'), Buffer.from([0xff, 0])]),
        stderr: Buffer.alloc(0),
      }),
    });
    const invalidText = scan(repoRoot, ['go/source.txt']);

    assert.equal(invalidPath.code, 2, invalidPath.messages);
    assert.match(invalidPath.messages, /tracked path.*valid UTF-8/i);
    assert.equal(invalidText.code, 2, invalidText.messages);
    assert.match(invalidText.messages, /tracked text file.*valid UTF-8/i);
  });
});

test('fails closed when a tracked text file exceeds the bounded read', () => {
  withFixture({'go/source.txt': Buffer.alloc((2 * 1024 * 1024) + 1, 0x61)}, (repoRoot) => {
    const result = scan(repoRoot, ['go/source.txt']);

    assert.equal(result.code, 2, result.messages);
    assert.match(result.messages, /exceeds the scan limit/);
  });
});

test('fails closed when a tracked file changes while it is read', () => {
  withFixture({'go/source.txt': 'safe\n'}, (repoRoot) => {
    const fs = {
      lstatSync: realFS.lstatSync,
      openSync: realFS.openSync,
      fstatSync: realFS.fstatSync,
      readFileSync: () => Buffer.alloc(0),
      closeSync: realFS.closeSync,
    };
    const result = scan(repoRoot, ['go/source.txt'], {fs});

    assert.equal(result.code, 2, result.messages);
    assert.match(result.messages, /changed while scanning/);
  });
});

test('uses the exact tracked-text Git inventory command in the selected root', () => {
  withFixture({'go/source.txt': 'safe\n'}, (repoRoot) => {
    const calls = [];
    const result = scan(repoRoot, ['go/source.txt'], {
      git: (args, options) => {
        calls.push({args, options});
        return inventory(['go/source.txt'])(args, options);
      },
    });

    assert.equal(result.code, 0, result.messages);
    assert.deepEqual(calls.map(({args}) => args), [['ls-files', '--eol', '-z']]);
    assert.equal(calls[0].options.cwd, path.resolve(repoRoot));
  });
});

test('rejects non-fixture paths in the allowlist', () => {
  withFixture({'go/main.go': 'package main\n'}, (repoRoot) => {
    const result = scan(repoRoot, ['go/main.go'], {
      fixtureAllowlist: [{
        sourcePath: 'go/main.go',
        line: 1,
        policy: 'live-token-prefix',
        fingerprint: '0'.repeat(64),
      }],
    });

    assert.equal(result.code, 2, result.messages);
    assert.match(result.messages, /non-fixture path/);
  });
});

test('rejects duplicate fixture allowlist entries', () => {
  const entry = {
    sourcePath: 'go/internal/client_test.go',
    line: 1,
    policy: 'live-token-prefix',
    fingerprint: '0'.repeat(64),
  };
  withFixture({'go/internal/client_test.go': 'safe\n'}, (repoRoot) => {
    const result = scan(repoRoot, ['go/internal/client_test.go'], {
      fixtureAllowlist: [entry, {...entry}],
    });

    assert.equal(result.code, 2, result.messages);
    assert.match(result.messages, /duplicate entry/);
  });
});
