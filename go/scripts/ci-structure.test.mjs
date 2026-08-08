import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import {fileURLToPath} from 'node:url';

const goRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workflow = readFileSync(path.join(goRoot, '..', '.github', 'workflows', 'go.yml'), 'utf8');

function sourceAccessibilitySteps(source) {
  const jobStart = source.indexOf('\n  source-accessibility:\n');
  assert.notEqual(jobStart, -1, 'source-accessibility job is missing');
  const job = source.slice(jobStart);
  return job
    .split(/\n      - name: /)
    .slice(1)
    .map((block) => {
      const lines = block.split('\n');
      const name = lines.shift().trim();
      const env = new Map();
      let inEnv = false;
      let inRun = false;
      const run = [];
      for (const line of lines) {
        if (line === '        env:') {
          inEnv = true;
          inRun = false;
          continue;
        }
        if (/^        run:/.test(line)) {
          inEnv = false;
          inRun = true;
          continue;
        }
        const envMatch = inEnv ? line.match(/^          ([A-Z0-9_]+): (.+)$/) : null;
        if (envMatch) env.set(envMatch[1], envMatch[2]);
        if (inRun && line.startsWith('          ')) run.push(line.slice(10));
      }
      return {name, env, run: run.join('\n')};
    });
}

test('private release token exists only in a download-only step', () => {
  const steps = sourceAccessibilitySteps(workflow);
  const tokenSteps = steps.filter((step) =>
    [...step.env.values()].some((value) => value.includes('A11Y_RELEASE_READ_TOKEN')),
  );

  assert.equal(tokenSteps.length, 1);
  const [download] = tokenSteps;
  assert.match(download.name, /^Download a11y-check-web v0\.3\.1$/);
  assert.deepEqual([...download.env], [
    ['GH_TOKEN', '${{ secrets.A11Y_RELEASE_READ_TOKEN }}'],
  ]);
  assert.match(download.run, /gh release download v0\.3\.1/);
  for (const commandLine of download.run.split('\n').map((line) => line.trim())) {
    assert.doesNotMatch(commandLine, /^(?:npm|node|a11y-check-web)\b/);
  }
  assert.doesNotMatch(download.run, /(?:set -x|--verbose|--debug|https?:|--header|--include|--body)/i);
});

test('tokenless install step accepts one exact regular artifact and verifies CLI version', () => {
  const steps = sourceAccessibilitySteps(workflow);
  const install = steps.find((step) => step.name === 'Install and verify a11y-check-web v0.3.1');

  assert.ok(install, 'tokenless install/verify step is missing');
  assert.equal(install.env.size, 0);
  assert.match(install.run, /a11y-check-web-mcp-server-0\.3\.1\.tgz/);
  assert.doesNotMatch(install.run, /a11y-check-web-mcp-server-\*\.tgz/);
  assert.match(install.run, /find .* -type f/);
  assert.match(install.run, /find .* -mindepth 1 -maxdepth 1/);
  assert.match(install.run, /test ! -L/);
  assert.match(install.run, /npm install -g "\$expected_artifact"/);
  assert.match(install.run, /test "\$\(a11y-check-web --version\)" = "0\.3\.1"/);
});

test('release download requests the exact v0.3.1 asset name without wildcards', () => {
  const steps = sourceAccessibilitySteps(workflow);
  const download = steps.find((step) => step.name === 'Download a11y-check-web v0.3.1');

  assert.ok(download, 'download step is missing');
  assert.match(download.run, /--pattern 'a11y-check-web-mcp-server-0\.3\.1\.tgz'/);
  assert.doesNotMatch(download.run, /--pattern '[^']*\*/);
});
