import AxeBuilder from '@axe-core/playwright';
import {writeFile} from 'node:fs/promises';
import {
  test,
  expect,
  authorize,
  resetWorkflow,
  manualWorkflowPath,
  validGitHubWorkflow,
  environmentManagedWorkflow,
} from './fixtures.mjs';

async function expectNoAxeViolations(page) {
  const results = await new AxeBuilder({page})
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'])
    .analyze();
  expect(results.violations.map(({id, impact, nodes}) => ({
    id,
    impact,
    targets: nodes.map(node => node.target),
  }))).toEqual([]);
}

test.beforeEach(async ({page}) => {
  await resetWorkflow();
  await authorize(page, '/configuration');
});

test('pristine configuration exposes complete labelled forms and no axe violations', async ({page}) => {
  await expect(page.locator('form')).toHaveCount(3);
  await expect(page.getByLabel('Complete WORKFLOW.md')).toHaveValue(validGitHubWorkflow);
  await expect(page.getByLabel('Owner')).toHaveValue('coryj627');
  await expect(page.getByLabel('Repository')).toHaveValue('symphony');
  await expect(page.getByText('Selected scope:')).toContainText('github:coryj627/symphony');
  await expect(page.getByText('Current state:')).toContainText('Not configured');
  await expect(page.getByLabel(/New github credential/)).toHaveAttribute('type', 'password');
  await expect(page.getByLabel(/New github credential/)).not.toHaveAttribute('value');
  await expectNoAxeViolations(page);
});

test('invalid raw workflow focuses linked summary and retains submitted bytes', async ({page}) => {
  const invalid = '---\ntracker: []\n---\nRetained browser input';
  await page.getByLabel('Complete WORKFLOW.md').fill(invalid);
  const response = await Promise.all([
    page.waitForResponse(res => res.url().endsWith('/api/v1/config/save')),
    page.getByRole('button', {name: 'Save complete workflow'}).click(),
  ]).then(([res]) => res);
  expect(response.status()).toBe(422);
  const summary = page.locator('#error-summary');
  await expect(summary).toBeFocused();
  await expect(summary.getByRole('link')).toHaveAttribute('href', '#raw-source');
  await expect(page.getByLabel('Complete WORKFLOW.md')).toHaveValue(invalid);
  await expectNoAxeViolations(page);
});

test('conflict retains unsaved source and shows freshly read disk source', async ({page}) => {
  const unsaved = validGitHubWorkflow.replace('repository: symphony', 'repository: operator-unsaved');
  const external = validGitHubWorkflow.replace('repository: symphony', 'repository: externally-edited');
  await page.getByLabel('Complete WORKFLOW.md').fill(unsaved);
  await writeFile(manualWorkflowPath, external, {mode: 0o600});
  const response = await Promise.all([
    page.waitForResponse(res => res.url().endsWith('/api/v1/config/save')),
    page.getByRole('button', {name: 'Save complete workflow'}).click(),
  ]).then(([res]) => res);
  expect(response.status()).toBe(409);
  await expect(page.locator('#error-summary')).toBeFocused();
  await expect(page.getByLabel('Complete WORKFLOW.md')).toHaveValue(unsaved);
  await expect(page.getByRole('heading', {name: 'Current workflow on disk'}).locator('..')).toContainText('externally-edited');
  await expectNoAxeViolations(page);
});

test('credential success announces once, restores focus, and leaves no canary artifact', async ({page}) => {
  const canary = 'browser-credential-canary';
  await page.getByLabel(/New github credential/).fill(canary);
  await page.getByRole('button', {name: 'Replace credential'}).click();
  await expect(page).toHaveURL(/result=credential-stored/);
  await expect(page.getByRole('status')).toHaveCount(1);
  await expect(page.getByRole('status')).toHaveText('Credential stored.');
  await expect(page.getByRole('button', {name: 'Replace credential'})).toBeFocused();
  await expect(page.getByText('Current state:')).toContainText('Stored in macOS Keychain');
  expect(await page.content()).not.toContain(canary);
  await expectNoAxeViolations(page);

  await page.getByRole('button', {name: 'Delete credential'}).click();
  await page.getByRole('button', {name: 'Delete credential', exact: true}).last().click();
  await expect(page.getByRole('status')).toHaveText('Credential deleted.');
});

test('environment-managed credential is labelled and vault actions are unavailable', async ({page}) => {
  await page.getByLabel('Complete WORKFLOW.md').fill(environmentManagedWorkflow);
  await page.getByRole('button', {name: 'Save complete workflow'}).click();
  await expect(page.getByRole('status')).toContainText('Configuration saved');
  await expect(page.getByText('Current state:')).toContainText('Environment managed');
  await expect(page.getByRole('button', {name: 'Replace credential'})).toBeDisabled();
  await expect(page.getByRole('button', {name: 'Delete credential'})).toBeDisabled();
  await expectNoAxeViolations(page);
});

test('delete dialog contains focus and restores it after Escape and Cancel', async ({page}) => {
  const deleteButton = page.getByRole('button', {name: 'Delete credential'});
  await deleteButton.click();
  const dialog = page.getByRole('dialog', {name: 'Delete credential?'});
  await expect(dialog).toBeVisible();
  await expect(page.getByRole('button', {name: 'Cancel'})).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(deleteButton).toBeFocused();

  await deleteButton.click();
  await page.getByRole('button', {name: 'Cancel'}).click();
  await expect(dialog).toBeHidden();
  await expect(deleteButton).toBeFocused();
});

test('validation save and named confirmation work without application JavaScript', async ({page}) => {
  await page.route('**/static/app.js', route => route.abort());
  await page.reload();
  const invalid = '---\ntracker: []\n---\nNo JavaScript input';
  await page.getByLabel('Complete WORKFLOW.md').fill(invalid);
  await page.getByRole('button', {name: 'Validate complete workflow'}).click();
  await expect(page.locator('#error-summary')).toBeVisible();
  await expect(page.getByLabel('Complete WORKFLOW.md')).toHaveValue(invalid);

  await resetWorkflow();
  await page.reload();
  await page.getByRole('button', {name: 'Delete credential'}).click();
  const dialog = page.getByRole('dialog', {name: 'Delete credential?'});
  await expect(dialog).toHaveAttribute('open', '');
  await expect(dialog.getByRole('button', {name: 'Delete credential'})).toBeVisible();
  await expectNoAxeViolations(page);
});
