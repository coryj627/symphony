import AxeBuilder from '@axe-core/playwright';
import {test, expect, authorize, scenarioPath} from './fixtures.mjs';
import {formatE2EDisplayTime} from './timezone.mjs';

async function expectNoAxeViolations(page) {
  const results = await new AxeBuilder({page})
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'])
    .analyze();
  expect(results.violations.map(({id, nodes}) => ({id, targets: nodes.map(node => node.target)}))).toEqual([]);
}

async function expectFocusedVisibleAndUnobscured(locator) {
  await expect(locator).toBeVisible();
  await expect(locator).toBeFocused();
  const result = await locator.evaluate(element => {
    const rect = element.getBoundingClientRect();
    const style = getComputedStyle(element);
    const outlineWidth = Number.parseFloat(style.outlineWidth);
    const outlineOffset = Number.parseFloat(style.outlineOffset);
    const clearance = outlineWidth + outlineOffset;
    const hit = document.elementFromPoint(rect.left + (rect.width / 2), rect.top + (rect.height / 2));
    return {
      hasClientRect: element.getClientRects().length > 0,
      focusVisible: element.matches(':focus-visible'),
      outlineStyle: style.outlineStyle,
      outlineWidth,
      inViewport: rect.left - clearance >= 0
        && rect.top - clearance >= 0
        && rect.right + clearance <= window.innerWidth
        && rect.bottom + clearance <= window.innerHeight,
      unobscured: hit === element || element.contains(hit),
    };
  });
  expect(result).toEqual({
    hasClientRect: true,
    focusVisible: true,
    outlineStyle: 'solid',
    outlineWidth: 3,
    inViewport: true,
    unobscured: true,
  });
}

for (const [name, path] of [
  ['empty', scenarioPath('/', 'empty')],
  ['populated', scenarioPath('/issues', 'populated')],
  ['stale error', scenarioPath('/', 'stale-error')],
  ['filtered empty', `${scenarioPath('/issues', 'filtered-empty')}&state=Open`],
  ['malicious provider text', scenarioPath('/issues/MAL-1', 'malicious-text')],
  ['encoded identifier', scenarioPath('/issues/TEAM%2F%2342', 'encoded-identifier')],
]) {
  test(`${name} queue state has no axe violations`, async ({page}) => {
    await authorize(page, path);
    await expect(page.getByRole('status')).toHaveCount(0);
    await expectNoAxeViolations(page);
  });
}

test('wide table and narrow list expose the same candidate links exclusively', async ({page}) => {
  await page.setViewportSize({width: 1100, height: 800});
  await authorize(page, scenarioPath('/issues', 'populated'));
  const table = page.getByRole('table', {name: 'Tracker work candidates'});
  const list = page.getByRole('list', {name: 'Tracker work candidates'});
  await expect(table).toBeVisible();
  await expect(list).toBeHidden();
  const wideIdentifiers = await table.getByRole('link').allTextContents();

  await page.setViewportSize({width: 320, height: 900});
  await expect(table).toBeHidden();
  await expect(list).toBeVisible();
  expect(await list.getByRole('link').allTextContents()).toEqual(wideIdentifiers);
  const reflow = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(reflow[0]).toBeLessThanOrEqual(reflow[1]);
});

test('candidate focus follows the same logical issue across both reflow directions', async ({page}) => {
  await page.setViewportSize({width: 1280, height: 900});
  await authorize(page, scenarioPath('/issues', 'populated'));
  const wideLink = page.locator('.responsive-wide').getByRole('link', {name: 'SYM-123'});
  const narrowLink = page.locator('.responsive-narrow').getByRole('link', {name: 'SYM-123'});

  await wideLink.focus();
  await expectFocusedVisibleAndUnobscured(wideLink);
  await page.setViewportSize({width: 320, height: 900});
  await expectFocusedVisibleAndUnobscured(narrowLink);

  await page.setViewportSize({width: 1280, height: 900});
  await expectFocusedVisibleAndUnobscured(wideLink);
});

test('candidate focus is not restored after an intentional outside pointer action', async ({page}) => {
  await page.setViewportSize({width: 1280, height: 900});
  await authorize(page, scenarioPath('/issues', 'populated'));
  const wideLink = page.locator('.responsive-wide').getByRole('link', {name: 'SYM-123'});
  const narrowLink = page.locator('.responsive-narrow').getByRole('link', {name: 'SYM-123'});

  await wideLink.focus();
  await expect(wideLink).toBeFocused();
  await page.getByText('Symphony runs on this workstation.', {exact: true}).click();
  const activeKey = await page.evaluate(() => document.activeElement?.dataset?.responsiveFocusKey ?? '');
  expect(activeKey).toBe('');

  await page.setViewportSize({width: 320, height: 900});
  await page.evaluate(() => new Promise(resolve => {
    requestAnimationFrame(() => requestAnimationFrame(resolve));
  }));
  await expect(narrowLink).not.toBeFocused();
});

test('no-JavaScript issue filters survive list detail and return journeys', async ({page}) => {
  await page.route('**/static/app.js', route => route.abort());
  await authorize(page, scenarioPath('/issues', 'populated'));
  await page.getByLabel('Search issues').fill('Improve');
  await page.getByLabel('State').selectOption('Open');
  await page.getByLabel('Eligibility').selectOption('routable');
  await page.getByLabel('Sort issues').selectOption('identifier');
  await page.getByRole('button', {name: 'Apply filters'}).click();
  await page.getByRole('link', {name: 'SYM-123'}).first().click();
  let current = new URL(page.url());
  expect(Object.fromEntries(current.searchParams)).toMatchObject({
    __e2e_scenario: 'populated', query: 'Improve', state: 'Open', eligibility: 'routable', sort: 'identifier',
  });
  await page.getByRole('link', {name: 'Return to filtered issues'}).click();
  current = new URL(page.url());
  expect(Object.fromEntries(current.searchParams)).toMatchObject({
    __e2e_scenario: 'populated', query: 'Improve', state: 'Open', eligibility: 'routable', sort: 'identifier',
  });
  await expect(page.getByLabel('Search issues')).toHaveValue('Improve');
  await expect(page.getByRole('link', {name: 'SYM-123'}).first()).toBeVisible();
  await expect(page.getByRole('link', {name: 'SYM-124'})).toHaveCount(0);
});

test('encoded opaque identifier survives list link, detail, and navigation', async ({page}) => {
  await page.route('**/static/app.js', route => route.abort());
  await authorize(page, scenarioPath('/issues', 'encoded-identifier'));
  await page.getByRole('link', {name: 'TEAM/#42'}).first().click();
  await expect(page.getByRole('heading', {level: 1, name: 'Issue TEAM/#42'})).toBeVisible();
  const current = new URL(page.url());
  expect(current.pathname).toBe('/issues/TEAM%2F%2342');
  expect(current.searchParams.get('__e2e_scenario')).toBe('encoded-identifier');
  await page.getByRole('link', {name: 'Activity'}).click();
  expect(new URL(page.url()).searchParams.get('__e2e_scenario')).toBe('encoded-identifier');
});

test('malicious provider text is text-only and unsafe URL is never linked', async ({page}) => {
  await authorize(page, scenarioPath('/issues/MAL-1', 'malicious-text'));
  await expect(page.getByText('<script>fixture-title-canary</script>', {exact: true})).toBeVisible();
  await expect(page.getByText('<img src=x onerror=fixture-description-canary>', {exact: true})).toBeVisible();
  await expect(page.locator('script', {hasText: 'fixture-title-canary'})).toHaveCount(0);
  await expect(page.locator('img')).toHaveCount(0);
  await expect(page.locator('a[href^="javascript:"]')).toHaveCount(0);
  await expect(page.getByText('Routing details are unavailable.')).toBeVisible();
});

test('stale and provider failures remain persistent ordinary text', async ({page}) => {
  await authorize(page, scenarioPath('/', 'stale-error'));
  await expect(page.getByText(/last known/)).toBeVisible();
  await expect(page.locator('[data-live-overview-field="config-message"]')).toHaveText('Configuration needs attention.');
  await expect(page.locator('[data-live-overview-error="config"]')).toContainText('Configuration needs attention.');
  await expect(page.locator('aside li', {hasText: 'W'.repeat(512)})).toBeVisible();
  await expect(page.getByRole('status')).toHaveCount(0);
});

test('refresh preserves scenario, restores focus, and announces one concise status', async ({page}) => {
  await authorize(page, scenarioPath('/', 'populated'));
  await Promise.all([
    page.waitForURL(url => url.searchParams.get('result') === 'refresh-requested'),
    page.getByRole('button', {name: 'Refresh tracker work'}).click(),
  ]);
  const current = new URL(page.url());
  expect(current.searchParams.get('__e2e_scenario')).toBe('populated');
  expect(current.searchParams.get('result')).toBe('refresh-requested');
  await expect(page.locator('.persistent-status')).toHaveText('Refresh requested.');
  const announcement = page.locator('[data-page-load-announcement-target]');
  await expect(announcement).toHaveText('Refresh requested.');
  await expect(announcement).toHaveCount(1);
  await expect(page.getByRole('button', {name: 'Refresh tracker work'})).toBeFocused();
});

test('refresh remains an ordinary form journey without JavaScript', async ({page}) => {
  await page.route('**/static/app.js', route => route.abort());
  await authorize(page, scenarioPath('/', 'populated'));
  await Promise.all([
    page.waitForURL(url => url.searchParams.get('result') === 'refresh-requested'),
    page.getByRole('button', {name: 'Refresh tracker work'}).click(),
  ]);
  const current = new URL(page.url());
  expect(current.searchParams.get('__e2e_scenario')).toBe('populated');
  expect(current.searchParams.get('result')).toBe('refresh-requested');
  await expect(page.getByRole('status')).toHaveText('Refresh requested.');
  await expect(page.getByRole('button', {name: 'Refresh tracker work'})).toBeFocused();
});

test('maximum-length provider labels and operator request text reflow at 320 pixels', async ({page}) => {
  await page.setViewportSize({width: 320, height: 900});
  await authorize(page, scenarioPath('/issues/MAL-1', 'malicious-text'));
  await expect(page.getByText('W'.repeat(512), {exact: true}).first()).toBeVisible();
  const dimensions = await page.evaluate(() => ({
    viewport: document.documentElement.clientWidth,
    page: document.documentElement.scrollWidth,
  }));
  expect(dimensions.page).toBeLessThanOrEqual(dimensions.viewport);
});

test('maximum-length provider error reflows at 320 pixels with WCAG text spacing', async ({page}) => {
  await page.setViewportSize({width: 320, height: 900});
  await authorize(page, scenarioPath('/', 'stale-error'));
  await expect(page.locator('aside li', {hasText: 'W'.repeat(512)})).toBeVisible();

  for (const spacing of [false, true]) {
    if (spacing) {
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
    }
    const dimensions = await page.evaluate(() => ({
      viewport: document.documentElement.clientWidth,
      page: document.documentElement.scrollWidth,
    }));
    expect(dimensions.page, spacing ? 'WCAG text spacing' : 'default spacing').toBeLessThanOrEqual(dimensions.viewport);
  }
});

test('issue metadata preserves RFC3339 values and displays workstation-local time', async ({page}) => {
  const instant = '2026-08-08T16:00:00Z';
  await authorize(page, scenarioPath('/issues/SYM-123', 'populated'));
  const times = page.locator('section[aria-labelledby="metadata-heading"] time');
  await expect(times).toHaveCount(2);
  await expect(times.first()).toHaveAttribute('datetime', instant);
  await expect(times.last()).toHaveAttribute('datetime', instant);
  const localTime = formatE2EDisplayTime(instant);
  await expect(times).toHaveText([localTime, localTime]);
});

test('valid-scenario issue not-found document has the expected shell and no axe violations', async ({page}) => {
  const response = await page.goto(scenarioPath('/issues/MISSING-1', 'issue-not-found'));
  expect(response?.status()).toBe(404);
  await expect(page).toHaveTitle('Page not found — Symphony');
  await expect(page.getByRole('main')).toHaveCount(1);
  await expect(page.getByRole('heading', {level: 1, name: 'Page not found', exact: true})).toHaveCount(1);
  await expect(page.getByRole('status')).toHaveCount(0);
  await expectNoAxeViolations(page);
});

test('valid scenario remains on navigation rendered inside an issue not-found document', async ({page}) => {
  const response = await page.goto(scenarioPath('/issues/MISSING-1', 'issue-not-found'));
  expect(response?.status()).toBe(404);
  const activity = page.getByRole('link', {name: 'Activity'});
  await expect(activity).toHaveAttribute('href', '/activity?__e2e_scenario=issue-not-found');
  await activity.click();
  expect(new URL(page.url()).searchParams.get('__e2e_scenario')).toBe('issue-not-found');
  await expect(page.getByRole('heading', {level: 1, name: 'Activity'})).toBeVisible();
});

test('invalid and multiple scenario selectors fail closed for HTML and JSON', async ({page}) => {
  for (const path of [
    '/issues?__e2e_scenario=unknown',
    '/issues?__e2e_scenario=empty&__e2e_scenario=populated',
    `/issues?__e2e_scenario=${'x'.repeat(33)}`,
    '/issues?__e2e_scenario=%ZZ',
  ]) {
    const response = await page.goto(path);
    expect(response?.status()).toBe(404);
    await expect(page).toHaveTitle('Page not found — Symphony');
  }
  const response = await page.request.get('/api/v1/state?__e2e_scenario=unknown');
  expect(response.status()).toBe(404);
  expect(await response.json()).toMatchObject({error: {code: 'not_found', retryable: false}});
});
