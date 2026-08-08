import {spawnSync} from 'node:child_process';
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
    options.error(`Phase 1 verification could not start ${command}: ${result.error.message}`);
    return 2;
  }
  return result.status ?? 2;
}

export function run({
  platform = process.platform,
  nodeVersion = process.version,
  repoRoot = defaultRepoRoot,
  goTool,
  exec = commandExec,
  error = console.error,
} = {}) {
  if (platform !== 'darwin' && platform !== 'win32') {
    error('Phase 1 verification supports only macOS and Windows; no Linux support is claimed.');
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

  const npm = platform === 'win32' ? 'npm.cmd' : 'npm';
  const commands = [
    [process.execPath, ['--test', 'scripts/a11y-precommit.test.mjs', 'scripts/a11y-scan-all.test.mjs', 'scripts/ci-structure.test.mjs', 'scripts/go-tool.test.mjs', 'scripts/verify.test.mjs']],
    [selectedGo.command, [...selectedGo.prefix, 'test', './...']],
    ...(platform === 'darwin'
      ? [[selectedGo.command, [...selectedGo.prefix, 'test', '-race', './...']]]
      : []),
    [selectedGo.command, [...selectedGo.prefix, 'vet', './...']],
    [npm, ['ci']],
    [npm, ['run', 'html:validate']],
    [npm, ['run', 'test:a11y']],
    [process.execPath, ['scripts/a11y-scan-all.mjs']],
  ];

  for (const [command, args] of commands) {
    const code = exec(command, args, {cwd: path.resolve(repoRoot), env: process.env, error});
    if (code !== 0) return code;
  }
  return 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === thisFile) {
  process.exitCode = run();
}
