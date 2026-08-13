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

function requestCard(page, title) {
  return page.locator('[data-operator-request]').filter({has: page.getByRole('heading', {name: title, exact: true})});
}

test('operator requests expose finite named keyboard workflows without moving focus', async ({page}) => {
  await page.clock.install();
  await authorize(page, scenarioPath('/', 'live-operator-requests'));

  const region = page.getByRole('region', {name: 'Operator requests'});
  await expect(region).toBeVisible();
  await expect(region.getByText('REQ-1', {exact: true}).first()).toBeVisible();
  await expect(region.getByText('thread-1-turn-1', {exact: true}).first()).toBeVisible();
  await expect(requestCard(page, 'Approve command execution').getByText('go test ./...', {exact: true})).toBeVisible();
  await expect(requestCard(page, 'Approve additional permissions').getByText('{"network":{"enabled":true}}', {exact: true})).toBeVisible();
  await expect(page.getByRole('group', {name: 'Choose a response'}).first()).toBeVisible();
  await expect(page.getByRole('group', {name: 'Platform'})).toBeVisible();
  await expect(page.getByRole('group', {name: 'Detail'})).toBeVisible();
  await expect(page.getByRole('group', {name: 'Token'})).toBeVisible();
  await expect(page.getByLabel('Your answer')).toBeVisible();
  const secret = page.getByLabel('Secret answer');
  await expect(secret).toHaveAttribute('type', 'password');
  await secret.fill('temporary-secret');
  expect(await secret.evaluate(input => input.dispatchEvent(new Event('paste', {bubbles: true, cancelable: true})))).toBe(true);

  const expiring = requestCard(page, 'Expiring approval');
  const extend = expiring.getByRole('button', {name: 'Extend response time'});
  const dimensions = await extend.boundingBox();
  expect(dimensions?.width).toBeGreaterThanOrEqual(44);
  expect(dimensions?.height).toBeGreaterThanOrEqual(44);
  await extend.focus();
  const deadline = Date.parse(await expiring.getAttribute('data-deadline') ?? '');
  const browserNow = await page.evaluate(() => Date.now());
  const advanceToWarning = deadline - browserNow - 15_000;
  expect(advanceToWarning).toBeGreaterThan(0);
  await page.clock.fastForward(advanceToWarning);
  await expect(extend).toBeFocused();
  await expect(expiring.getByRole('status')).toHaveText('This request expires in 20 seconds.');
  await expect(expiring.locator('[data-request-deadline]')).toBeVisible();

  await expectNoAxeViolations(page);

  const command = requestCard(page, 'Approve command execution');
  const allowOnce = command.getByRole('radio', {name: 'Allow once'});
  await allowOnce.focus();
  await page.keyboard.press('Space');
  await expect(allowOnce).toBeChecked();
  const submit = command.getByRole('button', {name: 'Submit response'});
  await submit.focus();
  await Promise.all([
    page.waitForURL(url => url.searchParams.get('result') === 'request-responded'),
    page.keyboard.press('Enter'),
  ]);
  await expect(requestCard(page, 'Approve command execution')).toHaveCount(0);
  await expect(page.locator('.persistent-status[role="status"]')).toHaveText('Operator response submitted.');
  await expect(page.getByRole('heading', {name: 'Operator requests'})).toBeFocused();

  const stale = requestCard(page, 'Stale approval example');
  const decline = stale.getByRole('radio', {name: 'Deny'});
  await decline.focus();
  await page.keyboard.press('Space');
  const staleSubmit = stale.getByRole('button', {name: 'Submit response'});
  await staleSubmit.focus();
  const [staleResponse] = await Promise.all([
    page.waitForResponse(response => new URL(response.url()).pathname.endsWith('/stale-request/respond')),
    page.keyboard.press('Enter'),
  ]);
  expect(staleResponse.status()).toBe(409);
  await expect(page.locator('#error-summary')).toBeFocused();
  await expect(page.locator('#error-summary')).toContainText('This operator request is no longer pending.');
  await expectNoAxeViolations(page);
});
