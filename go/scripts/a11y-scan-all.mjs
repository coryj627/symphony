import {spawnSync} from 'node:child_process';
import {randomUUID} from 'node:crypto';
import * as realFS from 'node:fs';
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

function missing(error) {
  return error?.code === 'ENOENT';
}

function snapshotBaseline(fs, baselinePath) {
  let stat;
  try {
    stat = fs.lstatSync(baselinePath);
  } catch (statError) {
    if (missing(statError)) return {kind: 'missing'};
    throw statError;
  }
  if (stat.isSymbolicLink() || !stat.isFile()) {
    throw new Error('baseline is not a regular file');
  }
  return {
    kind: 'file',
    bytes: fs.readFileSync(baselinePath),
    mode: stat.mode & 0o777,
  };
}

function inspectBaseline(fs, baselinePath) {
  let stat;
  try {
    stat = fs.lstatSync(baselinePath);
  } catch (statError) {
    if (missing(statError)) return {kind: 'missing'};
    throw statError;
  }
  if (stat.isSymbolicLink() || !stat.isFile()) return {kind: 'unsafe'};
  return {kind: 'file', bytes: fs.readFileSync(baselinePath)};
}

function sameBaseline(snapshot, current) {
  if (snapshot.kind !== current.kind) return false;
  return snapshot.kind === 'missing' || snapshot.bytes.equals(current.bytes);
}

function restoreBaseline(fs, baselinePath, snapshot) {
  if (snapshot.kind === 'missing') {
    let stat;
    try {
      stat = fs.lstatSync(baselinePath);
    } catch (statError) {
      if (missing(statError)) return;
      throw statError;
    }
    if (stat.isDirectory()) throw new Error('refusing to remove baseline directory');
    fs.rmSync(baselinePath, {force: true});
    return;
  }

  const restorePath = path.join(
    path.dirname(baselinePath),
    `.${path.basename(baselinePath)}.restore-${randomUUID()}.tmp`,
  );
  try {
    fs.writeFileSync(restorePath, snapshot.bytes, {flag: 'wx', mode: snapshot.mode});
    fs.renameSync(restorePath, baselinePath);
  } catch (restoreError) {
    try {
      fs.rmSync(restorePath, {force: true});
    } catch {
      // The primary restoration error is more actionable than temporary-file cleanup failure.
    }
    throw restoreError;
  }
}

export function run({
  repoRoot = defaultRepoRoot,
  exec = scannerExec,
  fs = realFS,
  error = console.error,
} = {}) {
  const goRoot = path.resolve(repoRoot);
  const baselinePath = path.join(goRoot, '.a11y', 'web', 'baseline.json');
  let snapshot;
  try {
    snapshot = snapshotBaseline(fs, baselinePath);
  } catch (snapshotError) {
    error(`accessibility scan baseline snapshot failed: ${snapshotError.message}`);
    return 2;
  }

  let code;
  try {
    code = exec(
      'a11y-check-web',
      ['scan', '--repo-root', goRoot, '--no-update-baseline', '--format', 'text'],
      {
        cwd: goRoot,
        env: {...process.env, A11Y_ALLOWED_ROOTS: goRoot},
        error,
      },
    );
  } catch (toolError) {
    error(`accessibility scan tool failed: ${toolError.message}`);
    code = 2;
  }

  let current;
  let comparisonError;
  try {
    current = inspectBaseline(fs, baselinePath);
  } catch (readError) {
    comparisonError = readError;
    error(`accessibility scan baseline comparison failed: ${readError.message}`);
  }
  if (comparisonError === undefined && sameBaseline(snapshot, current)) return code;

  try {
    restoreBaseline(fs, baselinePath, snapshot);
  } catch (restoreError) {
    error(`accessibility scan baseline restoration failed: ${restoreError.message}`);
    return 2;
  }
  error(
    comparisonError === undefined
      ? 'accessibility scan baseline changed despite --no-update-baseline; restored the reviewed baseline and failed closed'
      : 'accessibility scan baseline could not be compared; restored the reviewed baseline and failed closed',
  );
  return 2;
}

if (process.argv[1] && path.resolve(process.argv[1]) === thisFile) {
  process.exitCode = run();
}
