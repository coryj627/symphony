import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import path from 'node:path';
import test from 'node:test';
import {fileURLToPath} from 'node:url';

const goRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repositoryRoot = path.resolve(goRoot, '..');

function git(args, options = {}) {
  const result = spawnSync('git', args, {
    cwd: repositoryRoot,
    encoding: 'utf8',
    shell: false,
    windowsHide: true,
    ...options,
  });
  assert.equal(result.error, undefined, result.error?.message);
  assert.equal(result.status, 0, result.stderr);
  return result.stdout;
}

test('tracked Codex schema JSON files check out with LF line endings', () => {
  const files = git(['ls-files', '-z', 'go/schema/codex'])
    .split('\0')
    .filter((name) => name.endsWith('.json'));
  assert.ok(files.length > 0, 'tracked Codex schema JSON files are missing');

  const output = git(['check-attr', '-z', 'eol', '--stdin'], {
    input: `${files.join('\0')}\0`,
  }).split('\0');

  const attributes = new Map();
  for (let index = 0; index + 2 < output.length; index += 3) {
    const [name, attribute, value] = output.slice(index, index + 3);
    assert.equal(attribute, 'eol');
    attributes.set(name, value);
  }
  for (const name of files) {
    assert.equal(attributes.get(name), 'lf', `${name} must use eol=lf`);
  }
});
