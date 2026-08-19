import {spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import * as realFS from 'node:fs';
import path from 'node:path';
import {TextDecoder} from 'node:util';
import {fileURLToPath} from 'node:url';

const thisFile = fileURLToPath(import.meta.url);
const defaultRepoRoot = path.resolve(path.dirname(thisFile), '..', '..');
const maximumTextBytes = 2 * 1024 * 1024;
const utf8 = new TextDecoder('utf-8', {fatal: true});

const policies = [
  {
    name: 'private-key-block',
    expressions: [
      /-----BEGIN (?:[A-Z0-9][A-Z0-9 -]* )?PRIVATE KEY(?: BLOCK)?-----/g,
    ],
  },
  {
    name: 'live-token-prefix',
    expressions: [
      /\b(?:gh[pousr]_[A-Za-z0-9_]{20,255}|github_pat_[A-Za-z0-9_]{20,255})\b/g,
      /\blin_api_[A-Za-z0-9_-]{16,255}\b/g,
      /\bxox[baprs]-[A-Za-z0-9-]{16,255}\b/g,
      /\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,255}\b/g,
      /\b(?:sk|rk)_live_[A-Za-z0-9]{16,255}\b/g,
      /\bglpat-[A-Za-z0-9_-]{16,255}\b/g,
      /\b(?:AKIA|ASIA)[A-Z0-9]{16}\b/g,
      /\bAIza[0-9A-Za-z_-]{35}\b/g,
      /\bnpm_[A-Za-z0-9]{36}\b/g,
      /\bhf_[A-Za-z0-9]{20,255}\b/g,
    ],
  },
  {
    name: 'authorization-value',
    expressions: [
      /\bAuthorization\s*:\s*(?:Bearer|Basic)\s+[A-Za-z0-9+/_=.-]{8,255}/gi,
      /["']Authorization["']\s*[:=,]\s*["'](?:Bearer|Basic)\s+[A-Za-z0-9+/_=.-]{8,255}["']/gi,
    ],
  },
  {
    name: 'credential-literal',
    expressions: [
      /(?:\b|["'`])(?:api[_-]?key|api[_-]?token|access[_-]?key|client[_-]?secret|password|secret|token)(?:\b|["'`])\s*(?::=|=>|=|:)\s*["'`][A-Za-z0-9+/_=.,:@-]{12,255}["'`]/gi,
      /\b(?:API_KEY|API_TOKEN|ACCESS_KEY|CLIENT_SECRET|PASSWORD|SECRET|TOKEN)\s*=\s*[A-Za-z0-9+/_=.,:@-]{12,255}(?=\s|$)/gm,
    ],
  },
];

const policyNames = new Set(policies.map(({name}) => name));

// Approved matches must be intentional test fixtures. Each entry is bound to
// one repository path, one line, one policy, and a SHA-256 of every complete
// source line touched by the match. Changing any part of an approved fixture
// invalidates the entry.
// The gate rejects stale entries and never prints a match or its fingerprint.
const approvedFixtures = [
  {sourcePath: 'elixir/test/symphony_elixir/workspace_and_config_test.exs', line: 1173, policy: 'credential-literal', fingerprint: 'fb4b39e7daf76af273a097463c6a73a0098722d8d1b20385ed233ba88ff93736'},
  {sourcePath: 'elixir/test/symphony_elixir/workspace_and_config_test.exs', line: 1208, policy: 'credential-literal', fingerprint: 'ff6a2607494341c33a822f88b22f3cdbd5d83eb1ae797cb2816af63fb6630160'},
  {sourcePath: 'elixir/test/symphony_elixir/workspace_and_config_test.exs', line: 1222, policy: 'credential-literal', fingerprint: '0ae2b1bf853eefa9cdfe19a1e9257a9e257eea9b47e15d29cd9ee74b1bd8dccc'},
  {sourcePath: 'elixir/test/symphony_elixir/workspace_and_config_test.exs', line: 1278, policy: 'credential-literal', fingerprint: 'fb4b39e7daf76af273a097463c6a73a0098722d8d1b20385ed233ba88ff93736'},
  {sourcePath: 'go/internal/codex/fakeappserver/scenario_test.go', line: 119, policy: 'credential-literal', fingerprint: '5242fd0bb6565f56d9c72cb24b3b835ed283242e630171a734464dde8b3ee1cd'},
  {sourcePath: 'go/internal/codex/fakeappserver/scenario_test.go', line: 164, policy: 'credential-literal', fingerprint: 'c0ef2c0cdde29df9459df88c45aa4eb51e1af76e5c7d074269ea44987cbcf6db'},
  {sourcePath: 'go/internal/codex/request_broker_test.go', line: 17, policy: 'credential-literal', fingerprint: 'f69edfeb73d87204e6d7897269f4058795d964d10127b12103862a0d16e1b955'},
  {sourcePath: 'go/internal/codex/request_broker_test.go', line: 171, policy: 'credential-literal', fingerprint: '37722066c39c6865289e240d498ff858228994eb6f4a267d9f92847b3d86a812'},
  {sourcePath: 'go/internal/codex/stderr_test.go', line: 13, policy: 'credential-literal', fingerprint: 'd0911be63e46456d48d74782165f827620eb66fe891bcd5bd183e5ac8fe4407b'},
  {sourcePath: 'go/internal/codex/stderr_test.go', line: 13, policy: 'live-token-prefix', fingerprint: 'd0911be63e46456d48d74782165f827620eb66fe891bcd5bd183e5ac8fe4407b'},
  {sourcePath: 'go/internal/codex/stderr_test.go', line: 31, policy: 'credential-literal', fingerprint: 'f4dbc1f295ad27684dcb0f48c530d1bf37afe3380d99140d26c2f306cfb52767'},
  {sourcePath: 'go/internal/codex/stderr_test.go', line: 31, policy: 'live-token-prefix', fingerprint: 'f4dbc1f295ad27684dcb0f48c530d1bf37afe3380d99140d26c2f306cfb52767'},
  {sourcePath: 'go/internal/codex/stderr_test.go', line: 46, policy: 'credential-literal', fingerprint: 'fcdc7bcb8c893b21987f582be24e5b04beb7bfc640115b1a6cdb258047b92d49'},
  {sourcePath: 'go/internal/codex/stderr_test.go', line: 46, policy: 'live-token-prefix', fingerprint: 'fcdc7bcb8c893b21987f582be24e5b04beb7bfc640115b1a6cdb258047b92d49'},
  {sourcePath: 'go/internal/codex/wire_test.go', line: 17, policy: 'credential-literal', fingerprint: '8cd72093dbc50abb1f9ef57132466a8e7c3c3a201332338922dd7c518e4aacc7'},
  {sourcePath: 'go/internal/codex/wire_test.go', line: 66, policy: 'credential-literal', fingerprint: 'd76d033bcc8207f16f4418f7cb76f6bd374678743a50d2b66f79372cac640cf9'},
  {sourcePath: 'go/internal/observability/redactor_test.go', line: 57, policy: 'live-token-prefix', fingerprint: '3e712216212e540b90c62145851e67040a8412e7704e84384193a0c84b4885e5'},
  {sourcePath: 'go/internal/observability/redactor_test.go', line: 100, policy: 'live-token-prefix', fingerprint: 'b0eb9e24f0ee0f55e544dfb6b90190687b971049f4d45e395258bc5bc165b500'},
  {sourcePath: 'go/internal/observability/redactor_test.go', line: 101, policy: 'live-token-prefix', fingerprint: 'fe868c83c944f68bad42cce5172f2547fedbe0669a98a4631c08792b8ecc7a4f'},
  {sourcePath: 'go/internal/observability/redactor_test.go', line: 102, policy: 'live-token-prefix', fingerprint: '1c3ba800e8a17f5d92f45d92087cf4ce71aaef409d39e88ed9b12c3f8d29668e'},
  {sourcePath: 'go/internal/observability/redactor_test.go', line: 103, policy: 'live-token-prefix', fingerprint: 'bbfae55846de5356113af2f32e5bbc49db65545bc10a202b33d2eb03b40a4ffd'},
  {sourcePath: 'go/internal/web/api_test.go', line: 280, policy: 'credential-literal', fingerprint: 'd73b12bfa1277bb6744a5a08516a7ad2c8ff4549310c86923027b643e7c82348'},
  {sourcePath: 'go/internal/web/api_test.go', line: 478, policy: 'credential-literal', fingerprint: '1f3b2b581321b6f0d9c48e84aa4ef56f1e24de2864160f26def7e0f0a0d63ac7'},
  {sourcePath: 'go/internal/web/csrf_test.go', line: 146, policy: 'credential-literal', fingerprint: '83eb30eb4f4fa17235c40e5c6f836dcfb77816d57f6e4e1b8a5d8b0a28d3b2a5'},
  {sourcePath: 'go/internal/web/queue_handlers_test.go', line: 287, policy: 'credential-literal', fingerprint: '0f7814f1089dc73dc323289a14f72012db7e5fc2ed0821f70a9eec63cdfb75e0'},
  {sourcePath: 'go/internal/web/server_test.go', line: 862, policy: 'credential-literal', fingerprint: '16425222dbb4ba04fa2fc5e5a110903b2f3354a5ebe1617923359d409a83265c'},
];

function defaultGit(args, options) {
  return spawnSync('git', args, {
    ...options,
    encoding: null,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
}

function fixturePath(sourcePath) {
  const segments = sourcePath.split('/');
  const basename = segments.at(-1) ?? '';
  return segments.some((segment) => ['test', 'tests', 'testdata', 'fixture', 'fixtures'].includes(segment.toLowerCase()))
    || /(?:^|[._-])test\.[^.]+$/i.test(basename)
    || /(?:^|[._-])spec\.[^.]+$/i.test(basename);
}

function normalizedTrackedPath(sourcePath) {
  if (typeof sourcePath !== 'string' || sourcePath === '' || sourcePath.includes('\\') || /[\u0000-\u001f\u007f\u2028-\u202e\u2066-\u2069]/.test(sourcePath)) {
    throw new Error('git reported an invalid tracked path');
  }
  if (path.posix.isAbsolute(sourcePath)
      || /^[A-Za-z]:/.test(sourcePath)
      || sourcePath.split('/').some((part) => part === '' || part === '.' || part === '..')
      || path.posix.normalize(sourcePath) !== sourcePath) {
    throw new Error('git reported an unsafe tracked path');
  }
  return sourcePath;
}

function decode(value, subject) {
  try {
    return utf8.decode(value);
  } catch {
    throw new Error(`${subject} is not valid UTF-8`);
  }
}

function trackedInventory(repoRoot, git) {
  const result = git(['ls-files', '--eol', '-z'], {cwd: repoRoot});
  if (result?.error) throw new Error(`git ls-files --eol could not start: ${result.error.message}`);
  if (!result || result.status !== 0) {
    const status = Number.isInteger(result?.status) ? result.status : 'unknown';
    throw new Error(`git ls-files --eol failed with exit ${status}`);
  }
  if (!Buffer.isBuffer(result.stdout)) throw new Error('git ls-files --eol returned an invalid inventory');

  const records = result.stdout.length === 0
    ? []
    : result.stdout.subarray(0, result.stdout.at(-1) === 0 ? -1 : undefined).toString('binary').split('\0')
      .map((record) => Buffer.from(record, 'binary'));
  const inventory = [];
  const seen = new Set();
  for (const record of records) {
    const tab = record.indexOf(0x09);
    if (tab <= 0 || tab === record.length - 1) throw new Error('git reported a malformed tracked entry');
    const eol = decode(record.subarray(0, tab), 'git metadata');
    const sourcePath = normalizedTrackedPath(decode(record.subarray(tab + 1), 'a tracked path'));
    if (seen.has(sourcePath)) throw new Error('git reported a duplicate tracked path');
    seen.add(sourcePath);
    inventory.push({sourcePath, binary: /(?:^|\s)i\/-text(?:\s|$)/.test(eol)});
  }
  return inventory;
}

function sourceLine(source, index) {
  let line = 1;
  for (let cursor = 0; cursor < index; cursor += 1) {
    if (source[cursor] === '\r') {
      line += 1;
      if (source[cursor + 1] === '\n') cursor += 1;
    } else if (source[cursor] === '\n' || source[cursor] === '\u2028' || source[cursor] === '\u2029') {
      line += 1;
    }
  }
  return line;
}

function sourceSpanValue(source, index, length) {
  let start = index;
  while (start > 0 && !['\r', '\n', '\u2028', '\u2029'].includes(source[start - 1])) start -= 1;
  let end = index + length;
  while (end < source.length && !['\r', '\n', '\u2028', '\u2029'].includes(source[end])) end += 1;
  return source.slice(start, end);
}

export function secretFingerprint(value) {
  return createHash('sha256').update(value, 'utf8').digest('hex');
}

function allowlistMap(entries) {
  if (!Array.isArray(entries)) throw new Error('the fixture allowlist is not an array');
  const allowed = new Map();
  for (const entry of entries) {
    const sourcePath = normalizedTrackedPath(entry?.sourcePath);
    if (!fixturePath(sourcePath)) throw new Error('the fixture allowlist contains a non-fixture path');
    if (!Number.isSafeInteger(entry?.line) || entry.line < 1) throw new Error('the fixture allowlist contains an invalid line');
    if (!policyNames.has(entry?.policy)) throw new Error('the fixture allowlist contains an invalid policy');
    if (!/^[0-9a-f]{64}$/.test(entry?.fingerprint ?? '')) throw new Error('the fixture allowlist contains an invalid fingerprint');
    const key = `${sourcePath}:${entry.line}:${entry.policy}:${entry.fingerprint}`;
    if (allowed.has(key)) throw new Error('the fixture allowlist contains a duplicate entry');
    allowed.set(key, entry);
  }
  return allowed;
}

function scanSource(source, sourcePath, allowed, observed, addFinding) {
  for (const {name: policy, expressions} of policies) {
    for (const expression of expressions) {
      expression.lastIndex = 0;
      for (const match of source.matchAll(expression)) {
        const line = sourceLine(source, match.index);
        const fingerprint = secretFingerprint(sourceSpanValue(source, match.index, match[0].length));
        const key = `${sourcePath}:${line}:${policy}:${fingerprint}`;
        if (allowed.has(key)) observed.add(key);
        else addFinding(sourcePath, line, policy);
      }
    }
  }
}

function sameTrackedFile(before, after) {
  return before.dev === after.dev
    && before.ino === after.ino
    && before.size === after.size
    && before.mtimeMs === after.mtimeMs
    && before.ctimeMs === after.ctimeMs;
}

function readTrackedText(fs, absolutePath, sourcePath, initialMetadata) {
  let descriptor;
  try {
    descriptor = fs.openSync(absolutePath, 'r');
    const opened = fs.fstatSync(descriptor);
    if (!opened.isFile() || !sameTrackedFile(initialMetadata, opened)) {
      throw new Error(`tracked text file ${sourcePath} changed before scanning`);
    }
    if (opened.size > maximumTextBytes) {
      throw new Error(`tracked text file ${sourcePath} exceeds the scan limit`);
    }
    const bytes = fs.readFileSync(descriptor);
    const completed = fs.fstatSync(descriptor);
    if (!Buffer.isBuffer(bytes)) throw new Error(`tracked text file ${sourcePath} returned invalid bytes`);
    if (bytes.length !== completed.size || !sameTrackedFile(opened, completed)) {
      throw new Error(`tracked text file ${sourcePath} changed while scanning`);
    }
    return bytes;
  } finally {
    if (descriptor !== undefined) fs.closeSync(descriptor);
  }
}

export function run({
  repoRoot = defaultRepoRoot,
  fs = realFS,
  git = defaultGit,
  fixtureAllowlist = approvedFixtures,
  output = console.log,
  error = console.error,
} = {}) {
  try {
    const canonicalRoot = path.resolve(repoRoot);
    const rootMetadata = fs.lstatSync(canonicalRoot);
    if (!rootMetadata.isDirectory() || rootMetadata.isSymbolicLink()) {
      throw new Error('the repository root is not a regular directory');
    }
    const allowed = allowlistMap(fixtureAllowlist);
    const observed = new Set();
    const findings = [];
    const findingKeys = new Set();
    const addFinding = (sourcePath, line, policy) => {
      const key = `${sourcePath}:${line}:${policy}`;
      if (findingKeys.has(key)) return;
      findingKeys.add(key);
      findings.push({sourcePath, line, policy});
    };
    let textFiles = 0;
    let binaryFiles = 0;

    for (const entry of trackedInventory(canonicalRoot, git)) {
      const absolutePath = path.resolve(canonicalRoot, ...entry.sourcePath.split('/'));
      if (path.relative(canonicalRoot, absolutePath).split(path.sep).includes('..')) {
        throw new Error('a tracked path escapes the repository root');
      }
      const metadata = fs.lstatSync(absolutePath);
      if (!metadata.isFile() || metadata.isSymbolicLink()) {
        throw new Error(`tracked entry ${entry.sourcePath} is not a regular file`);
      }
      if (entry.binary) {
        binaryFiles += 1;
        continue;
      }
      const bytes = readTrackedText(fs, absolutePath, entry.sourcePath, metadata);
      const source = decode(bytes, `tracked text file ${entry.sourcePath}`);
      textFiles += 1;
      scanSource(source, entry.sourcePath, allowed, observed, addFinding);
    }

    for (const [key, entry] of allowed) {
      if (!observed.has(key)) addFinding(entry.sourcePath, entry.line, 'stale-fixture-allowlist');
    }
    if (findings.length > 0) {
      output('Tracked-source secret pattern check failed:');
      for (const finding of findings.sort((left, right) => left.sourcePath.localeCompare(right.sourcePath)
        || left.line - right.line || left.policy.localeCompare(right.policy))) {
        output(`- ${finding.sourcePath}:${finding.line} [${finding.policy}]`);
      }
      return 1;
    }
    output(`Tracked-source secret pattern check passed (${textFiles} text files, ${binaryFiles} binary files, ${observed.size} approved fixture matches).`);
    return 0;
  } catch (scanError) {
    error(`Tracked-source secret pattern check could not complete: ${scanError.message}`);
    return 2;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === thisFile) {
  process.exitCode = run();
}
