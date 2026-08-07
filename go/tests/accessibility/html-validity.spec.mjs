import {HtmlValidate} from 'html-validate';
import {test, expect, authorize, routes} from './fixtures.mjs';

const validator = new HtmlValidate({
  extends: ['html-validate:recommended'],
  rules: {
    'attribute-boolean-style': 'off',
    'no-trailing-whitespace': 'off',
    'no-inline-style': 'error',
  },
});

async function expectValidHTML(page) {
  const report = await validator.validateString(await page.content());
  expect(report.valid, report.results.flatMap(result => result.messages).map(message => `${message.ruleId} at ${message.line}:${message.column}`).join('\n')).toBe(true);
}

for (const route of routes) {
  test(`${route.heading} renders valid HTML`, async ({page}) => {
    await authorize(page, route.path);
    await expectValidHTML(page);
  });
}

test('missing-session authorization document renders valid HTML', async ({authenticatedContext}) => {
  const context = await authenticatedContext.browser().newContext();
  const page = await context.newPage();
  try {
    const response = await page.goto('/');
    expect(response?.status()).toBe(401);
    await expectValidHTML(page);
  } finally {
    await context.close();
  }
});

test('authenticated not-found document renders valid HTML', async ({page}) => {
  const response = await page.goto('/not-a-page');
  expect(response?.status()).toBe(404);
  await expectValidHTML(page);
});
