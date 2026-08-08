import AxeBuilder from '@axe-core/playwright';
import {readFile, writeFile} from 'node:fs/promises';
import {
  test,
  expect,
  authorize,
  resetWorkflow,
  manualWorkflowPath,
  validGitHubWorkflow,
  validLinearWorkflow,
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

async function expectFocusedAndUnobscured(page, locator) {
  await expect(locator).toBeFocused();
  await expect(locator).toBeInViewport();
  const unobscured = await locator.evaluate(element => {
    return [...element.getClientRects()].some(rectangle => {
      const x = rectangle.left + rectangle.width / 2;
      const y = rectangle.top + rectangle.height / 2;
      const hit = document.elementFromPoint(x, y);
      return rectangle.width > 0 && rectangle.height > 0 && hit !== null && (hit === element || element.contains(hit));
    });
  });
  expect(unobscured).toBe(true);
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

test('Linear workflow renders its actual provider state', async ({page}) => {
  await writeFile(manualWorkflowPath, validLinearWorkflow, {mode: 0o600});
  await page.reload();
  await expect(page.getByLabel('Provider', {exact: true})).toHaveValue('linear');
  await expect(page.getByLabel('Project slug')).toHaveValue('symphony-project');
  await expect(page.getByText('Selected scope:')).toContainText('linear:symphony-project');
  await expect(page.getByLabel(/New linear credential/)).toHaveAttribute('type', 'password');
  await expectNoAxeViolations(page);
});

test('successful validation retains unsaved source and does not write it', async ({page}) => {
  const unsaved = validGitHubWorkflow.replace('repository: symphony', 'repository: validated-unsaved');
  await page.getByLabel('Complete WORKFLOW.md').fill(unsaved);
  const response = await Promise.all([
    page.waitForResponse(result => result.url().endsWith('/api/v1/config/validate')),
    page.getByRole('button', {name: 'Validate complete workflow'}).click(),
  ]).then(([result]) => result);
  expect(response.status()).toBe(200);
  await expect(page.getByRole('status')).toHaveText('Workflow source is valid. No changes were saved.');
  await expect(page.getByLabel('Complete WORKFLOW.md')).toHaveValue(unsaved);
  expect(await readFile(manualWorkflowPath, 'utf8')).toBe(validGitHubWorkflow);
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
  await expect(page.getByRole('link', {name: 'Cancel'})).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(deleteButton).toBeFocused();

  await deleteButton.click();
  await page.getByRole('link', {name: 'Cancel'}).click();
  await expect(dialog).toBeHidden();
  await expect(deleteButton).toBeFocused();
});

test('delete failure keeps one focused reachable summary inside the modal', async ({page}) => {
  await writeFile(manualWorkflowPath, validLinearWorkflow, {mode: 0o600});
  await page.reload();
  await page.getByLabel(/New linear credential/).fill('linear-delete-failure-canary');
  await page.getByRole('button', {name: 'Replace credential'}).click();
  await page.getByRole('button', {name: 'Delete credential'}).click();
  const dialog = page.getByRole('dialog', {name: 'Delete credential?'});
  await dialog.getByRole('button', {name: 'Delete credential'}).click();
  const summary = dialog.locator('#error-summary');
  await expect(summary).toHaveCount(1);
  await expectFocusedAndUnobscured(page, summary);
  await expect(summary.getByRole('link')).toHaveAttribute('href', '#credential-delete-confirm');
  expect(await page.content()).not.toContain('linear-delete-failure-canary');
  await expectNoAxeViolations(page);

  const summaryLink = summary.getByRole('link');
  const cancel = dialog.getByRole('link', {name: 'Cancel'});
  const confirm = dialog.getByRole('button', {name: 'Delete credential'});
  await page.keyboard.press('Tab');
  await expectFocusedAndUnobscured(page, summaryLink);
  await page.keyboard.press('Tab');
  await expectFocusedAndUnobscured(page, cancel);
  await page.keyboard.press('Tab');
  await expectFocusedAndUnobscured(page, confirm);
  await page.keyboard.press('Tab');
  await expectFocusedAndUnobscured(page, summaryLink);
  await page.keyboard.press('Shift+Tab');
  await expectFocusedAndUnobscured(page, confirm);
});

test('validation save cancel and confirmed deletion work without application JavaScript', async ({page}) => {
  await page.route('**/static/app.js', route => route.abort());
  await page.reload();
  const invalid = '---\ntracker: []\n---\nNo JavaScript input';
  await page.getByLabel('Complete WORKFLOW.md').fill(invalid);
  await page.getByRole('button', {name: 'Validate complete workflow'}).click();
  await expect(page.locator('#error-summary')).toBeVisible();
  await expect(page.getByLabel('Complete WORKFLOW.md')).toHaveValue(invalid);

  await resetWorkflow();
  await page.reload();
  const saved = validGitHubWorkflow.replace('repository: symphony', 'repository: no-js-saved');
  await page.getByLabel('Complete WORKFLOW.md').fill(saved);
  await page.getByRole('button', {name: 'Save complete workflow'}).click();
  await expect(page.getByRole('status')).toContainText('Configuration saved');
  await expect(page.getByLabel('Repository')).toHaveValue('no-js-saved');

  await page.getByRole('button', {name: 'Delete credential'}).click();
  let dialog = page.getByRole('dialog', {name: 'Delete credential?'});
  await expect(dialog).toHaveAttribute('open', '');
  await expect(dialog.getByRole('button', {name: 'Delete credential'})).toBeVisible();
  await dialog.getByRole('link', {name: 'Cancel'}).click();
  await expect(page).toHaveURL(/\/configuration#delete-credential$/);
  await expect(page.locator('#credential-delete-dialog')).not.toHaveAttribute('open', '');

  await page.getByRole('button', {name: 'Delete credential'}).click();
  dialog = page.getByRole('dialog', {name: 'Delete credential?'});
  await dialog.getByRole('button', {name: 'Delete credential'}).click();
  await expect(page.getByRole('status')).toHaveText('Credential deleted.');
  await expectNoAxeViolations(page);
});
