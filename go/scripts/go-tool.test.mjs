import assert from 'node:assert/strict';
import test from 'node:test';

import {selectGoCommand} from './go-tool.mjs';

test('uses ambient Go when setup-go or PATH provides it', () => {
  const selected = selectGoCommand({probe: (command) => command === 'go'});
  assert.deepEqual(selected, {command: 'go', prefix: []});
});

test('falls back to the pinned mise Go when ambient Go is absent', () => {
  const probes = [];
  const selected = selectGoCommand({
    probe: (command, args) => {
      probes.push([command, args]);
      return command === 'mise';
    },
  });

  assert.deepEqual(selected, {command: 'mise', prefix: ['exec', '--', 'go']});
  assert.deepEqual(probes, [
    ['go', ['version']],
    ['mise', ['exec', '--', 'go', 'version']],
  ]);
});

test('fails closed when neither ambient nor pinned Go is available', () => {
  assert.equal(selectGoCommand({probe: () => false}), null);
});
