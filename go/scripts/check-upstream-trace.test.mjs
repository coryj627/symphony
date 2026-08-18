import assert from 'node:assert/strict';
import test from 'node:test';

import {
  extractGovernedRequirements,
  markdownAnchors,
  parseEvidence,
  sourceTextSHA256,
  validateManifest,
} from './check-upstream-trace.mjs';

const fixtureSpec = `
### 17.1 Workflow and Config Parsing

- Parent requirement:
  - child requirement
- A wrapped requirement that
  continues on another line

### 17.5 Coding-Agent App-Server Client

- If provider-native agent tools are implemented:
  - only selected tools are advertised

### 17.8 Real Integration Profile (RECOMMENDED)

- Real smoke can run

### 18.2 RECOMMENDED Extensions (Not REQUIRED for Conformance)

- TODO: Deferred extension.
`;

test('extracts every governed nested bullet with stable profiles and hashes', () => {
  const rows = extractGovernedRequirements(fixtureSpec);
  assert.deepEqual(rows.map(({section, depth, source_text, profile}) => ({section, depth, source_text, profile})), [
    {section: '17.1', depth: 0, source_text: 'Parent requirement:', profile: 'core'},
    {section: '17.1', depth: 1, source_text: 'child requirement', profile: 'core'},
    {section: '17.1', depth: 0, source_text: 'A wrapped requirement that continues on another line', profile: 'core'},
    {section: '17.5', depth: 0, source_text: 'If provider-native agent tools are implemented:', profile: 'extension'},
    {section: '17.5', depth: 1, source_text: 'only selected tools are advertised', profile: 'extension'},
    {section: '17.8', depth: 0, source_text: 'Real smoke can run', profile: 'real_integration'},
    {section: '18.2', depth: 0, source_text: 'TODO: Deferred extension.', profile: 'extension'},
  ]);
  assert.equal(rows[0].source_text_sha256, sourceTextSHA256('Parent requirement:'));
});

test('parses only exact evidence reference shapes', () => {
  assert.deepEqual(parseEvidence('go:./internal/workflow::TestLoadMissingFileIsTyped'), {
    kind: 'go', package: './internal/workflow', name: 'TestLoadMissingFileIsTyped',
  });
  assert.deepEqual(parseEvidence('playwright:tests/accessibility/shell.spec.mjs::skip link works'), {
    kind: 'playwright', file: 'tests/accessibility/shell.spec.mjs', title: 'skip link works',
  });
  assert.deepEqual(parseEvidence('report:../docs/design.md#definition-of-done'), {
    kind: 'report', file: '../docs/design.md', anchor: 'definition-of-done',
  });
  assert.equal(parseEvidence('go:./internal/workflow:TestLooseDelimiter'), null);
  assert.equal(parseEvidence('report:../docs/design.md'), null);
});

test('validates order, exact evidence, status boundaries, and optional citations', () => {
  const requirements = extractGovernedRequirements(fixtureSpec);
  const rows = requirements.map((requirement, index) => ({
    id: `S${requirement.section}-fixture-${index + 1}`,
    section: requirement.section,
    profile: requirement.profile,
    source_text: requirement.source_text,
    source_text_sha256: requirement.source_text_sha256,
    status: requirement.profile === 'real_integration' ? 'skipped_real_profile' : 'pass',
    evidence: ['go:./internal/fixture::TestFixture'],
  }));
  rows.at(-1).status = 'not_implemented_optional';
  rows.at(-1).evidence = [
    'go:./tests/conformance::TestDeferredExtensionsRemainUnclaimed',
    'report:../docs/design.md#definition-of-done',
  ];
  const manifest = {schema_version: 1, spec_path: '../SPEC.md', rows};
  const options = {
    goTests: new Map([
      ['./internal/fixture', new Set(['TestFixture'])],
      ['./tests/conformance', new Set(['TestDeferredExtensionsRemainUnclaimed'])],
    ]),
    playwrightTitles: new Set(),
    fileExists: () => true,
    readText: () => '# Definition of done\n',
  };
  assert.deepEqual(validateManifest(manifest, requirements, options), []);

  manifest.rows[0].source_text_sha256 = '0'.repeat(64);
  manifest.rows[1].evidence = ['go:./internal/fixture::TestMissing'];
  const errors = validateManifest(manifest, requirements, options);
  assert.ok(errors.some((message) => message.includes('source_text_sha256 is stale')));
  assert.ok(errors.some((message) => message.includes('evidence does not exist')));
});

test('computes GitHub-style heading anchors used by report evidence', () => {
  assert.deepEqual(
    [...markdownAnchors('# Definition of Done\n## 18.2 — Recommended Extensions\n')],
    ['definition-of-done', '182-recommended-extensions'],
  );
});

test('rejects duplicate, synthetic, and unproved optional evidence', () => {
  const requirements = extractGovernedRequirements(fixtureSpec);
  const rows = requirements.map((requirement, index) => ({
    id: `S${requirement.section}-negative-${index + 1}`,
    section: requirement.section,
    profile: requirement.profile,
    source_text: requirement.source_text,
    source_text_sha256: requirement.source_text_sha256,
    status: requirement.profile === 'real_integration' ? 'skipped_real_profile' : 'pass',
    evidence: ['go:./internal/fixture::TestFixture'],
  }));
  rows[0].evidence = ['command:FAKE_SUCCESS', 'command:FAKE_SUCCESS'];
  rows.at(-1).status = 'not_implemented_optional';
  rows.at(-1).evidence = ['report:../docs/design.md#definition-of-done'];
  const errors = validateManifest(
    {schema_version: 1, spec_path: '../SPEC.md', rows},
    requirements,
    {
      goTests: new Map([['./internal/fixture', new Set(['TestFixture'])]]),
      fileExists: () => true,
      readText: () => '# Definition of done\n',
    },
  );
  assert.ok(errors.some((message) => message.includes('duplicate evidence reference')));
  assert.ok(errors.some((message) => message.includes('command-only evidence')));
  assert.ok(errors.some((message) => message.includes('synthetic evidence')));
  assert.ok(errors.some((message) => message.includes('exact deferral-boundary test')));
});
