import {spawnSync} from 'node:child_process';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

const thisFile = fileURLToPath(import.meta.url);
const defaultRepoRoot = path.resolve(path.dirname(thisFile), '..');

function scannerExec(command, args, options) {
  const result = spawnSync(command, args, {...options, stdio: 'inherit'});
  if (result.error) {
    options.error(`accessibility scan could not start: ${result.error.message}`);
    return 2;
  }
  return result.status ?? 2;
}

function gitPaths(args, repoRoot, error) {
  const result = spawnSync('git', args, {
    cwd: repoRoot,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  if (result.error || result.status !== 0) {
    const detail = result.error?.message ?? result.stderr.trim() ?? `exit ${result.status}`;
    error(`accessibility pre-commit could not inspect Git paths: ${detail}`);
    return null;
  }
  return result.stdout.split('\0').filter(Boolean);
}

function isApplicable(filePath) {
  return filePath.startsWith('go/web/') || filePath.startsWith('go/internal/web/');
}

function applicablePaths(paths) {
  return paths.filter((filePath) => isApplicable(filePath) || isApplicable(filePath.trim()));
}

function isUnsafeScannerPath(filePath) {
  return filePath.includes(',') || /\s/.test(filePath);
}

export function run({
  repoRoot = defaultRepoRoot,
  staged,
  unstaged,
  exec = scannerExec,
  error = console.error,
} = {}) {
  const goRoot = path.resolve(repoRoot);
  const stagedPaths = staged ?? gitPaths(
    ['diff', '--cached', '--name-only', '--diff-filter=ACMR', '-z'],
    goRoot,
    error,
  );
  if (stagedPaths === null) return 2;

  const applicable = applicablePaths(stagedPaths);
  if (applicable.length === 0) return 0;

  const unsafe = applicable.find(isUnsafeScannerPath);
  if (unsafe !== undefined) {
    error(`accessibility pre-commit cannot safely scan staged path ${JSON.stringify(unsafe)}: commas and whitespace are unsupported`);
    return 2;
  }

  const unstagedPaths = unstaged ?? gitPaths(['diff', '--name-only', '-z'], goRoot, error);
  if (unstagedPaths === null) return 2;
  const unstagedSet = new Set(unstagedPaths);
  const partiallyStaged = applicable.filter((filePath) => unstagedSet.has(filePath));
  if (partiallyStaged.length > 0) {
    error(
      `accessibility pre-commit cannot scan partially staged file(s): ${partiallyStaged.join(', ')}. ` +
        'Stage the full file or move its unstaged edit before committing.',
    );
    return 2;
  }

  const args = ['scan', '--repo-root', goRoot];
  for (const filePath of applicable) {
    args.push('--changed-files', filePath.slice('go/'.length));
  }
  args.push('--no-update-baseline', '--format', 'text');

  return exec('a11y-check-web', args, {
    cwd: goRoot,
    env: {...process.env, A11Y_ALLOWED_ROOTS: goRoot},
    error,
  });
}

if (process.argv[1] && path.resolve(process.argv[1]) === thisFile) {
  process.exitCode = run();
}
