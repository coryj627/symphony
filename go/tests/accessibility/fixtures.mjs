import {test as base, expect} from '@playwright/test';
import {writeFile} from 'node:fs/promises';
import {fileURLToPath} from 'node:url';
import path from 'node:path';

const bootstrapToken = '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';
const sessionEnvironmentKey = 'SYMPHONY_E2E_SESSION_COOKIE';
export const manualWorkflowPath = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../testdata/manual/WORKFLOW.md');

export const validGitHubWorkflow = `---
tracker:
  kind: github
  provider:
    owner: coryj627
    repository: symphony
    credential_ref: os-vault
  required_labels: [symphony]
  active_states: [open]
  terminal_states: [closed]
workspace:
  root: .symphony/workspaces
server:
  port: 43127
---
Work on {{ issue.identifier }}.
`;

export const environmentManagedWorkflow = validGitHubWorkflow.replace('credential_ref: os-vault', 'credential_ref: $GITHUB_TOKEN');

export const scenarioManifest = [
  'empty',
  'populated',
  'stale-error',
  'filtered-empty',
  'issue-not-found',
  'malicious-text',
  'encoded-identifier',
  'degraded-log',
  'long-log',
];

export function scenarioPath(pathname, scenario) {
  const target = new URL(pathname, 'http://127.0.0.1:43127');
  target.searchParams.set('__e2e_scenario', scenario);
  return `${target.pathname}${target.search}${target.hash}`;
}

export const scenarioCases = [
  {scenario: 'empty', path: scenarioPath('/', 'empty')},
  {scenario: 'populated', path: scenarioPath('/issues', 'populated')},
  {scenario: 'stale-error', path: scenarioPath('/', 'stale-error')},
  {scenario: 'filtered-empty', path: `${scenarioPath('/issues', 'filtered-empty')}&state=Open`},
  {scenario: 'issue-not-found', path: scenarioPath('/issues/MISSING-1', 'issue-not-found'), status: 404},
  {scenario: 'malicious-text', path: scenarioPath('/issues/MAL-1', 'malicious-text')},
  {scenario: 'encoded-identifier', path: scenarioPath('/issues/TEAM%2F%2342', 'encoded-identifier')},
  {scenario: 'degraded-log', path: scenarioPath('/logs', 'degraded-log')},
  {scenario: 'long-log', path: scenarioPath('/logs', 'long-log')},
];

export const validLinearWorkflow = `---
tracker:
  kind: linear
  provider:
    project_slug: symphony-project
    credential_ref: os-vault
  required_labels: [symphony]
  active_states: [Todo]
  terminal_states: [Done]
workspace:
  root: .symphony/workspaces
server:
  port: 43127
---
Work on {{ issue.identifier }}.
`;

export async function resetWorkflow() {
  await writeFile(manualWorkflowPath, validGitHubWorkflow, {mode: 0o600});
}

export const routes = [
  {path: scenarioPath('/', 'empty'), title: 'Overview — Symphony', heading: 'Overview'},
  {path: scenarioPath('/issues', 'populated'), title: 'Issues — Symphony', heading: 'Issues'},
  {path: scenarioPath('/issues/SYM-123', 'populated'), title: 'SYM-123 — Symphony', heading: 'Issue SYM-123'},
  {path: scenarioPath('/activity', 'populated'), title: 'Activity — Symphony', heading: 'Activity'},
  {path: scenarioPath('/configuration', 'empty'), title: 'Configuration — Symphony', heading: 'Configuration'},
  {path: scenarioPath('/logs', 'populated'), title: 'Logs — Symphony', heading: 'Logs'},
];

export const navigationLabels = ['Overview', 'Issues', 'Activity', 'Configuration', 'Logs'];

export default async function globalSetup() {
  await resetWorkflow();
  let response;
  try {
    response = await fetch(`http://127.0.0.1:43127/?access_token=${bootstrapToken}`, {redirect: 'manual'});
  } catch {
    throw new Error('e2e bootstrap request failed');
  }
  if (response.status !== 303) {
    throw new Error('e2e bootstrap exchange was rejected');
  }
  const sessionCookie = response.headers.getSetCookie()
    .map(header => /^symphony_session=([^;]+);/.exec(header)?.[1])
    .find(Boolean);
  if (!sessionCookie) {
    throw new Error('e2e bootstrap response did not establish a session');
  }
  process.env[sessionEnvironmentKey] = sessionCookie;
  return async () => {
    delete process.env[sessionEnvironmentKey];
    await resetWorkflow();
  };
}

export const test = base.extend({
  authenticatedContext: [async ({playwright, browserName}, use) => {
    const sessionCookie = process.env[sessionEnvironmentKey];
    delete process.env[sessionEnvironmentKey];
    if (!sessionCookie) {
      throw new Error('e2e session is unavailable');
    }
    const browser = await playwright[browserName].launch();
    const context = await browser.newContext();
    await context.addCookies([{
      name: 'symphony_session',
      value: sessionCookie,
      domain: '127.0.0.1',
      path: '/',
      httpOnly: true,
      sameSite: 'Strict',
    }]);
    try {
      await use(context);
    } finally {
      await context.close();
      await browser.close();
    }
  }, {scope: 'worker'}],
  page: async ({authenticatedContext}, use) => {
    const page = await authenticatedContext.newPage();
    await use(page);
    await page.close();
  },
});

export {expect};

export async function authorize(page, path) {
  const response = await page.goto(path);
  expect(response?.ok()).toBeTruthy();
}
