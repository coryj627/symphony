import assert from 'node:assert/strict';
import test from 'node:test';

import {runGo, selectGoCommand} from './go-tool.mjs';

test('uses ambient Go when setup-go or PATH provides it', () => {
  const selected = selectGoCommand({
    probe: (command) => command === 'go' ? 'go version go1.26.5 darwin/arm64\n' : null,
  });
  assert.deepEqual(selected, {command: 'go', prefix: [], version: '1.26.5'});
});

test('falls back to the pinned mise Go when ambient Go is absent', () => {
  const probes = [];
  const selected = selectGoCommand({
    probe: (command, args) => {
      probes.push([command, args]);
      return command === 'mise' ? 'go version go1.26.5 darwin/arm64\n' : null;
    },
  });

  assert.deepEqual(selected, {command: 'mise', prefix: ['exec', '--', 'go'], version: '1.26.5'});
  assert.deepEqual(probes, [
    ['go', ['version']],
    ['mise', ['exec', '--', 'go', 'version']],
  ]);
});

test('fails closed when neither ambient nor pinned Go is available', () => {
  assert.equal(selectGoCommand({probe: () => null}), null);
});

for (const badVersion of [
  'go version go1.25.9 darwin/arm64\n',
  'go version go1.26.6 darwin/arm64\n',
  'not a go version\n',
]) {
  test(`rejects ambient Go output ${JSON.stringify(badVersion.trim())}`, () => {
    const selected = selectGoCommand({
      probe: (command) => command === 'go'
        ? badVersion
        : 'go version go1.26.5 darwin/arm64\n',
    });

    assert.deepEqual(selected, {
      command: 'mise',
      prefix: ['exec', '--', 'go'],
      version: '1.26.5',
    });
  });
}

test('fails closed when both ambient and mise Go versions are wrong', () => {
  assert.equal(selectGoCommand({probe: () => 'go version go1.26.6 darwin/arm64\n'}), null);
});

test('runGo rejects the wrong Node runtime before spawning', () => {
  let invoked = false;
  const code = runGo([], {
    nodeVersion: 'v24.18.1',
    selection: {command: 'go', prefix: [], version: '1.26.5'},
    spawn: () => {
      invoked = true;
      return {status: 0};
    },
    error: () => {},
  });

  assert.equal(code, 2);
  assert.equal(invoked, false);
});

test('runGo rejects an injected mismatched Go selection before spawning', () => {
  let invoked = false;
  const code = runGo([], {
    nodeVersion: 'v24.18.0',
    selection: {command: 'go', prefix: [], version: '1.26.6'},
    spawn: () => {
      invoked = true;
      return {status: 0};
    },
    error: () => {},
  });

  assert.equal(code, 2);
  assert.equal(invoked, false);
});
