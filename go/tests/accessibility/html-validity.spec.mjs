import {HtmlValidate} from 'html-validate';
import {writeFile} from 'node:fs/promises';
import {
  test,
  expect,
  authorize,
  routes,
  resetWorkflow,
  manualWorkflowPath,
  validGitHubWorkflow,
  environmentManagedWorkflow,
  scenarioPath,
  scenarioManifest,
  scenarioCases,
} from './fixtures.mjs';

const validator = new HtmlValidate({
  extends: ['html-validate:recommended'],
  rules: {
    'attribute-boolean-style': 'off',
    'attribute-empty-style': 'off',
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

test('operator request scenario renders valid HTML', async ({page}) => {
  await authorize(page, scenarioPath('/', 'live-operator-requests'));
  await expectValidHTML(page);
});

test('every fixed queue scenario renders valid HTML', async ({page}) => {
  expect(scenarioCases.map(({scenario}) => scenario)).toEqual(scenarioManifest);
  for (const scenario of scenarioCases) {
    const response = await page.goto(scenario.path);
    expect(response?.status(), scenario.scenario).toBe(scenario.status ?? 200);
    await expectValidHTML(page);
  }
});

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

test('invalid and conflict configuration states render valid HTML', async ({page}) => {
  await resetWorkflow();
  await authorize(page, '/configuration');
  await page.getByLabel('Complete WORKFLOW.md').fill('---\ntracker: []\n---\nRetained input');
  const invalidResponse = await Promise.all([
    page.waitForResponse(response => new URL(response.url()).pathname === '/api/v1/config/save'),
    page.getByRole('button', {name: 'Save complete workflow'}).click(),
  ]).then(([response]) => response);
  expect(invalidResponse.status()).toBe(422);
  await expect(page.locator('#error-summary')).toBeVisible();
  await expectValidHTML(page);

  await resetWorkflow();
  await page.reload();
  await page.getByLabel('Complete WORKFLOW.md').fill(validGitHubWorkflow.replace('repository: symphony', 'repository: unsaved'));
  await writeFile(manualWorkflowPath, validGitHubWorkflow.replace('repository: symphony', 'repository: external'), {mode: 0o600});
  const conflictResponse = await Promise.all([
    page.waitForResponse(response => new URL(response.url()).pathname === '/api/v1/config/save'),
    page.getByRole('button', {name: 'Save complete workflow'}).click(),
  ]).then(([response]) => response);
  expect(conflictResponse.status()).toBe(409);
  await expect(page.getByRole('heading', {name: 'Current workflow on disk'})).toBeVisible();
  await expectValidHTML(page);
});

test('success stored environment-managed and delete-confirmation states render valid HTML', async ({page}) => {
  await resetWorkflow();
  await authorize(page, '/configuration');
  await page.getByLabel(/New github credential/).fill('validity-credential-canary');
  await Promise.all([
    page.waitForURL(url => url.searchParams.get('result') === 'credential-stored'),
    page.getByRole('button', {name: 'Replace credential'}).click(),
  ]);
  await expect(page.getByRole('status')).toHaveText('Credential stored.');
  await expectValidHTML(page);

  await page.getByRole('button', {name: 'Delete credential'}).click();
  await expect(page.getByRole('dialog', {name: 'Delete credential?'})).toBeVisible();
  await expectValidHTML(page);
  await page.keyboard.press('Escape');

  await page.getByLabel('Complete WORKFLOW.md').fill(environmentManagedWorkflow);
  await Promise.all([
    page.waitForURL(url => url.searchParams.get('result')?.startsWith('configuration-saved')),
    page.getByRole('button', {name: 'Save complete workflow'}).click(),
  ]);
  await expect(page.getByText('Current state:')).toContainText('Environment managed');
  await expectValidHTML(page);

  await resetWorkflow();
  await page.reload();
  await page.getByRole('button', {name: 'Delete credential'}).click();
  await Promise.all([
    page.waitForURL(url => url.searchParams.get('result') === 'credential-deleted'),
    page.getByRole('button', {name: 'Delete credential', exact: true}).last().click(),
  ]);
  await expect(page.getByRole('status')).toHaveText('Credential deleted.');
});
