import {spawnSync} from 'node:child_process';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

const thisFile = fileURLToPath(import.meta.url);
const requiredGoVersion = '1.26.5';
const requiredNodeVersion = 'v24.18.0';

function defaultProbe(command, args) {
  const result = spawnSync(command, args, {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'ignore'],
  });
  return result.error === undefined && result.status === 0 ? result.stdout : null;
}

function hasRequiredGoVersion(output) {
  if (typeof output !== 'string') return false;
  const match = output.match(/^go version go([^\s]+)(?:\s|$)/);
  return match?.[1] === requiredGoVersion;
}

export function selectGoCommand({probe = defaultProbe} = {}) {
  if (hasRequiredGoVersion(probe('go', ['version']))) {
    return {command: 'go', prefix: [], version: requiredGoVersion};
  }
  if (hasRequiredGoVersion(probe('mise', ['exec', '--', 'go', 'version']))) {
    return {
      command: 'mise',
      prefix: ['exec', '--', 'go'],
      version: requiredGoVersion,
    };
  }
  return null;
}

export function runGo(args, {
  selection,
  nodeVersion = process.version,
  spawn = spawnSync,
  cwd = process.cwd(),
  env = process.env,
  error = console.error,
} = {}) {
  if (nodeVersion !== requiredNodeVersion) {
    error(`Node 24.18.0 is required; running ${nodeVersion}.`);
    return 2;
  }
  const selected = selection ?? selectGoCommand();
  if (selected === null) {
    error('Go 1.26.5 is unavailable through both PATH and the repository mise toolchain.');
    return 2;
  }
  if (selected.version !== requiredGoVersion) {
    error(`Go 1.26.5 is required; selected ${selected.version ?? 'an unknown version'}.`);
    return 2;
  }
  const result = spawn(selected.command, [...selected.prefix, ...args], {
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
