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

for (const route of routes) {
  test(`${route.heading} renders valid HTML`, async ({page}) => {
    await authorize(page, route.path);
    const report = await validator.validateString(await page.content());
    expect(report.valid, report.results.flatMap(result => result.messages).map(message => `${message.ruleId} at ${message.line}:${message.column}`).join('\n')).toBe(true);
  });
}
