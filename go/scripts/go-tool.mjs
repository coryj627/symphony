import {spawnSync} from 'node:child_process';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

const thisFile = fileURLToPath(import.meta.url);

function defaultProbe(command, args) {
  const result = spawnSync(command, args, {stdio: 'ignore'});
  return result.error === undefined && result.status === 0;
}

export function selectGoCommand({probe = defaultProbe} = {}) {
  if (probe('go', ['version'])) return {command: 'go', prefix: []};
  if (probe('mise', ['exec', '--', 'go', 'version'])) {
    return {command: 'mise', prefix: ['exec', '--', 'go']};
  }
  return null;
}

export function runGo(args, {
  selection = selectGoCommand(),
  spawn = spawnSync,
  cwd = process.cwd(),
  env = process.env,
  error = console.error,
} = {}) {
  if (selection === null) {
    error('Go 1.26.5 is unavailable through both PATH and the repository mise toolchain.');
    return 2;
  }
  const result = spawn(selection.command, [...selection.prefix, ...args], {
    cwd,
    env,
    stdio: 'inherit',
  });
  if (result.error) {
    error(`Go command could not start: ${result.error.message}`);
    return 2;
  }
  return result.status ?? 2;
}

if (process.argv[1] && path.resolve(process.argv[1]) === thisFile) {
  process.exitCode = runGo(process.argv.slice(2));
}
