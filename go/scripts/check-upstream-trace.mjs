import {spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {existsSync, readFileSync} from 'node:fs';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

import {selectGoCommand} from './go-tool.mjs';

const thisFile = fileURLToPath(import.meta.url);
const defaultGoRoot = path.resolve(path.dirname(thisFile), '..');
const governedSections = new Set([
  '17.1', '17.2', '17.3', '17.4', '17.5', '17.6', '17.7', '17.8',
  '18.1', '18.2', '18.3',
]);
const allowedStatuses = new Set([
  'pass',
  'not_implemented_optional',
  'skipped_real_profile',
]);

export function normalizeSourceText(value) {
  return value.replace(/\s+/g, ' ').trim();
}

export function sourceTextSHA256(value) {
  return createHash('sha256').update(normalizeSourceText(value), 'utf8').digest('hex');
}

function profileFor(section, text, ancestors) {
  if (section === '17.8' || section === '18.3') return 'real_integration';
  if (section === '18.2') return 'extension';
  if (text.startsWith('OPTIONAL ') || text.startsWith('If ')) return 'extension';
  if (ancestors.some((ancestor) => ancestor.startsWith('If '))) return 'extension';
  return 'core';
}

export function extractGovernedRequirements(specSource) {
  const source = specSource.replace(/\r\n?/g, '\n');
  const requirements = [];
  const ancestors = [];
  let section = '';
  let current = null;

  function finishCurrent() {
    if (current === null) return;
    const sourceText = normalizeSourceText(current.parts.join(' '));
    const parentTexts = ancestors
      .slice(0, current.depth)
      .filter((value) => typeof value === 'string');
    requirements.push({
      section: current.section,
      depth: current.depth,
      source_text: sourceText,
      source_text_sha256: sourceTextSHA256(sourceText),
      profile: profileFor(current.section, sourceText, parentTexts),
    });
    ancestors[current.depth] = sourceText;
    ancestors.length = current.depth + 1;
    current = null;
  }

  for (const line of source.split('\n')) {
    const heading = line.match(/^###\s+(17\.[1-8]|18\.[1-3])(?:\s|$)/);
    if (heading) {
      finishCurrent();
      section = heading[1];
      ancestors.length = 0;
      continue;
    }
    if (/^#{1,3}\s/.test(line)) {
      finishCurrent();
      section = '';
      ancestors.length = 0;
      continue;
    }
    if (!governedSections.has(section)) continue;

    const bullet = line.match(/^(\s*)-\s+(.*)$/);
    if (bullet) {
      finishCurrent();
      current = {
        section,
        depth: Math.floor(bullet[1].length / 2),
        parts: [bullet[2]],
      };
      continue;
    }
    if (current !== null && /^\s+\S/.test(line)) {
      current.parts.push(line.trim());
      continue;
    }
    if (line.trim() === '') finishCurrent();
  }
  finishCurrent();
  return requirements;
}

export function parseEvidence(reference) {
  if (typeof reference !== 'string') return null;
  const go = reference.match(/^go:(\.\/[A-Za-z0-9_./-]+)::(Test[A-Za-z0-9_]+)$/);
  if (go) return {kind: 'go', package: go[1], name: go[2]};
  const playwright = reference.match(/^playwright:([A-Za-z0-9_./-]+\.(?:mjs|js))::(.+)$/);
  if (playwright) return {kind: 'playwright', file: playwright[1], title: playwright[2]};
  const command = reference.match(/^command:(\S.+)$/);
  if (command) return {kind: 'command', command: command[1]};
  const report = reference.match(/^report:([^#]+)#([a-z0-9][a-z0-9-]*)$/);
  if (report) return {kind: 'report', file: report[1], anchor: report[2]};
  return null;
}

export function markdownAnchors(source) {
  const anchors = new Set();
  for (const match of source.replace(/\r\n?/g, '\n').matchAll(/^#{1,6}\s+(.+)$/gm)) {
    const anchor = match[1]
      .trim()
      .toLowerCase()
      .replace(/[`*_~]/g, '')
      .replace(/[^\p{L}\p{N}\s-]/gu, '')
      .replace(/\s+/g, '-')
      .replace(/-+/g, '-');
    anchors.add(anchor);
  }
  return anchors;
}

function evidenceExists(parsed, {goTests, playwrightTitles, goRoot, fileExists, readText}) {
  if (parsed.kind === 'go') return goTests.get(parsed.package)?.has(parsed.name) === true;
  if (parsed.kind === 'playwright') {
    return playwrightTitles.has(`${parsed.file}::${parsed.title}`);
  }
  if (parsed.kind === 'command') return parsed.command.trim() !== '';
  const absolute = path.resolve(goRoot, parsed.file);
  if (!fileExists(absolute)) return false;
  return markdownAnchors(readText(absolute)).has(parsed.anchor);
}

export function validateManifest(manifest, requirements, {
  goTests = new Map(),
  playwrightTitles = new Set(),
  goRoot = defaultGoRoot,
  fileExists = existsSync,
  readText = (file) => readFileSync(file, 'utf8'),
  validateEvidenceExistence = true,
} = {}) {
  const errors = [];
  if (manifest?.schema_version !== 1) errors.push('schema_version must be 1');
  if (manifest?.spec_path !== '../SPEC.md') errors.push('spec_path must be ../SPEC.md');
  if (!Array.isArray(manifest?.rows)) return [...errors, 'rows must be an array'];
  if (manifest.rows.length !== requirements.length) {
    errors.push(`row count ${manifest.rows.length} does not match governed requirement count ${requirements.length}`);
  }

  const seenIDs = new Set();
  for (let index = 0; index < manifest.rows.length; index += 1) {
    const row = manifest.rows[index];
    const requirement = requirements[index];
    const label = row?.id ?? `row ${index + 1}`;
    if (!/^S(?:17\.[1-8]|18\.[1-3])-[a-z0-9]+(?:-[a-z0-9]+)*$/.test(row?.id ?? '')) {
      errors.push(`${label}: invalid id`);
    } else if (seenIDs.has(row.id)) {
      errors.push(`${label}: duplicate id`);
    }
    seenIDs.add(row?.id);

    if (requirement !== undefined) {
      if (row.section !== requirement.section) errors.push(`${label}: section is stale`);
      if (normalizeSourceText(row.source_text ?? '') !== requirement.source_text) {
        errors.push(`${label}: source_text is stale`);
      }
      if (row.source_text_sha256 !== requirement.source_text_sha256) {
        errors.push(`${label}: source_text_sha256 is stale`);
      }
      if (row.profile !== requirement.profile) errors.push(`${label}: profile is stale`);
    }
    if (!allowedStatuses.has(row.status)) errors.push(`${label}: invalid status`);
    if (row.profile === 'core' && row.status !== 'pass') errors.push(`${label}: core rows must pass`);
    if (row.profile === 'extension' && !['pass', 'not_implemented_optional'].includes(row.status)) {
      errors.push(`${label}: extension status is invalid`);
    }
    if (row.profile === 'real_integration' && !['pass', 'skipped_real_profile'].includes(row.status)) {
      errors.push(`${label}: real-integration status is invalid`);
    }
    if (!Array.isArray(row.evidence) || row.evidence.length === 0) {
      errors.push(`${label}: evidence is required`);
      continue;
    }
    let hasReport = false;
    let hasDeferralProof = false;
    const seenEvidence = new Set();
    for (const reference of row.evidence) {
      if (seenEvidence.has(reference)) errors.push(`${label}: duplicate evidence reference ${reference}`);
      seenEvidence.add(reference);
      const parsed = parseEvidence(reference);
      if (parsed === null) {
        errors.push(`${label}: invalid evidence reference ${JSON.stringify(reference)}`);
        continue;
      }
      if (parsed.kind === 'report') hasReport = true;
      if (reference === 'go:./tests/conformance::TestDeferredExtensionsRemainUnclaimed') hasDeferralProof = true;
      if (parsed.kind === 'command' && row.profile !== 'real_integration') {
        errors.push(`${label}: command-only evidence is limited to real integration`);
      }
      if (parsed.kind === 'command' && /synthetic[_ -]?pass|fake[_ -]?success|assumecompatible/i.test(parsed.command)) {
        errors.push(`${label}: synthetic evidence is forbidden`);
      }
      if (validateEvidenceExistence && !evidenceExists(parsed, {goTests, playwrightTitles, goRoot, fileExists, readText})) {
        errors.push(`${label}: evidence does not exist: ${reference}`);
      }
    }
    if (row.status === 'not_implemented_optional' && !hasReport) {
      errors.push(`${label}: not_implemented_optional requires a report citation`);
    }
    if (row.status === 'not_implemented_optional' && !hasDeferralProof) {
      errors.push(`${label}: not_implemented_optional requires the exact deferral-boundary test`);
    }
  }
  return errors;
}

function capture(command, args, {cwd, env = process.env} = {}) {
  const result = spawnSync(command, args, {
    cwd,
    env,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  return {
    status: result.status ?? 2,
    stdout: result.stdout ?? '',
    stderr: result.stderr ?? '',
    error: result.error,
  };
}

function enumerateGoTests(packages, {goRoot, runner = capture, selection = selectGoCommand()} = {}) {
  if (selection === null) throw new Error('Go 1.26.5 is unavailable through PATH or the repository mise toolchain.');
  const tests = new Map();
  for (const packageName of packages) {
    const result = runner(selection.command, [...selection.prefix, 'test', packageName, '-list', '^Test'], {cwd: goRoot});
    if (result.error) throw result.error;
    if (result.status !== 0) throw new Error(`go test -list failed for ${packageName}: ${result.stderr.trim()}`);
    tests.set(packageName, new Set(result.stdout.split(/\r?\n/).filter((line) => /^Test[A-Za-z0-9_]+$/.test(line))));
  }
  return tests;
}

function enumeratePlaywrightTitles({goRoot, runner = capture} = {}) {
  const cli = path.join(goRoot, 'node_modules', '@playwright', 'test', 'cli.js');
  const result = runner(process.execPath, [cli, 'test', '--list'], {cwd: goRoot});
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`playwright --list failed: ${result.stderr.trim()}`);
  const titles = new Set();
  for (const line of result.stdout.split(/\r?\n/)) {
    const match = line.match(/^\s*\[[^\]]+\]\s+›\s+([^:]+(?:\.[cm]?js)):\d+:\d+\s+›\s+(.+)$/);
    if (match) titles.add(`${match[1].replaceAll('\\', '/')}::${match[2].trim()}`);
  }
  return titles;
}

export function run({
  goRoot = defaultGoRoot,
  runner = capture,
  selection,
  output = console.log,
  error = console.error,
  listRequirements = false,
} = {}) {
  try {
    const spec = readFileSync(path.resolve(goRoot, '..', 'SPEC.md'), 'utf8');
    const requirements = extractGovernedRequirements(spec);
    if (listRequirements) {
      output(JSON.stringify(requirements, null, 2));
      return 0;
    }
    const manifest = JSON.parse(readFileSync(path.join(goRoot, 'tests', 'conformance', 'upstream-requirements.json'), 'utf8'));
    const parsedEvidence = manifest.rows.flatMap((row) => row.evidence.map(parseEvidence)).filter(Boolean);
    const packages = [...new Set(parsedEvidence.filter((entry) => entry.kind === 'go').map((entry) => entry.package))].sort();
    const goTests = enumerateGoTests(packages, {goRoot, runner, selection});
    const playwrightTitles = enumeratePlaywrightTitles({goRoot, runner});
    const errors = validateManifest(manifest, requirements, {goRoot, goTests, playwrightTitles});
    if (errors.length > 0) {
      for (const message of errors) error(message);
      return 1;
    }
    let section = '';
    for (const row of manifest.rows) {
      if (row.section !== section) {
        section = row.section;
        output(`Section ${section}`);
      }
      output(`  ${row.id}  ${row.status}  ${row.evidence.length} evidence`);
    }
    output(`Validated ${manifest.rows.length} governed requirements.`);
    return 0;
  } catch (caught) {
    error(caught instanceof Error ? caught.message : String(caught));
    return 2;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === thisFile) {
  process.exitCode = run({listRequirements: process.argv.includes('--list-requirements')});
}
