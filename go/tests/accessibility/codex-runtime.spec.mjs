import {test, expect, authorize, scenarioPath} from './fixtures.mjs';

test.describe.configure({mode: 'serial', retries: 0});

test('Codex operator requests remain named, finite, and keyboard reachable', async ({page}) => {
  await authorize(page, scenarioPath('/', 'live-operator-requests'));
  const region = page.getByRole('region', {name: 'Operator requests'});
  await expect(region).toBeVisible();
  await expect(region.getByRole('heading', {name: 'Approve command execution'})).toBeVisible();
  await expect(region.getByRole('heading', {name: 'Codex needs your input'})).toBeVisible();
  await expect(region.getByRole('group', {name: 'Choose a response'}).first()).toBeVisible();
  await expect(region.getByRole('group', {name: 'Platform'})).toBeVisible();
  await expect(region.getByLabel('Secret answer')).toHaveAttribute('type', 'password');

  const control = region.getByRole('radio', {name: 'Allow once'}).first();
  await control.focus();
  await page.keyboard.press('Space');
  await expect(control).toBeChecked();
  await expect(control).toBeFocused();
});

test('Codex request deadlines and stale responses have explicit status and recovery focus', async ({page}) => {
  await page.clock.install();
  await authorize(page, scenarioPath('/', 'live-operator-requests'));
  const expiring = page.locator('[data-operator-request]').filter({has: page.getByRole('heading', {name: 'Expiring approval'})});
  const deadlineDisplay = expiring.locator('[data-request-deadline]');
  await expect(deadlineDisplay).toHaveAttribute('datetime', /.+Z$/);
  const deadline = Date.parse(await expiring.getAttribute('data-deadline') ?? '');
  const browserNow = await page.evaluate(() => Date.now());
  await page.clock.fastForward(deadline - browserNow - 15_000);
  await expect(expiring.getByRole('status')).toHaveText('This request expires in 20 seconds.');

  const stale = page.locator('[data-operator-request]').filter({has: page.getByRole('heading', {name: 'Stale approval example'})});
  await stale.getByRole('radio', {name: 'Deny'}).check();
  await stale.getByRole('button', {name: 'Submit response'}).click();
  await expect(page.locator('#error-summary')).toBeFocused();
  await expect(page.locator('#error-summary')).toContainText('This operator request is no longer pending.');
});

for (const state of [
  {
    name: 'incompatible app-server',
    path: scenarioPath('/', 'codex-incompatible'),
    text: 'The installed Codex CLI does not match the reviewed app-server version.',
  },
  {
    name: 'provider tool failure',
    path: scenarioPath('/issues/CODEX-TOOL-1', 'codex-tool-failure'),
    text: 'The provider tool returned a safe failure result.',
  },
  {
    name: 'process cleanup failure',
    path: scenarioPath('/issues/STOPFAIL-1', 'runtime-stopping-failed'),
    text: 'Manual cleanup may be required before restarting.',
  },
]) {
  test(`${state.name} is exposed as visible text without relying on color`, async ({page}) => {
    await authorize(page, state.path);
    const main = page.getByRole('main');
    await expect(main).toBeVisible();
    await expect(main.getByText(state.text, {exact: true}).first()).toBeVisible();
  });
}

const codexPresentationStates = [
  {
    path: scenarioPath('/', 'live-operator-requests'),
    heading: 'Operator requests',
  },
  {
    path: scenarioPath('/', 'codex-incompatible'),
    text: 'The installed Codex CLI does not match the reviewed app-server version.',
  },
  {
    path: scenarioPath('/issues/CODEX-TOOL-1', 'codex-tool-failure'),
    text: 'The provider tool returned a safe failure result.',
  },
  {
    path: scenarioPath('/issues/STOPFAIL-1', 'runtime-stopping-failed'),
    text: 'Manual cleanup may be required before restarting.',
  },
];

test('Codex runtime states reflow at 320 CSS pixels as a 400 percent zoom equivalent', async ({page}) => {
  await page.setViewportSize({width: 320, height: 900});
  for (const state of codexPresentationStates) {
    await authorize(page, state.path);
    if (state.heading) {
      await expect(page.getByRole('heading', {name: state.heading, exact: true})).toBeVisible();
    } else {
      await expect(page.getByText(state.text, {exact: true}).first()).toBeVisible();
    }
    const dimensions = await page.evaluate(() => ({
      viewport: document.documentElement.clientWidth,
      page: document.documentElement.scrollWidth,
    }));
    expect(dimensions.page, state.path).toBeLessThanOrEqual(dimensions.viewport);
  }
});

test('Codex operator requests preserve content and controls with WCAG text spacing', async ({page}) => {
  await page.setViewportSize({width: 640, height: 900});
  await authorize(page, scenarioPath('/', 'live-operator-requests'));
  await page.evaluate(() => {
    const sheet = [...document.styleSheets].find(candidate => candidate.href?.endsWith('/static/app.css'));
    if (!sheet) throw new Error('local application stylesheet was not loaded');
    for (const rule of [
      '* { letter-spacing: 0.12em !important; line-height: 1.5 !important; word-spacing: 0.16em !important; }',
      'p { margin-block-end: 2em !important; }',
    ]) {
      sheet.insertRule(rule, sheet.cssRules.length);
    }
  });

  await expect(page.getByRole('heading', {name: 'Approve command execution'})).toBeVisible();
  await expect(page.getByRole('button', {name: 'Submit response'}).first()).toBeVisible();
  const dimensions = await page.evaluate(() => ({
    viewport: document.documentElement.clientWidth,
    page: document.documentElement.scrollWidth,
  }));
  expect(dimensions.page).toBeLessThanOrEqual(dimensions.viewport);
});

test('Codex request controls honor reduced motion', async ({page}) => {
  await page.emulateMedia({reducedMotion: 'reduce'});
  await authorize(page, scenarioPath('/', 'live-operator-requests'));
  const styles = await page.getByRole('button', {name: 'Submit response'}).first().evaluate(element => {
    const computed = getComputedStyle(element);
    return {
      transitionDuration: computed.transitionDuration,
      animationDuration: computed.animationDuration,
      scrollBehavior: getComputedStyle(document.documentElement).scrollBehavior,
    };
  });
  expect(styles).toEqual({transitionDuration: '0s', animationDuration: '0s', scrollBehavior: 'auto'});
});

test('Codex request controls remain distinguishable in forced colors', async ({page, browserName}) => {
  test.skip(browserName !== 'chromium', 'Playwright forced-colors emulation is Chromium-only.');
  await page.emulateMedia({forcedColors: 'active'});
  await authorize(page, scenarioPath('/', 'live-operator-requests'));
  await expect(page.getByRole('radio', {name: 'Allow once'}).first()).toHaveCSS('appearance', 'auto');
  await expect(page.getByRole('button', {name: 'Submit response'}).first()).toHaveCSS('border-style', 'solid');
});

test('Codex protocol and tool states do not become live-region announcements', async ({page}) => {
  for (const state of codexPresentationStates.slice(1)) {
    await authorize(page, state.path);
    await expect(page.locator('[role="status"], [aria-live]:not([aria-live="off"])')).toHaveCount(0);
  }
});
