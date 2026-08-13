import AxeBuilder from '@axe-core/playwright';
import {test, expect, authorize, scenarioPath} from './fixtures.mjs';

test.describe.configure({mode: 'serial', retries: 0});

async function expectNoAxeViolations(page) {
  const results = await new AxeBuilder({page})
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'])
    .analyze();
  expect(results.violations.map(({id, nodes}) => ({
    id,
    targets: nodes.map(node => node.target),
  }))).toEqual([]);
}

async function requestRuntime(page, action) {
  const token = await page.locator('input[name="csrf_token"]').first().inputValue();
  return page.evaluate(async ({target, csrf}) => {
    const response = await fetch(target, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf},
      body: '{}',
    });
    return response.status;
  }, {target: scenarioPath(`/api/v1/runtime/${action}`, 'live-runtime-controls'), csrf: token});
}

test('keyboard start and stop preserve focus on the next available control', async ({page}) => {
  await authorize(page, scenarioPath('/', 'live-runtime-controls'));
  if (await page.getByText('Running', {exact: true}).count()) {
    expect(await requestRuntime(page, 'stop')).toBe(202);
    await page.reload();
  }

  const start = page.getByRole('button', {name: 'Start scheduler'});
  await start.focus();
  await Promise.all([
    page.waitForURL(url => url.searchParams.get('result') === 'runtime-started'),
    page.keyboard.press('Enter'),
  ]);
  await expect(page.getByText('Running', {exact: true})).toBeVisible();
  await expect(page.getByRole('status')).toHaveText('Scheduler start requested.');
  const stop = page.getByRole('button', {name: 'Stop scheduler'});
  await expect(stop).toBeFocused();
  await expectNoAxeViolations(page);

  await Promise.all([
    page.waitForURL(url => url.searchParams.get('result') === 'runtime-stopped'),
    page.keyboard.press('Enter'),
  ]);
  await expect(page.getByText('Paused', {exact: true})).toBeVisible();
  await expect(page.getByRole('status')).toHaveText('Scheduler stop requested.');
  await expect(page.getByRole('button', {name: 'Start scheduler'})).toBeFocused();

  await page.goto(scenarioPath('/issues/CTRL-1', 'live-runtime-controls'));
  await expect(page.getByText('stopping', {exact: true})).toBeVisible();
  await expectNoAxeViolations(page);
});

test('live worker state does not move focus and remains held while presentation is paused', async ({page}) => {
  await authorize(page, scenarioPath('/', 'live-runtime-controls'));
  expect(await requestRuntime(page, 'stop')).toBe(202);
  await page.reload();

  const liveToggle = page.getByRole('button', {name: 'Pause live updates'});
  await liveToggle.focus();
  expect(await requestRuntime(page, 'start')).toBe(202);
  await expect(page.getByText('Running', {exact: true})).toBeVisible();
  await expect(liveToggle).toBeFocused();

  await page.keyboard.press('Enter');
  await expect(page.getByRole('button', {name: 'Resume live updates'})).toBeFocused();
  expect(await requestRuntime(page, 'stop')).toBe(202);
  await page.waitForTimeout(250);
  await expect(page.getByText('Running', {exact: true})).toBeVisible();
  await page.keyboard.press('Enter');
  await expect(page.getByText('Paused', {exact: true})).toBeVisible();
  await expectNoAxeViolations(page);
});

test('automatic runtime snapshots preserve the focused start and stop controls', async ({page}) => {
  await authorize(page, scenarioPath('/', 'live-runtime-controls'));
  expect(await requestRuntime(page, 'stop')).toBe(202);
  await page.reload();

  const start = page.getByRole('button', {name: 'Start scheduler'});
  const stop = page.getByRole('button', {name: 'Stop scheduler'});
  await start.focus();
  expect(await requestRuntime(page, 'start')).toBe(202);
  await expect(page.getByText('Running', {exact: true})).toBeVisible();
  await expect(start).toBeFocused();
  await expect(start).toHaveAttribute('aria-disabled', 'true');
  expect(await start.evaluate(button => button.disabled)).toBe(false);
  const startURL = page.url();
  await page.keyboard.press('Enter');
  expect(page.url()).toBe(startURL);

  await stop.focus();
  await expect.poll(() => start.evaluate(button => button.disabled)).toBe(true);
  expect(await requestRuntime(page, 'stop')).toBe(202);
  await expect(page.getByText('Paused', {exact: true})).toBeVisible();
  await expect(stop).toBeFocused();
  await expect(stop).toHaveAttribute('aria-disabled', 'true');
  expect(await stop.evaluate(button => button.disabled)).toBe(false);
  const stopURL = page.url();
  await page.keyboard.press('Enter');
  expect(page.url()).toBe(stopURL);

  await start.focus();
  await expect.poll(() => stop.evaluate(button => button.disabled)).toBe(true);
  await expectNoAxeViolations(page);
});

for (const state of [
  {name: 'unavailable', path: scenarioPath('/', 'runtime-unavailable'), text: 'Unavailable'},
  {name: 'retrying', path: scenarioPath('/issues/RETRY-1', 'runtime-retrying'), text: 'A retry is scheduled.'},
  {name: 'stalled', path: scenarioPath('/issues/STALL-1', 'runtime-stalled'), text: 'stalled'},
  {name: 'stopping', path: scenarioPath('/', 'runtime-stopping'), text: 'Stopping'},
]) {
  test(`${state.name} runtime state is semantic and has no axe violations`, async ({page}) => {
    await authorize(page, state.path);
    await expect(page.getByText(state.text, {exact: true})).toBeVisible();
    if (state.name === 'unavailable' || state.name === 'stopping') {
      await expect(page.getByRole('button', {name: 'Start scheduler'})).toBeDisabled();
      await expect(page.getByRole('button', {name: 'Stop scheduler'})).toBeDisabled();
    }
    if (state.name === 'retrying') {
      const due = page.locator('section[aria-labelledby="retry-heading"] time');
      await expect(due).toHaveAttribute('datetime', '2026-08-08T16:05:00Z');
      await expect(due).not.toHaveText('');
    }
    await expectNoAxeViolations(page);
  });
}
