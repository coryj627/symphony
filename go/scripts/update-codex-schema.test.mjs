import assert from 'node:assert/strict';
import {mkdir, mkdtemp, readFile, readdir, rm, writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import test from 'node:test';

import {
  TARGET_VERSION,
  aggregateSchemaDigest,
  normalizeGeneratedSchema,
  normalizeFinalNewline,
  updateCodexSchema,
} from './update-codex-schema.mjs';

test('normalizes only final newlines', () => {
  assert.deepEqual(normalizeFinalNewline(Buffer.from('{\r\n  "x": true\r\n}\r\n\r\n')), Buffer.from('{\r\n  "x": true\r\n}\n'));
  assert.deepEqual(normalizeFinalNewline(Buffer.from('{}')), Buffer.from('{}\n'));
});

test('aggregate digest is deterministic across input order and final newline style', () => {
  const first = new Map([
    ['z.json', Buffer.from('{"z":true}\r\n')],
    ['a/b.json', Buffer.from('{"b":true}')],
  ]);
  const second = new Map([
    ['a/b.json', Buffer.from('{"b":true}\n')],
    ['z.json', Buffer.from('{"z":true}\n')],
  ]);
  assert.equal(aggregateSchemaDigest(first), aggregateSchemaDigest(second));
  assert.match(aggregateSchemaDigest(first), /^sha256:[a-f0-9]{64}$/);
});

test('stabilizes the upstream v2 combined-schema definition order without rewriting values', () => {
  const first = Buffer.from(`{
  "definitions": {
    "Zulu": {"minimum": 0.0},
    "Alpha": {"type": "string"}
  }
}\n`);
  const second = Buffer.from(`{
  "definitions": {
    "Alpha": {"type": "string"},
    "Zulu": {"minimum": 0.0}
  }
}\n`);

  const normalized = normalizeGeneratedSchema('codex_app_server_protocol.v2.schemas.json', first);
  assert.deepEqual(normalized, normalizeGeneratedSchema('codex_app_server_protocol.v2.schemas.json', second));
  assert.match(normalized.toString('utf8'), /"minimum": 0\.0/);
});

test('generates the exact target atomically with a manifest', async (t) => {
  const fixture = await createFixture(t);
  await mkdir(fixture.targetDirectory, {recursive: true});
  await writeFile(path.join(fixture.targetDirectory, 'old.txt'), 'old');

  const result = await updateCodexSchema({
    schemaRoot: fixture.schemaRoot,
    command: fixture.command,
    env: {...process.env, FAKE_CODEX_VERSION: `codex-cli ${TARGET_VERSION}`},
  });

  assert.equal(result.manifest.target_version, TARGET_VERSION);
  assert.deepEqual(result.manifest.files, [
    'codex_app_server_protocol.schemas.json',
    'v2/ThreadStartParams.json',
  ]);
  assert.equal(result.manifest.schema_sha256, aggregateSchemaDigest(new Map([
    ['codex_app_server_protocol.schemas.json', Buffer.from('{"initialize":true}\n')],
    ['v2/ThreadStartParams.json', Buffer.from('{"thread":true}\n')],
  ])));
  assert.deepEqual(result.manifest.compatible, [{
    version: TARGET_VERSION,
    schema_sha256: result.manifest.schema_sha256,
  }]);
  assert.equal(result.manifest.generation_command, 'codex app-server generate-json-schema --out <dir> --experimental');
  assert.equal(await readFile(path.join(fixture.targetDirectory, 'codex_app_server_protocol.schemas.json'), 'utf8'), '{"initialize":true}\n');
  await assert.rejects(readFile(path.join(fixture.targetDirectory, 'old.txt')), {code: 'ENOENT'});

  const entries = await readdir(fixture.schemaRoot);
  assert.deepEqual(entries, [TARGET_VERSION]);
});

test('rejects a mismatched CLI version without changing the prior directory', async (t) => {
  const fixture = await createFixture(t);
  await mkdir(fixture.targetDirectory, {recursive: true});
  await writeFile(path.join(fixture.targetDirectory, 'sentinel.bin'), Buffer.from([0, 1, 2, 3]));
  const before = await snapshotDirectory(fixture.targetDirectory);

  await assert.rejects(updateCodexSchema({
    schemaRoot: fixture.schemaRoot,
    command: fixture.command,
    env: {...process.env, FAKE_CODEX_VERSION: 'codex-cli 0.145.0'},
  }), /expected codex-cli 0\.144\.1/);

  assert.deepEqual(await snapshotDirectory(fixture.targetDirectory), before);
});

test('rejects extra version output instead of accepting an approximate match', async (t) => {
  const fixture = await createFixture(t);
  await assert.rejects(updateCodexSchema({
    schemaRoot: fixture.schemaRoot,
    command: fixture.command,
    env: {...process.env, FAKE_CODEX_VERSION: `codex-cli ${TARGET_VERSION}\n\n`},
  }), /expected codex-cli 0\.144\.1/);
  assert.deepEqual(await readdir(fixture.schemaRoot), []);
});

test('failed generation leaves the prior directory byte-for-byte intact', async (t) => {
  const fixture = await createFixture(t);
  await mkdir(fixture.targetDirectory, {recursive: true});
  await mkdir(path.join(fixture.targetDirectory, 'nested'));
  await writeFile(path.join(fixture.targetDirectory, 'nested', 'sentinel.bin'), Buffer.from([255, 0, 10, 13]));
  const before = await snapshotDirectory(fixture.targetDirectory);

  await assert.rejects(updateCodexSchema({
    schemaRoot: fixture.schemaRoot,
    command: fixture.command,
    env: {...process.env, FAKE_CODEX_VERSION: `codex-cli ${TARGET_VERSION}`, FAKE_CODEX_FAIL: '1'},
  }), /schema generation failed/);

  assert.deepEqual(await snapshotDirectory(fixture.targetDirectory), before);
  assert.deepEqual(await readdir(fixture.schemaRoot), [TARGET_VERSION]);
});

async function createFixture(t) {
  const root = await mkdtemp(path.join(tmpdir(), 'symphony-codex-schema-'));
  t.after(() => rm(root, {recursive: true, force: true}));
  const fake = path.join(root, 'fake-codex.mjs');
  const source = `#!/usr/bin/env node
import {mkdir, writeFile} from 'node:fs/promises';
import path from 'node:path';
const args = process.argv.slice(2);
if (args.length === 1 && args[0] === '--version') {
  process.stdout.write(process.env.FAKE_CODEX_VERSION ?? '');
  process.exit(0);
}
if (args[0] === 'app-server' && args[1] === 'generate-json-schema') {
  const out = args[args.indexOf('--out') + 1];
  await mkdir(path.join(out, 'v2'), {recursive: true});
  if (process.env.FAKE_CODEX_FAIL === '1') {
    await writeFile(path.join(out, 'partial.json'), '{"partial":true}');
    process.exit(23);
  }
  await writeFile(path.join(out, 'codex_app_server_protocol.schemas.json'), '{"initialize":true}\\r\\n\\r\\n');
  await writeFile(path.join(out, 'v2', 'ThreadStartParams.json'), '{"thread":true}');
  process.exit(0);
}
process.exit(24);
`;
  await writeFile(fake, source);
  const schemaRoot = path.join(root, 'schema', 'codex');
  return {
    schemaRoot,
    targetDirectory: path.join(schemaRoot, TARGET_VERSION),
    command: {file: process.execPath, prefixArgs: [fake]},
  };
}

async function snapshotDirectory(directory) {
  const entries = [];
  async function walk(current, relative) {
    for (const entry of await readdir(current, {withFileTypes: true})) {
      const nextRelative = relative ? `${relative}/${entry.name}` : entry.name;
      const absolute = path.join(current, entry.name);
      if (entry.isDirectory()) await walk(absolute, nextRelative);
      else entries.push([nextRelative, (await readFile(absolute)).toString('hex')]);
    }
  }
  await walk(directory, '');
  return entries.sort(([a], [b]) => Buffer.from(a).compare(Buffer.from(b)));
}
