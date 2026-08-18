import assert from 'node:assert/strict';
import {existsSync, readFileSync} from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import {fileURLToPath} from 'node:url';

const goRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workflowsRoot = path.join(goRoot, '..', '.github', 'workflows');
const normalizeWorkflow = (source) => source.replace(/\r\n?/g, '\n');
const mainWorkflow = normalizeWorkflow(readFileSync(path.join(workflowsRoot, 'go.yml'), 'utf8'));
const integrationsPath = path.join(workflowsRoot, 'go-integrations.yml');
const integrationsWorkflow = existsSync(integrationsPath)
  ? normalizeWorkflow(readFileSync(integrationsPath, 'utf8'))
  : '';

const githubSentinel = 'SKIPPED: GitHub live profile not enabled';
const linearSentinel = 'SKIPPED: Linear live profile not enabled';
const codexSentinel = 'SKIPPED: real Codex smoke';
const disabledGitHubCommand = 'go test -v -tags=integration_live -count=1 -timeout=2m ./internal/tracker/github';
const disabledLinearCommand = 'go test -v -tags=integration_live -count=1 -timeout=2m ./internal/tracker/linear';
const disabledCodexCommand = "go test -v -count=1 -run '^TestRealCodexAppServerSmoke$' ./internal/codex";
const secretExpressionPattern = /\$\{\{\s*secrets\b/;

function workflowHeader(source) {
  const jobs = source.indexOf('\njobs:\n');
  assert.notEqual(jobs, -1, 'workflow jobs block is missing');
  return source.slice(0, jobs);
}

function jobBlock(source, identifier) {
  const marker = `\n  ${identifier}:\n`;
  const start = source.indexOf(marker);
  assert.notEqual(start, -1, `${identifier} job is missing`);
  const bodyStart = start + marker.length;
  const remaining = source.slice(bodyStart);
  const next = remaining.search(/\n  [A-Za-z0-9_-]+:\n/);
  return next === -1 ? remaining : remaining.slice(0, next);
}

function namedSteps(job) {
  return job
    .split(/\n      - name: /)
    .slice(1)
    .map((block) => {
      const newline = block.indexOf('\n');
      return {
        name: block.slice(0, newline).trim(),
        source: block.slice(newline + 1),
      };
    });
}

function namedStep(job, name) {
  const step = namedSteps(job).find((candidate) => candidate.name === name);
  assert.ok(step, `${name} step is missing`);
  return step.source;
}

function assertNativeMatrix(job) {
  assert.match(job, /runs-on: \$\{\{ matrix\.os \}\}/);
  assert.match(job, /fail-fast: false/);
  assert.match(job, /max-parallel: 1/);
  const matrix = job.match(/matrix:\n\s+os:\n((?:\s+- [^\n]+\n?)+)/);
  assert.ok(matrix, 'native runner matrix is missing');
  assert.deepEqual(
    [...matrix[1].matchAll(/^\s+- (\S+)$/gm)].map((match) => match[1]),
    ['macos-15', 'windows-2025'],
  );
}

function assertPinnedActions(source) {
  const uses = [...source.matchAll(/^\s*uses:\s*([^@\s]+)@([^\s#]+).*$/gm)];
  assert.ok(uses.length > 0, 'workflow contains no actions');
  for (const [, action, revision] of uses) {
    assert.match(revision, /^[0-9a-f]{40}$/, `${action} is not pinned to a full commit SHA`);
  }
}

function assertPinnedGoSetup(job) {
  const setup = namedStep(job, 'Set up Go');
  assert.match(setup, /uses: actions\/setup-go@[0-9a-f]{40}/);
  assert.match(setup, /go-version-file: go\/go\.mod/);
  assert.match(setup, /cache: false/);
}

function sourceAccessibilitySteps(source) {
  return namedSteps(jobBlock(source, 'source-accessibility'));
}

function secretNames(source) {
  const markers = source.match(/\$\{\{\s*secrets\b/g) ?? [];
  const matches = [...source.matchAll(/\$\{\{\s*secrets(?:\.([A-Za-z_][A-Za-z0-9_]*)|\[\s*(['"])([A-Za-z_][A-Za-z0-9_]*)\2\s*\])\s*\}\}/g)];
  assert.equal(matches.length, markers.length, 'unsupported secret expression syntax');
  return matches.map((match) => match[1] ?? match[3]);
}

function assertManualDispatchInputs(header) {
  const inputNames = [...header.matchAll(/^      ([A-Za-z0-9_-]+):$/gm)].map((match) => match[1]);
  assert.deepEqual(inputNames, ['github', 'linear', 'codex'], 'manual dispatch must define exactly github, linear, and codex inputs');
  for (const provider of ['github', 'linear', 'codex']) {
    const input = header.match(new RegExp(`\\n      ${provider}:\\n([\\s\\S]*?)(?=\\n      [a-z]|\\npermissions:)`));
    assert.ok(input, `${provider} input is missing`);
    assert.match(input[1], /required: true/);
    assert.match(input[1], /type: boolean/);
    assert.match(input[1], /default: false/);
  }
}

function assertSelectedSecretSteps(job, title) {
  const allowedStepNames = [
    `Verify ${title} live prerequisites`,
    `Run ${title} live profile`,
  ];
  const provider = title.toUpperCase();
  const scopeName = title === 'GitHub' ? 'REPO' : 'PROJECT';
  const allowedSecretNames = [
    `SYMPHONY_${provider}_TEST_${scopeName}`,
    `SYMPHONY_${provider}_TEST_TOKEN`,
  ];
  const seenSecretNames = new Set();
  let currentStepName = '';
  for (const line of job.split('\n')) {
    const namedStepMatch = line.match(/^      - name: (.+)$/);
    if (namedStepMatch) {
      currentStepName = namedStepMatch[1].trim();
    } else if (/^      - /.test(line) || /^    [A-Za-z0-9_-]+:/.test(line)) {
      currentStepName = '';
    }
    for (const secretName of secretNames(line)) {
      assert.ok(allowedStepNames.includes(currentStepName), 'secret expression is outside an allowed named step');
      assert.ok(allowedSecretNames.includes(secretName), `unexpected secret expression: ${secretName}`);
      seenSecretNames.add(secretName);
    }
  }
  assert.deepEqual([...seenSecretNames].sort(), [...allowedSecretNames].sort());

  const secretSteps = namedSteps(job).filter((step) => secretExpressionPattern.test(step.source));
  assert.deepEqual(secretSteps.map((step) => step.name), allowedStepNames);
  return secretSteps;
}

test('workflow parsing normalizes Windows line endings', () => {
  const source = normalizeWorkflow('name: Fixture\r\npermissions:\r\n  contents: read\r\njobs:\r\n  fixture:\r\n    runs-on: windows-2025\r\n');

  assert.match(workflowHeader(source), /^name: Fixture$/m);
  assert.match(jobBlock(source, 'fixture'), /runs-on: windows-2025/);
});

test('main CI uses neutral names and exact supported native runner labels', () => {
  const header = workflowHeader(mainWorkflow);
  const build = jobBlock(mainWorkflow, 'build-test');
  const source = jobBlock(mainWorkflow, 'source-accessibility');

  assert.match(header, /^name: Go CI$/m);
  assert.match(header, /\n  pull_request:\n/);
  assert.match(header, /\n  push:\n\s+branches:\n\s+- main\n/);
  assert.match(header, /permissions:\n  contents: read/);
  assert.doesNotMatch(mainWorkflow, /Phase 1|phase-1/i);
  assertNativeMatrix(build);
  assert.match(source, /runs-on: macos-15/);
  assert.doesNotMatch(mainWorkflow, /macos-latest|windows-latest/);
  assertPinnedActions(mainWorkflow);
  assertPinnedGoSetup(build);
  assert.match(namedStep(build, 'Set up Node.js'), /node-version: 24\.18\.0/);
});

test('main CI runs build, default Go, race, disabled profiles, and every accessibility gate', () => {
  const build = jobBlock(mainWorkflow, 'build-test');
  const source = jobBlock(mainWorkflow, 'source-accessibility');

  assert.doesNotMatch(build, /environment:|SYMPHONY_(?:RUN|GITHUB_TEST|LINEAR_TEST)_/);
  assert.doesNotMatch(build, secretExpressionPattern);
  assert.match(build, /run: go build \.\/cmd\/symphony/);
  assert.match(build, /run: go test \.\/\.\.\./);
  assert.match(build, /run: go vet \.\/\.\.\./);
  assert.match(build, /if: runner\.os == 'macOS'\n\s+run: go test -race \.\/\.\.\./);
  for (const [stepName, command, sentinel] of [
    ['Verify disabled GitHub live profile', disabledGitHubCommand, githubSentinel],
    ['Verify disabled Linear live profile', disabledLinearCommand, linearSentinel],
    ['Verify disabled real Codex smoke', disabledCodexCommand, codexSentinel],
  ]) {
    const step = namedStep(build, stepName);
    assert.match(step, new RegExp(command.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
    assert.match(step, new RegExp(sentinel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
    assert.match(step, /status=\$\?/);
    assert.match(step, /test "\$status" -eq 0/);
  }
  assert.match(build, /run: npm run test:wrappers/);
  assert.match(build, /run: npm run conformance:upstream/);
  assert.match(build, /run: npm run html:validate/);
  assert.match(build, /run: npm run test:a11y/);
  assert.match(build, /npx playwright install chromium webkit/);
  assert.match(source, /run: node scripts\/a11y-scan-all\.mjs/);
});

test('manual integration workflow is dispatch-only with exact boolean inputs and read-only permissions', () => {
  assert.notEqual(integrationsWorkflow, '', 'manual integration workflow is missing');
  const header = workflowHeader(integrationsWorkflow);

  assert.match(header, /^name: Go live integrations$/m);
  assert.match(header, /on:\n  workflow_dispatch:\n    inputs:/);
  assert.doesNotMatch(header, /\n  (?:push|pull_request|pull_request_target|schedule):/);
  assert.match(header, /permissions:\n  contents: read/);
  assertManualDispatchInputs(header);
  assertPinnedActions(integrationsWorkflow);
});

test('manual dispatch input validation rejects a fourth input key', () => {
  const header = workflowHeader(integrationsWorkflow);
  const unsafeHeader = header.replace(
    '\npermissions:\n',
    '\n      extra:\n        required: true\n        type: boolean\n        default: false\n\npermissions:\n',
  );

  assert.throws(() => assertManualDispatchInputs(unsafeHeader), /exactly github, linear, and codex/);
});

test('selected integrations on a non-main dispatch fail visibly without credentials', () => {
  const guard = jobBlock(integrationsWorkflow, 'selected-ref-guard');

  assert.ok(guard.includes("if: ${{ (inputs.github == true || inputs.linear == true || inputs.codex == true) && github.ref != 'refs/heads/main' }}"));
  assert.match(guard, /runs-on: macos-15/);
  assert.doesNotMatch(guard, /environment:|SYMPHONY_[A-Z_]+/);
  assert.doesNotMatch(guard, secretExpressionPattern);
  assert.doesNotMatch(guard, /uses:|go test|npm|playwright|https?:\/\//i);

  const reject = namedStep(guard, 'Reject selected integrations outside main');
  assert.match(reject, /refs\/heads\/main/);
  assert.match(reject, /exit 1/);
});

test('selected secret validation rejects a job-level secret before the first step', () => {
  const job = jobBlock(integrationsWorkflow, 'github-live');
  const unsafeJob = job.replace(
    '\n    steps:\n',
    '\n    env:\n      JOB_TOKEN: ${{ secrets.SYMPHONY_GITHUB_TEST_TOKEN }}\n\n    steps:\n',
  );

  assert.throws(() => assertSelectedSecretSteps(unsafeJob, 'GitHub'), /outside an allowed named step/);
});

test('selected secret validation rejects a job-level bracket-style secret before the first step', () => {
  const job = jobBlock(integrationsWorkflow, 'github-live');
  const unsafeJob = job.replace(
    '\n    steps:\n',
    "\n    env:\n      JOB_TOKEN: ${{ secrets['SYMPHONY_GITHUB_TEST_TOKEN'] }}\n\n    steps:\n",
  );

  assert.throws(() => assertSelectedSecretSteps(unsafeJob, 'GitHub'), /outside an allowed named step/);
});

test('selected secret validation rejects an extra secret inside an allowed step', () => {
  const job = jobBlock(integrationsWorkflow, 'github-live');
  const expected = '          SYMPHONY_GITHUB_TEST_REPO: ${{ secrets.SYMPHONY_GITHUB_TEST_REPO }}';
  const unsafeJob = job.replace(expected, expected + '\n          EXTRA: ${{ secrets.EXTRA }}');

  assert.throws(() => assertSelectedSecretSteps(unsafeJob, 'GitHub'), /unexpected secret expression/);
});

for (const provider of ['github', 'linear']) {
  const title = provider === 'github' ? 'GitHub' : 'Linear';
  const environment = `${provider}-live`;
  const scopeName = provider === 'github' ? 'REPO' : 'PROJECT';
  const packageName = `./internal/tracker/${provider}`;
  const sentinel = provider === 'github' ? githubSentinel : linearSentinel;
  const command = provider === 'github' ? disabledGitHubCommand : disabledLinearCommand;
  const other = provider === 'github' ? 'LINEAR' : 'GITHUB';

  test(`${title} selected job is main-only, provider-isolated, and runs the exact live command`, () => {
    const job = jobBlock(integrationsWorkflow, `${provider}-live`);
    assertNativeMatrix(job);
    assertPinnedGoSetup(job);
    assert.ok(job.includes("if: ${{ inputs." + provider + " == true && github.ref == 'refs/heads/main' }}"));
    assert.match(job, new RegExp(`environment: ${environment}`));
    assert.doesNotMatch(job, new RegExp(`environment: (?!${environment})[a-z-]+-live`));
    assert.doesNotMatch(job, new RegExp(`SYMPHONY_${other}_TEST_`));
    const scopeVariable = `SYMPHONY_${title.toUpperCase()}_TEST_${scopeName}`;
    const tokenVariable = `SYMPHONY_${title.toUpperCase()}_TEST_TOKEN`;
    assert.ok(job.includes(scopeVariable + ': ${{ secrets.' + scopeVariable + ' }}'));
    assert.ok(job.includes(tokenVariable + ': ${{ secrets.' + tokenVariable + ' }}'));
    assert.match(job, new RegExp(`SYMPHONY_RUN_${title.toUpperCase()}_LIVE: '1'`));
    assert.match(job, new RegExp(`go test -v -tags=integration_live -count=1 -timeout=2m ${packageName.replaceAll('/', '\\/')}`));

    const checkout = namedStep(job, 'Checkout dispatched SHA');
    assert.match(checkout, /uses: actions\/checkout@[0-9a-f]{40}/);
    assert.match(checkout, /ref: \$\{\{ github\.sha \}\}/);
    assert.match(checkout, /persist-credentials: false/);

    const secretSteps = assertSelectedSecretSteps(job, title);
    for (const step of secretSteps) {
      const runStart = step.source.indexOf('\n        run:');
      const run = runStart === -1 ? '' : step.source.slice(runStart);
      assert.doesNotMatch(run, secretExpressionPattern);
      assert.doesNotMatch(run, /set -x|--trace|--debug/i);
    }

    const prerequisite = namedStep(job, `Verify ${title} live prerequisites`);
    for (const variable of [scopeVariable, tokenVariable]) {
      assert.ok(prerequisite.includes('if [[ -z "${' + variable + ':-}" ]]'));
      assert.ok(prerequisite.includes('echo "::error::' + variable + ' is required"'));
    }
    assert.doesNotMatch(prerequisite, /echo[^\n]*\$\{SYMPHONY_/);
  });

  test(`${title} unselected job proves the exact disabled sentinel without secrets or environments`, () => {
    const job = jobBlock(integrationsWorkflow, `${provider}-disabled`);
    assertNativeMatrix(job);
    assertPinnedGoSetup(job);
    assert.ok(job.includes("if: ${{ inputs." + provider + " == false }}"));
    assert.doesNotMatch(job, /environment:/);
    assert.doesNotMatch(job, /SYMPHONY_[A-Z]+_TEST_(?:TOKEN|REPO|PROJECT)|SYMPHONY_RUN_[A-Z]+_LIVE/);
    assert.doesNotMatch(job, secretExpressionPattern);
    assert.match(job, new RegExp(command.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
    assert.match(job, new RegExp(sentinel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
    assert.doesNotMatch(job, /npm|playwright|a11y-check-web|https?:\/\//i);
    assert.doesNotMatch(job, /\b(?:curl|wget|Invoke-WebRequest|gh\s+(?:api|release))\b/i);

    const checkout = namedStep(job, 'Checkout dispatched SHA');
    assert.match(checkout, /uses: actions\/checkout@[0-9a-f]{40}/);
    assert.match(checkout, /ref: \$\{\{ github\.sha \}\}/);
    assert.match(checkout, /persist-credentials: false/);
  });
}

test('real Codex selected job is main-only, native, pinned, and genuinely opt-in', () => {
  const job = jobBlock(integrationsWorkflow, 'codex-live');
  assertNativeMatrix(job);
  assertPinnedGoSetup(job);
  assert.ok(job.includes("if: ${{ inputs.codex == true && github.ref == 'refs/heads/main' }}"));
  assert.match(job, /environment: codex-live/);
  assert.doesNotMatch(job, secretExpressionPattern);
  assert.match(namedStep(job, 'Set up Node.js'), /node-version: 24\.18\.0/);
  assert.match(namedStep(job, 'Install reviewed Codex CLI'), /@openai\/codex@0\.144\.1/);
  const smoke = namedStep(job, 'Run real Codex app-server smoke');
  assert.match(smoke, /SYMPHONY_REAL_CODEX_SMOKE: '1'/);
  assert.match(smoke, /SYMPHONY_REAL_CODEX_WORKFLOW: \$\{\{ github\.workspace \}\}\/go\/testdata\/manual\/WORKFLOW\.md/);
  assert.match(smoke, /go test -v -count=1 -timeout=2m -run '\^TestRealCodexAppServerSmoke\$' \.\/internal\/codex/);
  assert.doesNotMatch(smoke, /SKIPPED: real Codex smoke|\|\|\s*true/);
});

test('real Codex unselected job proves the exact disabled sentinel without install or network', () => {
  const job = jobBlock(integrationsWorkflow, 'codex-disabled');
  assertNativeMatrix(job);
  assertPinnedGoSetup(job);
  assert.ok(job.includes("if: ${{ inputs.codex == false }}"));
  assert.doesNotMatch(job, /environment:|secrets\.|npm|playwright|https?:\/\//i);
  const step = namedStep(job, 'Verify disabled real Codex smoke');
  assert.match(step, new RegExp(disabledCodexCommand.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
  assert.match(step, /SKIPPED: real Codex smoke/);
  assert.match(step, /test "\$status" -eq 0/);
});

test('manual provider secrets and environments occur only in their selected provider jobs', () => {
  const githubJob = jobBlock(integrationsWorkflow, 'github-live');
  const linearJob = jobBlock(integrationsWorkflow, 'linear-live');
  const withoutGitHub = integrationsWorkflow.replace(githubJob, '');
  const withoutLinear = integrationsWorkflow.replace(linearJob, '');

  assert.ok(!secretNames(withoutGitHub).some((name) => name.startsWith('SYMPHONY_GITHUB_TEST_')));
  assert.ok(!secretNames(withoutLinear).some((name) => name.startsWith('SYMPHONY_LINEAR_TEST_')));
  assert.equal((integrationsWorkflow.match(/^    environment: (?:github|linear)-live$/gm) ?? []).length, 2);
});

test('workflows disable caches and contain no artifact, trace, or command-argument secret paths', () => {
  const combined = `${mainWorkflow}\n${integrationsWorkflow}`;
  assert.doesNotMatch(combined, /actions\/(?:cache|upload-artifact|download-artifact)@/);
  assert.doesNotMatch(combined, /^\s*cache:\s*(?:true|npm|yarn|pnpm)\s*$/m);
  assert.equal(
    (combined.match(/uses:\s*actions\/setup-go@[0-9a-f]{40}/g) ?? []).length,
    (combined.match(/^\s*cache:\s*false\s*$/gm) ?? []).length,
    'every setup-go step must explicitly disable its default cache',
  );
  assert.doesNotMatch(combined, /set -x|ACTIONS_STEP_DEBUG|--trace|trace:\s*(?!off)/i);
  for (const workflow of [mainWorkflow, integrationsWorkflow]) {
    for (const identifier of ['build-test', 'source-accessibility', 'github-live', 'linear-live', 'codex-live', 'github-disabled', 'linear-disabled', 'codex-disabled']) {
      if (!workflow.includes(`\n  ${identifier}:\n`)) continue;
      for (const step of namedSteps(jobBlock(workflow, identifier))) {
        const runStart = step.source.indexOf('\n        run:');
        const run = runStart === -1 ? '' : step.source.slice(runStart);
        assert.doesNotMatch(run, secretExpressionPattern, `${identifier}/${step.name} passes a secret expression in a command`);
      }
    }
  }
});

test('private release token exists only in a download-only step', () => {
  const steps = sourceAccessibilitySteps(mainWorkflow);
  const tokenSteps = steps.filter((step) => step.source.includes('A11Y_RELEASE_READ_TOKEN'));

  assert.equal(tokenSteps.length, 1);
  const [download] = tokenSteps;
  assert.match(download.name, /^Download a11y-check-web v0\.3\.1$/);
  assert.match(download.source, /GH_TOKEN: \$\{\{ secrets\.A11Y_RELEASE_READ_TOKEN \}\}/);
  assert.match(download.source, /gh release download v0\.3\.1/);
  assert.doesNotMatch(download.source, /(?:set -x|--verbose|--debug|https?:|--header|--include|--body)/i);
});

test('tokenless install step accepts one exact regular artifact and verifies CLI version', () => {
  const steps = sourceAccessibilitySteps(mainWorkflow);
  const install = steps.find((step) => step.name === 'Install and verify a11y-check-web v0.3.1');

  assert.ok(install, 'tokenless install/verify step is missing');
  assert.doesNotMatch(install.source, secretExpressionPattern);
  assert.match(install.source, /a11y-check-web-mcp-server-0\.3\.1\.tgz/);
  assert.doesNotMatch(install.source, /a11y-check-web-mcp-server-\*\.tgz/);
  assert.match(install.source, /find .* -type f/);
  assert.match(install.source, /find .* -mindepth 1 -maxdepth 1/);
  assert.match(install.source, /test ! -L/);
  assert.match(install.source, /npm install -g "\$expected_artifact"/);
  assert.match(install.source, /test "\$\(a11y-check-web --version\)" = "0\.3\.1"/);
});

test('release download requests the exact v0.3.1 asset name without wildcards', () => {
  const steps = sourceAccessibilitySteps(mainWorkflow);
  const download = steps.find((step) => step.name === 'Download a11y-check-web v0.3.1');

  assert.ok(download, 'download step is missing');
  assert.match(download.source, /--pattern 'a11y-check-web-mcp-server-0\.3\.1\.tgz'/);
  assert.doesNotMatch(download.source, /--pattern '[^']*\*/);
});
