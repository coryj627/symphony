import AxeBuilder from '@axe-core/playwright';
import {test, expect, authorize, navigationLabels, routes} from './fixtures.mjs';

async function expectNoAxeViolations(page) {
  const results = await new AxeBuilder({page})
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'])
    .analyze();
  const safeViolations = results.violations.map(({id, impact, help, nodes}) => ({
    id,
    impact,
    help,
    targets: nodes.map(node => node.target),
  }));
  expect(safeViolations).toEqual([]);
}

for (const route of routes) {
  test(`${route.heading} has a valid accessible shell`, async ({page}) => {
    await authorize(page, route.path);
    await expect(page).toHaveTitle(route.title);
    await expect(page.getByRole('main')).toHaveCount(1);
    await expect(page.getByRole('heading', {level: 1, name: route.heading, exact: true})).toHaveCount(1);
    await expect(page.getByRole('status')).toHaveCount(0);

    await expectNoAxeViolations(page);
  });
}

test('missing session renders one accessible authorization document', async ({authenticatedContext}) => {
  const context = await authenticatedContext.browser().newContext();
  const page = await context.newPage();
  try {
    const response = await page.goto('/');
    expect(response?.status()).toBe(401);
    await expect(page).toHaveTitle('Authorization required — Symphony');
    await expect(page.getByRole('main')).toHaveCount(1);
    await expect(page.getByRole('heading', {level: 1, name: 'Authorization required'})).toHaveCount(1);
    await expect(page.getByRole('status')).toHaveCount(0);
    await expect(page.getByText('This browser session is missing or no longer valid.')).toBeVisible();
    await expect(page.getByText('Return to the terminal and open the newest Symphony launch URL.')).toBeVisible();
    await expectNoAxeViolations(page);
  } finally {
    await context.close();
  }
});

test('authenticated missing page renders one accessible not-found document', async ({page}) => {
  const response = await page.goto('/not-a-page');
  expect(response?.status()).toBe(404);
  await expect(page).toHaveTitle('Page not found — Symphony');
  await expect(page.getByRole('main')).toHaveCount(1);
  await expect(page.getByRole('heading', {level: 1, name: 'Page not found'})).toHaveCount(1);
  await expect(page.getByRole('status')).toHaveCount(0);
  await expect(page.getByText('The requested page is not available.')).toBeVisible();
  await expect(page.getByText('Use the primary navigation to choose an available page.')).toBeVisible();
  await expectNoAxeViolations(page);
});

test('nonempty flash replaces the fallback in the single persistent status', async ({page}) => {
  await authorize(page, '/configuration?result=configuration-saved&focus=save-structured');
  const status = page.getByRole('status');
  await expect(status).toHaveCount(1);
  await expect(status).toHaveText('Configuration saved.');
  await expect(page.getByText('Configuration is ready.')).toHaveCount(0);
  await expectNoAxeViolations(page);
});

test('skip link is first in focus order and moves focus to main', async ({page}) => {
  await authorize(page, '/configuration');
  await page.keyboard.press('Tab');
  const skipLink = page.getByRole('link', {name: 'Skip to main content'});
  await expect(skipLink).toBeFocused();
  await expect(skipLink).toHaveCSS('outline-style', 'solid');
  await expect(skipLink).toHaveCSS('outline-width', '3px');
  await expect(skipLink).toHaveCSS('outline-color', 'rgb(255, 209, 102)');
  await expect(skipLink).toHaveCSS('outline-offset', '3px');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('main')).toBeFocused();
});

test('persistent navigation keeps its order and text current state', async ({page}) => {
  for (const route of routes) {
    await authorize(page, route.path);
    const navigation = page.getByRole('navigation', {name: 'Primary'});
    await expect(navigation.getByRole('link')).toHaveText(navigationLabels);
    const current = navigation.locator('[aria-current="page"]');
    await expect(current).toHaveCount(1);
    await expect(current).toHaveText(route.heading.startsWith('Issue ') ? 'Issues' : route.heading);
  }
});

test('core navigation works when JavaScript is unavailable', async ({page}) => {
  await page.route('**/static/app.js', route => route.abort());
  await authorize(page, '/');
  await page.getByRole('link', {name: 'Configuration'}).click();
  await expect(page).toHaveURL(/\/configuration\?__e2e_scenario=empty$/);
  await expect(page.getByRole('heading', {level: 1, name: 'Configuration'})).toBeVisible();
});

test('configuration help fragment is visible and focused without JavaScript', async ({page}) => {
  await page.route('**/static/app.js', route => route.abort());
  await authorize(page, '/configuration');
  await page.getByRole('link', {name: 'Configuration documentation'}).click();
  await expect(page).toHaveURL(/\/configuration\?__e2e_scenario=empty#documentation$/);
  const help = page.locator('#documentation');
  await expect(help).toBeVisible();
  await expect(help).toBeFocused();
  await expect(help.getByRole('heading', {level: 2, name: 'Configuration help'})).toBeVisible();
});

test('pages reflow at 320 CSS pixels and product controls meet 44 pixel targets', async ({page}) => {
  await page.setViewportSize({width: 320, height: 900});
  for (const route of routes) {
    await authorize(page, route.path);
    const dimensions = await page.evaluate(() => ({
      viewport: document.documentElement.clientWidth,
      page: document.documentElement.scrollWidth,
    }));
    expect(dimensions.page).toBeLessThanOrEqual(dimensions.viewport);

    const controls = page.locator('nav a, button, input:not([type="hidden"]), select, textarea');
    for (let index = 0; index < await controls.count(); index += 1) {
      const control = controls.nth(index);
      if (!(await control.isVisible())) continue;
      const box = await control.boundingBox();
      const safeName = await control.evaluate(element => `${element.tagName.toLowerCase()}#${element.id || 'unnamed'}`);
      expect(box?.width, safeName).toBeGreaterThanOrEqual(44);
      expect(box?.height, safeName).toBeGreaterThanOrEqual(44);
    }
  }
});

test('two hundred percent text and WCAG text spacing preserve page content without viewport overflow', async ({page}) => {
  await page.setViewportSize({width: 640, height: 900});
  for (const route of routes) {
    await authorize(page, route.path);
    await page.evaluate(() => {
      const sheet = [...document.styleSheets].find(candidate => candidate.href?.endsWith('/static/app.css'));
      if (!sheet) throw new Error('local application stylesheet was not loaded');
      for (const rule of [
        'html { font-size: 200% !important; }',
        '* { letter-spacing: 0.12em !important; line-height: 1.5 !important; word-spacing: 0.16em !important; }',
        'p { margin-block-end: 2em !important; }',
      ]) {
        sheet.insertRule(rule, sheet.cssRules.length);
      }
    });

    await expect(page.getByRole('heading', {level: 1, name: route.heading, exact: true})).toBeVisible();
    const dimensions = await page.evaluate(() => ({
      viewport: document.documentElement.clientWidth,
      page: document.documentElement.scrollWidth,
    }));
    expect(dimensions.page, route.path).toBeLessThanOrEqual(dimensions.viewport);
  }
});

test('reduced motion disables transitions and smooth scrolling', async ({page}) => {
  await page.emulateMedia({reducedMotion: 'reduce'});
  await authorize(page, '/configuration');
  const styles = await page.getByRole('button', {name: 'Save structured settings'}).evaluate(element => {
    const computed = getComputedStyle(element);
    return {
      transitionDuration: computed.transitionDuration,
      animationDuration: computed.animationDuration,
      scrollBehavior: getComputedStyle(document.documentElement).scrollBehavior,
    };
  });
  expect(styles).toEqual({transitionDuration: '0s', animationDuration: '0s', scrollBehavior: 'auto'});
});

test('approved dark tokens and local system fonts are used exactly', async ({page}) => {
  const requestOrigins = new Set();
  page.on('request', request => requestOrigins.add(new URL(request.url()).origin));
  await authorize(page, '/');
  const theme = await page.evaluate(() => {
    const styles = getComputedStyle(document.documentElement);
    const names = [
      '--color-bg', '--color-surface-1', '--color-surface-2', '--color-surface-3',
      '--color-border', '--color-border-strong', '--color-text', '--color-muted',
      '--color-accent', '--color-accent-strong', '--color-focus', '--color-success', '--color-danger',
    ];
    return {
      colors: Object.fromEntries(names.map(name => [name, styles.getPropertyValue(name).trim()])),
      colorScheme: styles.colorScheme,
      fontFamily: styles.fontFamily,
    };
  });
  expect(theme.colors).toEqual({
    '--color-bg': '#090e15',
    '--color-surface-1': '#0d1420',
    '--color-surface-2': '#121b29',
    '--color-surface-3': '#182436',
    '--color-border': '#304159',
    '--color-border-strong': '#47607f',
    '--color-text': '#f4f7fb',
    '--color-muted': '#b1bfd2',
    '--color-accent': '#65dcc7',
    '--color-accent-strong': '#4fc6b1',
    '--color-focus': '#ffd166',
    '--color-success': '#83e7a3',
    '--color-danger': '#ff929a',
  });
  expect(theme.colorScheme).toBe('dark');
  expect(theme.fontFamily).toContain('system-ui');
  expect([...requestOrigins]).toEqual(['http://127.0.0.1:43127']);
});

test('enabled control boundaries have three-to-one contrast against fill and adjacent surface', async ({page}) => {
  for (const path of ['/issues', '/configuration']) {
    await authorize(page, path);
    const controls = page.locator('button:not(:disabled), input:not([type="hidden"]):not(:disabled), select:not(:disabled), textarea:not(:disabled)');
    for (let index = 0; index < await controls.count(); index += 1) {
      const control = controls.nth(index);
      if (!(await control.isVisible())) continue;
      for (const state of ['default', 'hover']) {
        if (state === 'hover') await control.hover();
        const sample = await control.evaluate(element => {
          const parseColor = value => value.match(/[\d.]+/g).slice(0, 3).map(Number);
          const luminance = value => {
            const channels = parseColor(value).map(channel => {
              const normalized = channel / 255;
              return normalized <= 0.04045 ? normalized / 12.92 : ((normalized + 0.055) / 1.055) ** 2.4;
            });
            return 0.2126 * channels[0] + 0.7152 * channels[1] + 0.0722 * channels[2];
          };
          const ratio = (first, second) => {
            const lighter = Math.max(luminance(first), luminance(second));
            const darker = Math.min(luminance(first), luminance(second));
            return (lighter + 0.05) / (darker + 0.05);
          };
          let adjacent = element.parentElement;
          while (adjacent && getComputedStyle(adjacent).backgroundColor === 'rgba(0, 0, 0, 0)') {
            adjacent = adjacent.parentElement;
          }
          const styles = getComputedStyle(element);
          const adjacentColor = adjacent ? getComputedStyle(adjacent).backgroundColor : getComputedStyle(document.body).backgroundColor;
          return {
            name: `${element.tagName.toLowerCase()}#${element.id || 'unnamed'}`,
            fillRatio: ratio(styles.borderTopColor, styles.backgroundColor),
            adjacentRatio: ratio(styles.borderTopColor, adjacentColor),
          };
        });
        expect(sample.fillRatio, `${path} ${sample.name} ${state} boundary versus fill`).toBeGreaterThanOrEqual(3);
        expect(sample.adjacentRatio, `${path} ${sample.name} ${state} boundary versus adjacent surface`).toBeGreaterThanOrEqual(3);
      }
    }
  }
});

test('forced colors retains current state and visible control boundaries', async ({page, browserName}) => {
  test.skip(browserName !== 'chromium', 'Playwright forced-colors emulation is Chromium-only.');
  await page.emulateMedia({forcedColors: 'active'});
  await authorize(page, '/configuration');
  const current = page.getByRole('link', {name: 'Configuration', exact: true});
  await expect(current).toHaveAttribute('aria-current', 'page');
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', {name: 'Skip to main content'})).toHaveCSS('outline-style', 'solid');
  await expect(page.getByRole('button', {name: 'Save structured settings'})).toHaveCSS('border-style', 'solid');
  await expect(page.getByLabel('Provider', {exact: true})).toHaveCSS('appearance', 'auto');
});
