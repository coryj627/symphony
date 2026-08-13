import AxeBuilder from '@axe-core/playwright';
import {test, expect, authorize, scenarioPath} from './fixtures.mjs';

const codexRuntimeStates = [
  scenarioPath('/', 'live-operator-requests'),
  scenarioPath('/', 'codex-incompatible'),
  scenarioPath('/issues/CODEX-TOOL-1', 'codex-tool-failure'),
  scenarioPath('/issues/STOPFAIL-1', 'runtime-stopping-failed'),
];

for (const path of codexRuntimeStates) {
  test(`Codex runtime state ${path} has no WCAG A or AA axe violations`, async ({page}) => {
    await authorize(page, path);
    const results = await new AxeBuilder({page})
      .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'])
      .analyze();
    expect(results.violations.map(({id, nodes}) => ({
      id,
      targets: nodes.map(node => node.target),
    }))).toEqual([]);
  });
}
