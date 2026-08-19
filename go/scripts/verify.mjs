import {spawnSync} from 'node:child_process';
import {mkdtempSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

import {selectGoCommand} from './go-tool.mjs';

const thisFile = fileURLToPath(import.meta.url);
const defaultRepoRoot = path.resolve(path.dirname(thisFile), '..');
const requiredNodeVersion = 'v24.18.0';
const requiredGoVersion = '1.26.5';

function commandExec(command, args, options) {
  const result = spawnSync(command, args, {...options, stdio: 'inherit'});
  if (result.error) {
    options.error(`Verification could not start ${command}: ${result.error.message}`);
    return 2;
  }
  return result.status ?? 2;
}

function commandCapture(command, args, options) {
  const result = spawnSync(command, args, {
    ...options,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const stdout = result.stdout ?? '';
  const stderr = result.stderr ?? '';
  if (stdout !== '') process.stdout.write(stdout);
  if (stderr !== '') process.stderr.write(stderr);
  if (result.error) {
    options.error(`Verification could not start ${command}: ${result.error.message}`);
    return {status: 2, stdout, stderr};
  }
  return {status: result.status ?? 2, stdout, stderr};
}

const liveEnvironmentNames = [
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
];

function deterministicEnvironment(source) {
  const environment = {...source};
  const liveNames = new Set(liveEnvironmentNames.map((name) => name.toUpperCase()));
  for (const name of Object.keys(environment)) {
    if (liveNames.has(name.toUpperCase())) delete environment[name];
  }
  return environment;
}

export function run({
  platform = process.platform,
  nodeVersion = process.version,
  repoRoot = defaultRepoRoot,
  goTool,
  exec = commandExec,
  capture = commandCapture,
  environment = process.env,
  error = console.error,
} = {}) {
  if (platform !== 'darwin' && platform !== 'win32') {
    error('Verification supports only macOS and Windows; no Linux support is claimed.');
    return 2;
  }
  if (nodeVersion !== requiredNodeVersion) {
    error(`Node 24.18.0 is required; running ${nodeVersion}.`);
    return 2;
  }

  const selectedGo = goTool ?? selectGoCommand();
  if (selectedGo === null) {
    error('Go 1.26.5 is unavailable through both PATH and the repository mise toolchain.');
    return 2;
  }
  if (selectedGo.version !== requiredGoVersion) {
    error(`Go 1.26.5 is required; selected ${selectedGo.version ?? 'an unknown version'}.`);
    return 2;
  }

  let buildDirectory;
  try {
    buildDirectory = mkdtempSync(path.join(tmpdir(), 'symphony-verify-'));
  } catch (filesystemError) {
    error(`Verification could not create its temporary build directory: ${filesystemError.message}`);
    return 2;
  }
  const buildOutput = path.join(buildDirectory, platform === 'win32' ? 'symphony.exe' : 'symphony');

  const npm = platform === 'win32' ? 'npm.cmd' : 'npm';
  const commands = [
    {command: process.execPath, args: ['--test', 'scripts/a11y-precommit.test.mjs', 'scripts/a11y-scan-all.test.mjs', 'scripts/check-local-assets.test.mjs', 'scripts/check-upstream-trace.test.mjs', 'scripts/ci-structure.test.mjs', 'scripts/git-attributes.test.mjs', 'scripts/go-tool.test.mjs', 'scripts/verify.test.mjs']},
    {command: selectedGo.command, args: [...selectedGo.prefix, 'build', '-o', buildOutput, './cmd/symphony']},
    {command: selectedGo.command, args: [...selectedGo.prefix, 'test', './...']},
    ...(platform === 'darwin'
      ? [{command: selectedGo.command, args: [...selectedGo.prefix, 'test', '-race', './...']}]
      : []),
    {command: selectedGo.command, args: [...selectedGo.prefix, 'vet', './...']},
    {
      command: selectedGo.command,
      args: [...selectedGo.prefix, 'test', '-v', '-tags=integration_live', '-count=1', '-timeout=2m', './internal/tracker/github'],
      sentinel: 'SKIPPED: GitHub live profile not enabled',
      profile: 'GitHub',
    },
    {
      command: selectedGo.command,
      args: [...selectedGo.prefix, 'test', '-v', '-tags=integration_live', '-count=1', '-timeout=2m', './internal/tracker/linear'],
      sentinel: 'SKIPPED: Linear live profile not enabled',
      profile: 'Linear',
    },
    {command: npm, args: ['ci']},
    {command: npm, args: ['run', 'security:assets']},
    {command: npm, args: ['run', 'conformance:upstream']},
    {command: npm, args: ['run', 'html:validate']},
    {command: npm, args: ['run', 'test:a11y']},
    {command: process.execPath, args: ['scripts/a11y-scan-all.mjs']},
  ];

  const options = {cwd: path.resolve(repoRoot), env: deterministicEnvironment(environment), error};
  try {
    for (const gate of commands) {
      if (gate.sentinel !== undefined) {
        const result = capture(gate.command, gate.args, options) ?? {};
        const code = Number.isInteger(result.status) ? result.status : 2;
        if (code !== 0) return code;
        const output = `${result.stdout ?? ''}${result.stderr ?? ''}`;
        if (!output.includes(gate.sentinel)) {
          error(`${gate.profile} disabled profile did not report ${gate.sentinel}.`);
          return 2;
        }
        continue;
      }
      const code = exec(gate.command, gate.args, options);
      if (code !== 0) return code;
    }
    return 0;
  } finally {
    rmSync(buildDirectory, {recursive: true, force: true});
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === thisFile) {
  process.exitCode = run();
}
