import AxeBuilder from '@axe-core/playwright';
import {test, expect, authorize, scenarioPath} from './fixtures.mjs';

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

for (const scenario of ['empty', 'populated', 'degraded-log', 'long-log', 'malicious-text']) {
  test(`${scenario} log state has no axe violations`, async ({page}) => {
    await authorize(page, scenarioPath('/logs', scenario));
    await expect(page.getByRole('status')).toHaveCount(0);
    await expectNoAxeViolations(page);
  });
}

test('wide log table and narrow list are mutually exclusive', async ({page}) => {
  await page.setViewportSize({width: 1100, height: 800});
  await authorize(page, scenarioPath('/logs', 'populated'));
  const table = page.getByRole('table', {name: 'Application log entries'});
  const list = page.getByRole('list', {name: 'Application log entries'});
  await expect(table).toBeVisible();
  await expect(list).toBeHidden();
  await page.setViewportSize({width: 320, height: 900});
  await expect(table).toBeHidden();
  await expect(list).toBeVisible();
  const reflow = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(reflow[0]).toBeLessThanOrEqual(reflow[1]);
});

test('structured-field focus follows the same logical log across both reflow directions', async ({page}) => {
  await page.setViewportSize({width: 1280, height: 900});
  await authorize(page, scenarioPath('/logs', 'populated'));
  const wideGroup = page.locator('.responsive-wide').getByRole('group', {name: 'Structured fields for log entry 2'});
  const narrowGroup = page.locator('.responsive-narrow').getByRole('group', {name: 'Structured fields for log entry 2'});

  await wideGroup.focus();
  await expectFocusedVisibleAndUnobscured(wideGroup);
  await page.setViewportSize({width: 320, height: 900});
  await expectFocusedVisibleAndUnobscured(narrowGroup);

  await page.setViewportSize({width: 1280, height: 900});
  await expectFocusedVisibleAndUnobscured(wideGroup);
});

test('long unbroken log fields overflow only inside their labelled scroll regions', async ({page}) => {
  for (const width of [1100, 320]) {
    await page.setViewportSize({width, height: 900});
    await authorize(page, scenarioPath('/logs', 'long-log'));
    const dimensions = await page.evaluate(() => {
      const region = [...document.querySelectorAll('.log-scroll')].find(candidate => candidate.getClientRects().length > 0);
      return {
        viewport: document.documentElement.clientWidth,
        page: document.documentElement.scrollWidth,
        region: region?.clientWidth ?? 0,
        regionContent: region?.scrollWidth ?? 0,
      };
    });
    expect(dimensions.page, `${width}px document overflow`).toBeLessThanOrEqual(dimensions.viewport);
    expect(dimensions.regionContent, `${width}px labelled region is not scrollable`).toBeGreaterThan(dimensions.region);
  }
});

test('log filtering and pagination work without JavaScript and preserve scenario', async ({page}) => {
  await page.route('**/static/app.js', route => route.abort());
  await authorize(page, scenarioPath('/logs', 'long-log'));
  await expect(page.getByText('Message truncated for display.').first()).toBeVisible();
  await expect(page.getByText('Structured fields truncated for display.').first()).toBeVisible();
  await page.getByRole('link', {name: 'Older log entries'}).click();
  let current = new URL(page.url());
  expect(current.searchParams.get('__e2e_scenario')).toBe('long-log');
  expect(current.searchParams.has('before')).toBeTruthy();
  await page.getByRole('link', {name: 'Newest log entries'}).click();
  current = new URL(page.url());
  expect(current.searchParams.get('__e2e_scenario')).toBe('long-log');
  expect(current.searchParams.has('before')).toBeFalsy();

  await page.getByLabel('Level').selectOption('INFO');
  await page.getByRole('button', {name: 'Apply log filters'}).click();
  current = new URL(page.url());
  expect(current.searchParams.get('__e2e_scenario')).toBe('long-log');
  expect(current.searchParams.get('level')).toBe('INFO');
});

test('degraded logging is persistent text and structured pre is labelled focusable overflow', async ({page}) => {
  await page.setViewportSize({width: 320, height: 900});
  await authorize(page, scenarioPath('/logs', 'degraded-log'));
  await expect(page.getByText('Symphony logging is degraded. Recent in-memory entries remain available.')).toBeVisible();
  const region = page.getByRole('group', {name: /Structured fields for log entry/}).first();
  await region.focus();
  await expect(region).toBeFocused();
  await expect(region).toHaveCSS('overflow-x', 'auto');
  await expect(page.getByRole('status')).toHaveCount(0);
});

test('malicious log text renders literally without creating executable markup', async ({page}) => {
  await authorize(page, scenarioPath('/logs', 'malicious-text'));
  await expect(page.getByText('<script>fixture-log-canary</script>', {exact: true}).first()).toBeVisible();
  await expect(page.locator('script', {hasText: 'fixture-log-canary'})).toHaveCount(0);
});
