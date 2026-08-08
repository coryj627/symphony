import {spawnSync} from 'node:child_process';
import {existsSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
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

export function run({repoRoot = defaultRepoRoot, exec = scannerExec, error = console.error} = {}) {
  const goRoot = path.resolve(repoRoot);
  const baselinePath = path.join(goRoot, '.a11y', 'web', 'baseline.json');
  const baselineExisted = existsSync(baselinePath);
  const reviewedBaseline = baselineExisted ? readFileSync(baselinePath) : null;

  const code = exec(
    'a11y-check-web',
    ['scan', '--repo-root', goRoot, '--no-update-baseline', '--format', 'text'],
    {
      cwd: goRoot,
      env: {...process.env, A11Y_ALLOWED_ROOTS: goRoot},
      error,
    },
  );

  const baselineExistsAfter = existsSync(baselinePath);
  const baselineChanged = baselineExisted
    ? !baselineExistsAfter || !readFileSync(baselinePath).equals(reviewedBaseline)
    : baselineExistsAfter;
  if (!baselineChanged) return code;

  try {
    if (baselineExisted) {
      writeFileSync(baselinePath, reviewedBaseline);
    } else {
      rmSync(baselinePath, {force: true});
    }
  } catch (restoreError) {
    error(`accessibility scan baseline changed and restoration failed: ${restoreError.message}`);
    return 2;
  }
  error('accessibility scan baseline changed despite --no-update-baseline; restored the reviewed baseline and failed closed');
  return 2;
}

if (process.argv[1] && path.resolve(process.argv[1]) === thisFile) {
  process.exitCode = run();
}
