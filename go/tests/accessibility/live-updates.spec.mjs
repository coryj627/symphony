import AxeBuilder from '@axe-core/playwright';
import {test, expect, authorize, scenarioPath} from './fixtures.mjs';

test.describe.configure({mode: 'serial', retries: 0});

async function expectNoAxeViolations(page) {
  const results = await new AxeBuilder({page})
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'])
    .analyze();
  expect(results.violations.map(({id, nodes}) => ({id, targets: nodes.map(node => node.target)}))).toEqual([]);
}

async function csrfForScenario(page, scenario) {
  await authorize(page, scenarioPath('/', scenario));
  await page.evaluate(() => sessionStorage.removeItem('symphony.live-updates.paused'));
  await page.reload();
  return page.locator('input[name="csrf_token"]').inputValue();
}

async function requestRefresh(page, scenario, csrf) {
  return page.evaluate(async ({target, token}) => {
    const response = await fetch(target, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': token},
      body: '{}',
    });
    return response.status;
  }, {target: scenarioPath('/api/v1/refresh', scenario), token: csrf});
}

async function installFakeEventSource(page) {
  await page.addInitScript(() => {
    class ControlledEventSource {
      constructor(url) {
        this.url = url;
        this.closed = false;
        this.listeners = new Map();
        window.__controlledEventSources.push(this);
      }

      addEventListener(type, listener) {
        const listeners = this.listeners.get(type) ?? [];
        listeners.push(listener);
        this.listeners.set(type, listeners);
      }

      close() {
        this.closed = true;
      }

      emit(type, {data = '{}', lastEventId = ''} = {}) {
        for (const listener of this.listeners.get(type) ?? []) listener({data, lastEventId});
      }
    }
    window.__controlledEventSources = [];
    Object.defineProperty(window, 'EventSource', {configurable: true, value: ControlledEventSource});
  });
}

async function installHeldStateFetch(page) {
  await page.addInitScript(() => {
    const nativeFetch = window.fetch.bind(window);
    let intercepted = false;
    let rejectHeld = () => {};
    window.__heldStateRequestStarted = false;
    window.__rejectHeldStateRequest = () => rejectHeld();
    window.fetch = (input, init) => {
      const raw = input instanceof Request ? input.url : String(input);
      const target = new URL(raw, window.location.href);
      if (!intercepted && target.pathname === '/api/v1/state') {
        intercepted = true;
        window.__heldStateRequestStarted = true;
        return new Promise((_resolve, reject) => {
          rejectHeld = () => {
            rejectHeld = () => {};
            reject(new TypeError('controlled late state failure'));
          };
        });
      }
      return nativeFetch(input, init);
    };
  });
}

function issueSnapshot(cursor, candidates = []) {
  return {event_cursor: cursor, candidates};
}

function advanceCursor(raw, amount = 1n) {
  const separator = raw.lastIndexOf(':');
  if (separator < 1) throw new Error(`invalid test cursor: ${raw}`);
  return `${raw.slice(0, separator)}:${BigInt(raw.slice(separator + 1)) + amount}`;
}

function candidate(issueID, identifier, title) {
  return {issue_id: issueID, identifier, title, state: 'Open', routable: true, stale: false, routing_reasons: []};
}

const updatedCandidate = {
  issue_id: 'controlled-one',
  identifier: 'CONTROL-1',
  title: 'Recovered snapshot',
  state: 'Open',
  routable: true,
  stale: false,
  routing_reasons: [],
};

test('real SSE updates issue rows while preserving the focused search node', async ({page}) => {
  const csrf = await csrfForScenario(page, 'live-focus');
  const overviewPage = await page.context().newPage();
  await authorize(overviewPage, scenarioPath('/', 'live-focus'));
  await expect(overviewPage.locator('[data-live-count="candidates"]')).toHaveText('1');
  await expect(overviewPage.locator('[data-live-overview-field-container="tracker-scope"] dd')).toHaveText('fixture/live');
  await overviewPage.route('**/api/v1/state**', async route => {
    const response = await route.fetch();
    const snapshot = await response.json();
    snapshot.counts.errors = 2;
    snapshot.scheduler = {
      ...snapshot.scheduler,
      available: true,
      enabled: true,
      state: 'running',
      message: 'Scheduler is active.',
    };
    snapshot.tracker = {
      ...snapshot.tracker,
      scope: 'fixture/updated',
      state: 'failed',
      stale: true,
      has_error: true,
      error_code: 'tracker_transport',
      message: 'Tracker refresh failed.',
      last_attempt_at: '2026-08-09T12:01:00Z',
    };
    snapshot.config = {
      ...snapshot.config,
      state: 'invalid',
      has_error: true,
      error_code: 'invalid_workflow',
      message: 'Configuration needs attention.',
    };
    await route.fulfill({response, contentType: 'application/json', body: JSON.stringify(snapshot)});
  });
  await authorize(page, `${scenarioPath('/issues', 'live-focus')}&credential=raw-query-canary&query=&query=ignored-canary`);
  expect(await page.locator('[data-live-issues-wide] > [data-live-issue-id]').evaluateAll(rows => rows.map(row => row.dataset.liveIssueId))).toEqual(['live-1']);
  expect(await page.locator('[data-live-issues-narrow] > [data-live-issue-id]').evaluateAll(rows => rows.map(row => row.dataset.liveIssueId))).toEqual(['live-1']);
  await expect(page.getByText('Before refresh', {exact: true}).first()).toBeVisible();
  await expect(page.getByText('After refresh', {exact: true})).toHaveCount(0);
  const search = page.getByLabel('Search issues');
  await search.focus();
  const originalSearch = await search.elementHandle();
  const beforeScroll = await page.evaluate(() => ({x: scrollX, y: scrollY}));

  expect(await requestRefresh(page, 'live-focus', csrf)).toBe(202);

  await expect(page.getByText('After refresh', {exact: true}).first()).toBeVisible();
  await expect(page.getByRole('link', {name: 'LIVE-2'}).first()).toBeVisible();
  const liveHrefs = await page.locator('[data-live-issue-id][href]').evaluateAll(links => links.map(link => link.getAttribute('href')));
  expect(liveHrefs.every(href => !href.includes('raw-query-canary') && !href.includes('ignored-canary'))).toBe(true);
  await expect(search).toBeFocused();
  expect(await page.evaluate(node => node === document.querySelector('#issue-query'), originalSearch)).toBe(true);
  expect(await page.evaluate(() => ({x: scrollX, y: scrollY}))).toEqual(beforeScroll);
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page.getByRole('status')).toHaveCount(0);

  await expect(overviewPage.locator('[data-live-count="candidates"]')).toHaveText('2');
  await expect(overviewPage.locator('[data-live-overview-field="scheduler"]')).toHaveText('Enabled');
  await expect(overviewPage.locator('[data-live-overview-field-container="tracker-scope"] dd')).toHaveText('fixture/updated');
  await expect(overviewPage.locator('[data-live-count="errors"]')).toHaveText('2');
  await expect(overviewPage.locator('[data-live-overview-stale]')).toBeVisible();
  await expect(overviewPage.locator('[data-live-overview-error="config"]')).toContainText('Configuration needs attention.');
  await expect(overviewPage.locator('[data-live-overview-error="tracker"]')).toContainText('Tracker refresh failed.');
  await expect(overviewPage.getByRole('status')).toHaveCount(0);

  const cursorAfterInitial = await page.locator('[data-live-root]').getAttribute('data-event-cursor-id');
  const standardCandidates = [
    candidate('live-1', 'LIVE-1', 'After refresh'),
    candidate('live-2', 'LIVE-2', 'Second issue'),
  ];
  let stateCalls = 0;
  let releaseTrailing;
  const trailingResponse = new Promise(resolve => { releaseTrailing = resolve; });
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    if (stateCalls === 1) await trailingResponse;
    const cursor = advanceCursor(cursorAfterInitial, BigInt(stateCalls));
    const candidates = standardCandidates.map(item => ({...item, title: stateCalls === 1 ? 'Trailing snapshot' : 'Coalesced latest snapshot'}));
    await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify(issueSnapshot(cursor, candidates))});
  });
  expect(await requestRefresh(page, 'live-focus', csrf)).toBe(202);
  await expect.poll(() => stateCalls).toBe(1);
  expect(await requestRefresh(page, 'live-focus', csrf)).toBe(202);
  releaseTrailing();
  await expect(page.getByText('Coalesced latest snapshot', {exact: true}).first()).toBeVisible();
  expect(stateCalls).toBe(2);
  await page.unroute('**/api/v1/state**');

  const cursorAfterCoalescing = await page.locator('[data-live-root]').getAttribute('data-event-cursor-id');
  stateCalls = 0;
  let releaseCovering;
  const coveringResponse = new Promise(resolve => { releaseCovering = resolve; });
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    await coveringResponse;
    const covering = standardCandidates.map(item => ({...item, title: 'Snapshot already covered the latest event'}));
    await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify(issueSnapshot(advanceCursor(cursorAfterCoalescing, 2n), covering))});
  });
  expect(await requestRefresh(page, 'live-focus', csrf)).toBe(202);
  await expect.poll(() => stateCalls).toBe(1);
  expect(await requestRefresh(page, 'live-focus', csrf)).toBe(202);
  releaseCovering();
  await expect(page.getByText('Snapshot already covered the latest event', {exact: true}).first()).toBeVisible();
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  expect(stateCalls).toBe(1);
  await page.unroute('**/api/v1/state**');

  const cursorBeforeStale = await page.locator('[data-live-root]').getAttribute('data-event-cursor-id');
  stateCalls = 0;
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    const cursor = stateCalls === 1
      ? advanceCursor(cursorBeforeStale, -1n)
      : stateCalls === 2 ? cursorBeforeStale : advanceCursor(cursorBeforeStale);
    const candidates = stateCalls === 3
      ? standardCandidates.map(item => ({...item, title: 'Caught up after stale snapshots'}))
      : standardCandidates;
    await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify(issueSnapshot(cursor, candidates))});
  });
  expect(await requestRefresh(page, 'live-focus', csrf)).toBe(202);
  await expect(page.getByText('Caught up after stale snapshots', {exact: true}).first()).toBeVisible({timeout: 5000});
  expect(stateCalls).toBe(3);
  await page.unroute('**/api/v1/state**');

  const cursorBeforeFailure = await page.locator('[data-live-root]').getAttribute('data-event-cursor-id');
  stateCalls = 0;
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    if (stateCalls === 1) {
      await route.fulfill({status: 503, contentType: 'application/json', body: '{}'});
      return;
    }
    const recovered = standardCandidates.map(item => ({...item, title: 'Recovered from rendered authority'}));
    await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify(issueSnapshot(advanceCursor(cursorBeforeFailure), recovered))});
  });
  expect(await requestRefresh(page, 'live-focus', csrf)).toBe(202);
  await expect(page.getByText('Recovered from rendered authority', {exact: true}).first()).toBeVisible({timeout: 5000});
  expect(stateCalls).toBe(2);
  await page.unroute('**/api/v1/state**');
  await expect(search).toBeFocused();
  await expectNoAxeViolations(page);
  await expectNoAxeViolations(overviewPage);
  await overviewPage.unroute('**/api/v1/state**');
  await overviewPage.close();
});

test('structural and focused-field changes wait for an explicit safe focus boundary', async ({page, browserName}) => {
  const csrf = await csrfForScenario(page, 'live-structural');
  const canonicalQuery = '?__e2e_scenario=live-structural&eligibility=routable&query=issue&sort=identifier&state=Open';
  await authorize(page, `${scenarioPath('/issues', 'live-structural')}&state=open&eligibility=routable&sort=identifier&query=issue`);
  expect(await page.locator('[data-live-issues-wide] > [data-live-issue-id]').evaluateAll(rows => rows.map(row => row.dataset.liveIssueId))).toEqual(['live-one', 'live-two']);
  expect(await page.locator('[data-live-issues-narrow] > [data-live-issue-id]').evaluateAll(rows => rows.map(row => row.dataset.liveIssueId))).toEqual(['live-one', 'live-two']);
  await expect(page.getByRole('link', {name: 'LIVE-1'}).first()).toBeVisible();
  const second = page.getByRole('link', {name: 'LIVE-2'}).first();
  await expect(second).toHaveAttribute('href', `/issues/LIVE-2${canonicalQuery}`);
  await second.focus();

  expect(await requestRefresh(page, 'live-structural', csrf)).toBe(202);
  await expect(page.getByRole('button', {name: 'Apply pending updates'})).toBeVisible();
  await expect(second).toBeFocused();
  await expect(page.getByRole('link', {name: 'LIVE-1'}).first()).toBeVisible();
  await expectNoAxeViolations(page);

  const apply = page.getByRole('button', {name: 'Apply pending updates'});
  await apply.focus();
  await expect(page.getByRole('link', {name: 'LIVE-1'}).first()).toBeVisible();
  const beforeApplyScroll = await page.evaluate(() => ({x: scrollX, y: scrollY}));
  await page.keyboard.press('Enter');
  await expect(apply).toBeFocused();
  await expect(apply).toBeVisible();
  expect(await page.evaluate(() => ({x: scrollX, y: scrollY}))).toEqual(beforeApplyScroll);
  await expect(page.getByRole('link', {name: 'LIVE-1'})).toHaveCount(0);

  const search = page.getByLabel('Search issues');
  await search.focus();
  await expect(apply).toBeHidden();

  const stableIssue = page.getByRole('link', {name: 'LIVE-2'}).first();
  await stableIssue.focus();
  expect(await requestRefresh(page, 'live-structural', csrf)).toBe(202);
  await expect(page.getByRole('button', {name: 'Apply pending updates'})).toBeVisible();
  await expect(stableIssue).toBeFocused();
  const logicalIssueLinks = page.locator('[data-live-issue-id="live-two"] [data-field="identifier"]');
  await expect(logicalIssueLinks).toHaveCount(2);
  await expect(logicalIssueLinks).toHaveText(['LIVE-2', 'LIVE-2']);
  const oldHrefs = await logicalIssueLinks.evaluateAll(links => links.map(link => {
    const target = new URL(link.href);
    return `${target.pathname}${target.search}`;
  }));
  expect(oldHrefs).toEqual([`/issues/LIVE-2${canonicalQuery}`, `/issues/LIVE-2${canonicalQuery}`]);
  const updatedIdentifier = 'TEAM:@&=+$!é';
  const pendingIdentifier = `${updatedIdentifier}-pending`;
  await expect(page.getByRole('link', {name: updatedIdentifier})).toHaveCount(0);
  let heldStateCalls = 0;
  let releaseNewerState;
  let releaseResumeState;
  let releasePrePauseState;
  const newerState = new Promise(resolve => { releaseNewerState = resolve; });
  const resumeState = new Promise(resolve => { releaseResumeState = resolve; });
  const prePauseState = new Promise(resolve => { releasePrePauseState = resolve; });
  await page.route('**/api/v1/state**', async route => {
    heldStateCalls += 1;
    const response = await route.fetch();
    if (heldStateCalls === 1) await newerState;
    if (heldStateCalls === 4) await resumeState;
    if (heldStateCalls === 5) await prePauseState;
    if (heldStateCalls === 3 || heldStateCalls === 7) {
      const snapshot = await response.json();
      snapshot.candidates = snapshot.candidates.map(item => item.issue_id === 'live-two'
        ? {...item, identifier: pendingIdentifier}
        : item);
      await route.fulfill({response, contentType: 'application/json', body: JSON.stringify(snapshot)}).catch(() => {});
      return;
    }
    await route.fulfill({response}).catch(() => {});
  });

  expect(await requestRefresh(page, 'live-structural', csrf)).toBe(202);
  await expect.poll(() => heldStateCalls).toBe(1);
  await search.focus();
  await expect(apply).toBeHidden();
  await expect(logicalIssueLinks).toHaveText(['LIVE-2', 'LIVE-2']);
  releaseNewerState();
  await expect(logicalIssueLinks).toHaveText([updatedIdentifier, updatedIdentifier]);
  const updatedHrefs = await logicalIssueLinks.evaluateAll(links => links.map(link => {
    const target = new URL(link.href);
    return `${target.pathname}${target.search}`;
  }));
  expect(updatedHrefs).toEqual([
    `/issues/TEAM:@&=+$%21%C3%A9${canonicalQuery}`,
    `/issues/TEAM:@&=+$%21%C3%A9${canonicalQuery}`,
  ]);

  const updatedWideLink = page.getByRole('link', {name: updatedIdentifier}).first();
  await updatedWideLink.focus();
  expect(await requestRefresh(page, 'live-structural', csrf)).toBe(202);
  await expect.poll(() => heldStateCalls).toBe(2);
  await expect(apply).toBeHidden();
  await expect(updatedWideLink).toBeFocused();
  await expect(updatedWideLink).toHaveAttribute('href', `/issues/TEAM:@&=+$%21%C3%A9${canonicalQuery}`);

  expect(await requestRefresh(page, 'live-structural', csrf)).toBe(202);
  await expect.poll(() => heldStateCalls).toBe(3);
  await expect(apply).toBeVisible();
  await expect(page.getByRole('link', {name: pendingIdentifier})).toHaveCount(0);
  const beforePendingPause = await page.locator('[data-live-presentation]').evaluate(node => node.innerHTML);
  await page.getByRole('button', {name: 'Pause live updates'}).focus();
  await page.keyboard.press('Enter');
  const heldResume = page.getByRole('button', {name: 'Resume live updates'});
  await expect(heldResume).toBeFocused();
  await expect(apply).toBeHidden();
  expect(await page.locator('[data-live-presentation]').evaluate(node => node.innerHTML)).toBe(beforePendingPause);
  await page.keyboard.press('Enter');
  await expect.poll(() => heldStateCalls).toBe(4);
  await expect(heldResume).toBeFocused();
  await expect(heldResume).toHaveText('Resume live updates');
  await expect(page.locator('[data-live-connection]')).toHaveText('Resuming live updates.');
  expect(await page.evaluate(() => sessionStorage.getItem('symphony.live-updates.paused'))).toBe('true');
  expect(await page.locator('[data-live-presentation]').evaluate(node => node.innerHTML)).toBe(beforePendingPause);
  releaseResumeState();
  await expect(page.getByRole('button', {name: 'Pause live updates'})).toBeFocused();
  expect(await page.evaluate(() => sessionStorage.getItem('symphony.live-updates.paused'))).toBeNull();
  await expect(logicalIssueLinks).toHaveText([updatedIdentifier, updatedIdentifier]);

  const beforeHeldPause = await page.locator('[data-live-presentation]').evaluate(node => node.innerHTML);
  expect(await requestRefresh(page, 'live-structural', csrf)).toBe(202);
  await expect.poll(() => heldStateCalls).toBe(5);
  await page.getByRole('button', {name: 'Pause live updates'}).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('button', {name: 'Resume live updates'})).toBeFocused();
  releasePrePauseState();
  expect(await page.locator('[data-live-presentation]').evaluate(node => node.innerHTML)).toBe(beforeHeldPause);
  await page.keyboard.press('Enter');
  await expect.poll(() => heldStateCalls).toBe(6);
  await expect(page.getByRole('button', {name: 'Pause live updates'})).toBeFocused();
  expect(await page.evaluate(() => sessionStorage.getItem('symphony.live-updates.paused'))).toBeNull();

  await page.setViewportSize({width: 320, height: 720});
  await page.emulateMedia({reducedMotion: 'reduce'});
  const updatedNarrowLink = page.getByRole('link', {name: updatedIdentifier}).last();
  await updatedNarrowLink.focus();
  expect(await requestRefresh(page, 'live-structural', csrf)).toBe(202);
  await expect.poll(() => heldStateCalls).toBe(7);
  await expect(apply).toBeVisible();
  await expect(page.getByRole('link', {name: pendingIdentifier})).toHaveCount(0);
  const pause = page.getByRole('button', {name: 'Pause live updates'});
  for (const control of [pause, apply]) {
    const box = await control.boundingBox();
    expect(box?.width).toBeGreaterThanOrEqual(44);
    expect(box?.height).toBeGreaterThanOrEqual(44);
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
  expect(await page.evaluate(() => getComputedStyle(document.documentElement).scrollBehavior)).toBe('auto');
  await expectNoAxeViolations(page);
  if (browserName === 'chromium') {
    await page.emulateMedia({reducedMotion: 'reduce', forcedColors: 'active'});
    await expectNoAxeViolations(page);
    await page.emulateMedia({reducedMotion: 'reduce', forcedColors: 'none'});
  }
  await pause.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('button', {name: 'Resume live updates'})).toBeVisible();
  await expectNoAxeViolations(page);
  await page.getByRole('button', {name: 'Resume live updates'}).focus();
  await page.keyboard.press('Enter');
  await expect.poll(() => heldStateCalls).toBe(8);
  await page.unroute('**/api/v1/state**');
  const navigationLink = page.getByRole('link', {name: updatedIdentifier}).last();
  await expect(navigationLink).toBeVisible();
  await navigationLink.focus();
  await expect(navigationLink).toBeFocused();
  const responsePromise = page.waitForResponse(response => response.request().isNavigationRequest());
  await page.keyboard.press('Enter');
  const response = await responsePromise;
  expect(response.status()).toBe(200);
  await expect(page.getByRole('heading', {level: 1, name: `Issue ${updatedIdentifier}`})).toBeVisible();
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', {name: 'Skip to main content'})).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.locator('#main-content')).toBeFocused();
});

test('pause freezes presentation across publication and persists until authoritative resume', async ({page}) => {
  const csrf = await csrfForScenario(page, 'live-pause');
  await authorize(page, scenarioPath('/issues', 'live-pause'));
  await expect(page.getByText('Before pause', {exact: true}).first()).toBeVisible();
  expect(await page.locator('[data-live-issues-wide] > [data-live-issue-id]').evaluateAll(rows => rows.map(row => row.dataset.liveIssueId))).toEqual(['live-1']);
  expect(await page.locator('[data-live-issues-narrow] > [data-live-issue-id]').evaluateAll(rows => rows.map(row => row.dataset.liveIssueId))).toEqual(['live-1']);
  const activityPage = await page.context().newPage();
  const overviewPage = await page.context().newPage();
  const navigationPage = await page.context().newPage();
  try {
    await authorize(activityPage, scenarioPath('/activity', 'live-pause'));
    await expect(activityPage.locator('[data-live-activity] > li')).toHaveCount(0);
    await expect(activityPage.locator('[data-live-activity-empty]')).toBeVisible();
    await authorize(overviewPage, scenarioPath('/', 'live-pause'));
    await authorize(navigationPage, scenarioPath('/issues', 'live-pause'));
    const issuesPresentation = page.locator('[data-live-presentation]');
    const activityPresentation = activityPage.locator('[data-live-presentation]');
    const overviewCounts = overviewPage.locator('[data-live-presentation]');
    const beforeIssues = await issuesPresentation.evaluate(node => node.innerHTML);
    const beforeActivity = await activityPresentation.evaluate(node => node.innerHTML);
    const beforeOverviewCounts = await overviewCounts.evaluate(node => node.innerHTML);

    await page.getByRole('button', {name: 'Pause live updates'}).focus();
    await page.keyboard.press('Enter');
    await activityPage.getByRole('button', {name: 'Pause live updates'}).focus();
    await activityPage.keyboard.press('Enter');
    await overviewPage.getByRole('button', {name: 'Pause live updates'}).focus();
    await overviewPage.keyboard.press('Enter');
    await navigationPage.getByRole('button', {name: 'Pause live updates'}).focus();
    await navigationPage.keyboard.press('Enter');
    await navigationPage.getByRole('link', {name: 'Activity'}).click();
    await expect(navigationPage.getByRole('button', {name: 'Resume live updates'})).toBeVisible();
    await expect(navigationPage.locator('[data-live-connection]')).toHaveText('Live updates are paused.');
    await expect(page.getByRole('button', {name: 'Resume live updates'})).toBeFocused();
    await expect(activityPage.getByRole('button', {name: 'Resume live updates'})).toBeFocused();
    await expectNoAxeViolations(page);
    await expectNoAxeViolations(activityPage);
    expect(await requestRefresh(page, 'live-pause', csrf)).toBe(202);
    expect(await issuesPresentation.evaluate(node => node.innerHTML)).toBe(beforeIssues);
    expect(await activityPresentation.evaluate(node => node.innerHTML)).toBe(beforeActivity);
    expect(await overviewCounts.evaluate(node => node.innerHTML)).toBe(beforeOverviewCounts);
    await expect(navigationPage.locator('[data-live-activity] > li')).toHaveCount(0);
    const activityLiveAncestors = await activityPage.locator('[data-live-connection], [data-live-activity], [data-live-activity] time').evaluateAll(nodes => nodes.map(node => Boolean(node.closest('[role="log"], [aria-live]'))));
    expect(activityLiveAncestors).toEqual([false, false]);

    await page.getByRole('button', {name: 'Resume live updates'}).focus();
    await page.keyboard.press('Enter');
    await activityPage.getByRole('button', {name: 'Resume live updates'}).focus();
    await activityPage.keyboard.press('Enter');
    await overviewPage.getByRole('button', {name: 'Resume live updates'}).focus();
    await overviewPage.keyboard.press('Enter');
    await expect(page.getByText('While paused', {exact: true}).first()).toBeVisible();
    await expect(activityPage.locator('[data-live-activity] > li')).toHaveCount(1);
    const resumedActivityLiveAncestors = await activityPage.locator('[data-live-connection], [data-live-activity], [data-live-activity] time').evaluateAll(nodes => nodes.map(node => Boolean(node.closest('[role="log"], [aria-live]'))));
    expect(resumedActivityLiveAncestors).toEqual([false, false, false]);

    expect(await requestRefresh(page, 'live-pause', csrf)).toBe(202);
    await expect(page.getByText('After resume', {exact: true}).first()).toBeVisible();
    await expect(page.getByRole('link', {name: 'LIVE-3'}).first()).toBeVisible();
    await expect(activityPage.locator('[data-live-activity] > li')).toHaveCount(2);

    let pageShowStateCalls = 0;
    let releasePageHideState;
    const pageHideState = new Promise(resolve => { releasePageHideState = resolve; });
    await page.route('**/api/v1/state**', async route => {
      pageShowStateCalls += 1;
      const response = await route.fetch();
      if (pageShowStateCalls === 1) await pageHideState;
      await route.fulfill({response}).catch(() => {});
    });
    expect(await requestRefresh(page, 'live-pause', csrf)).toBe(202);
    await expect.poll(() => pageShowStateCalls).toBe(1);
    await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pagehide', {persisted: true})));
    releasePageHideState();
    await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', {persisted: true})));
    await expect.poll(() => pageShowStateCalls).toBe(2);
    await expect(page.getByText('After resume', {exact: true}).first()).toBeVisible();
    await expect(page.getByRole('status')).toHaveCount(0);
    await page.unroute('**/api/v1/state**');
    await expectNoAxeViolations(page);
    await expectNoAxeViolations(activityPage);
    await expectNoAxeViolations(overviewPage);
    await expectNoAxeViolations(navigationPage);
  } finally {
    await activityPage.close();
    await overviewPage.close();
    await navigationPage.close();
  }
});

test('failed resume announces once on a later frame and keeps focus on the reusable control', async ({page}) => {
  await csrfForScenario(page, 'live-resume-failure');
  await expect(page.locator('[data-live-count="candidates"]')).toHaveText('1');
  await expect(page.locator('[data-live-count="routable"]')).toHaveText('1');
  const pause = page.getByRole('button', {name: 'Pause live updates'});
  await pause.focus();
  await page.keyboard.press('Enter');
  await page.getByRole('button', {name: 'Refresh tracker work'}).click();
  await expect(page.getByRole('status')).toHaveText('Refresh requested.');
  await expect(page.getByRole('status')).toHaveCount(1);
  await page.evaluate(() => {
    const target = document.querySelector('[data-live-feedback]');
    window.__liveFeedbackFrames = [];
    let frame = 0;
    const tick = () => {
      frame += 1;
      window.__liveFeedbackFrameRequest = requestAnimationFrame(tick);
    };
    window.__liveFeedbackFrameRequest = requestAnimationFrame(tick);
    window.__liveFeedbackObserver = new MutationObserver(() => {
      window.__liveFeedbackFrames.push({frame, role: target.getAttribute('role'), text: target.textContent});
    });
    window.__liveFeedbackObserver.observe(target, {attributes: true, childList: true, characterData: true, subtree: true});
  });

  const resume = page.getByRole('button', {name: 'Resume live updates'});
  await resume.focus();
  await page.keyboard.press('Enter');
  const status = page.getByRole('status');
  await expect(status).toHaveText('Live updates could not be resumed.');
  await expect(status).toHaveCount(1);
  await expect(resume).toBeFocused();
  const mutations = await page.evaluate(() => {
    cancelAnimationFrame(window.__liveFeedbackFrameRequest);
    window.__liveFeedbackObserver.disconnect();
    return window.__liveFeedbackFrames;
  });
  const roleFrame = mutations.find(entry => entry.role === 'status' && entry.text === '')?.frame;
  const textFrame = mutations.find(entry => entry.text === 'Live updates could not be resumed.')?.frame;
  expect(roleFrame).toBeDefined();
  expect(textFrame).toBeGreaterThan(roleFrame);
  await expectNoAxeViolations(page);
});

test('reset retries one failed snapshot and mirrors the exact server cursor grammar', async ({page}) => {
  await installFakeEventSource(page);
  await page.addInitScript(() => sessionStorage.removeItem('symphony.live-updates.paused'));
  let stateCalls = 0;
  const validCursor = 'epoch~allowed:18446744073709551615';
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    const status = stateCalls === 1 ? 503 : 200;
    const body = status === 200 ? issueSnapshot(validCursor) : {};
    await route.fulfill({status, contentType: 'application/json', body: JSON.stringify(body)});
  });
  await authorize(page, scenarioPath('/issues', 'empty'));
  const closedSynchronously = await page.evaluate(() => {
    const first = window.__controlledEventSources[0];
    first.emit('reset');
    return first.closed;
  });
  expect(closedSynchronously).toBe(true);
  await expect.poll(() => page.evaluate(() => window.__controlledEventSources.length), {timeout: 5000}).toBe(2);
  expect(stateCalls).toBe(2);
  const reconnectedURL = await page.evaluate(() => window.__controlledEventSources[1].url);
  expect(new URL(reconnectedURL, 'http://127.0.0.1').searchParams.get('after')).toBe(validCursor);

  const invalidCursors = [
    `${'e'.repeat(129)}:1`,
    'epoch:123456789012345678901',
    'epoch:18446744073709551616',
    'epoch:01',
    'bad/epoch:1',
  ];
  for (const invalid of invalidCursors) {
    const before = await page.evaluate(() => window.__controlledEventSources.length);
    const closed = await page.evaluate(cursor => {
      const current = window.__controlledEventSources.at(-1);
      current.emit('queue.refreshed', {lastEventId: cursor});
      return current.closed;
    }, invalid);
    expect(closed).toBe(true);
    await expect.poll(() => page.evaluate(() => window.__controlledEventSources.length)).toBe(before + 1);
  }
});

test('a clean reset closes the old source and reconciles once without focus or announcement churn', async ({page}) => {
  await installFakeEventSource(page);
  await page.addInitScript(() => sessionStorage.removeItem('symphony.live-updates.paused'));
  let stateCalls = 0;
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify(issueSnapshot('e2e:3', [updatedCandidate]))});
  });
  await authorize(page, scenarioPath('/issues', 'empty'));
  const search = page.getByLabel('Search issues');
  await search.focus();
  const originalSearch = await search.elementHandle();
  const beforeScroll = await page.evaluate(() => ({x: scrollX, y: scrollY}));
  const closedSynchronously = await page.evaluate(() => {
    const first = window.__controlledEventSources[0];
    first.emit('reset');
    return first.closed;
  });
  expect(closedSynchronously).toBe(true);
  await expect(page.getByText('Recovered snapshot', {exact: true}).first()).toBeVisible();
  expect(stateCalls).toBe(1);
  const sources = await page.evaluate(() => window.__controlledEventSources.map(source => ({closed: source.closed, url: source.url})));
  expect(sources).toHaveLength(2);
  expect(sources.filter(source => !source.closed)).toHaveLength(1);
  expect(new URL(sources[1].url, 'http://127.0.0.1').searchParams.get('after')).toBe('e2e:3');
  await expect(search).toBeFocused();
  expect(await page.evaluate(node => node === document.querySelector('#issue-query'), originalSearch)).toBe(true);
  expect(await page.evaluate(() => ({x: scrollX, y: scrollY}))).toEqual(beforeScroll);
  await expect(page.getByRole('status')).toHaveCount(0);
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expectNoAxeViolations(page);
});

test('failed reset invalidates an older pending snapshot without hiding a focused Apply control', async ({page}) => {
  await installFakeEventSource(page);
  await page.addInitScript(() => sessionStorage.removeItem('symphony.live-updates.paused'));
  let stateCalls = 0;
  let firstCursor;
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    if (stateCalls === 1) {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(issueSnapshot(firstCursor, [candidate('sym-124', 'SYM-124-C1', 'Pending C1')])),
      });
      return;
    }
    await route.fulfill({status: 503, contentType: 'application/json', body: '{}'});
  });
  await authorize(page, scenarioPath('/issues', 'populated'));
  const initialCursor = await page.locator('[data-live-root]').getAttribute('data-event-cursor-id');
  firstCursor = advanceCursor(initialCursor);
  const second = page.getByRole('link', {name: 'SYM-124'}).first();
  await second.focus();
  await page.evaluate(cursor => window.__controlledEventSources[0].emit('queue.refreshed', {lastEventId: cursor}), firstCursor);
  const apply = page.getByRole('button', {name: 'Apply pending updates'});
  await expect(apply).toBeVisible();
  await apply.focus();
  const closed = await page.evaluate(() => {
    const source = window.__controlledEventSources[0];
    source.emit('reset');
    return source.closed;
  });
  expect(closed).toBe(true);
  await expect.poll(() => stateCalls).toBe(2);
  await expect(apply).toBeFocused();
  await expect(apply).toBeVisible();
  await page.getByLabel('Search issues').focus();
  await expect(apply).toBeHidden();
  await expect(page.getByRole('link', {name: 'SYM-123'}).first()).toBeVisible();
  await expect(page.getByRole('link', {name: 'SYM-124-C1'})).toHaveCount(0);
  await expect(page.getByRole('status')).toHaveCount(0);
});

test('repeated error reset pause and resume transitions retain exactly one open source', async ({page}) => {
  await installFakeEventSource(page);
  await page.addInitScript(() => sessionStorage.removeItem('symphony.live-updates.paused'));
  let stateCalls = 0;
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(issueSnapshot(`e2e:${2 + stateCalls}`, [updatedCandidate])),
    });
  });
  await authorize(page, scenarioPath('/issues', 'empty'));
  await page.evaluate(() => window.__controlledEventSources[0].emit('error'));
  await expect(page.locator('[data-live-connection]')).toHaveText('Live updates are reconnecting.');
  await expectNoAxeViolations(page);
  await page.evaluate(() => window.__controlledEventSources[0].emit('reset'));
  await expect.poll(() => page.evaluate(() => window.__controlledEventSources.length)).toBe(2);
  await page.getByRole('button', {name: 'Pause live updates'}).focus();
  await page.keyboard.press('Enter');
  const resume = page.getByRole('button', {name: 'Resume live updates'});
  await resume.focus();
  await page.keyboard.press('Enter');
  await expect.poll(() => page.evaluate(() => window.__controlledEventSources.length)).toBe(3);
  await page.evaluate(() => window.__controlledEventSources.at(-1).emit('error'));
  await expect(page.locator('[data-live-connection]')).toHaveText('Live updates are reconnecting.');
  await expectNoAxeViolations(page);
  await page.evaluate(() => window.__controlledEventSources.at(-1).emit('reset'));
  await expect.poll(() => page.evaluate(() => window.__controlledEventSources.length)).toBe(4);
  const sourceState = await page.evaluate(() => window.__controlledEventSources.map(source => source.closed));
  expect(sourceState).toEqual([true, true, true, false]);
  expect(stateCalls).toBe(3);
  await expect(page.getByRole('button', {name: 'Pause live updates'})).toBeFocused();
  expect(await page.evaluate(() => sessionStorage.getItem('symphony.live-updates.paused'))).toBeNull();
  await expect(page.getByRole('status')).toHaveCount(0);
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('late generic failure from a superseded event fetch cannot overwrite reset authority', async ({page}) => {
  await installFakeEventSource(page);
  await installHeldStateFetch(page);
  await page.addInitScript(() => sessionStorage.removeItem('symphony.live-updates.paused'));
  let stateCalls = 0;
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify(issueSnapshot('e2e:4', [updatedCandidate]))});
  });
  await authorize(page, scenarioPath('/issues', 'empty'));
  await page.evaluate(() => window.__controlledEventSources[0].emit('queue.refreshed', {lastEventId: 'e2e:3'}));
  await expect.poll(() => page.evaluate(() => window.__heldStateRequestStarted)).toBe(true);
  const closed = await page.evaluate(() => {
    const current = window.__controlledEventSources[0];
    current.emit('reset');
    return current.closed;
  });
  expect(closed).toBe(true);
  await expect(page.getByText('Recovered snapshot', {exact: true}).first()).toBeVisible();
  await expect(page.locator('[data-live-connection]')).toHaveText('Live updates are connected.');
  await page.evaluate(() => window.__rejectHeldStateRequest());
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await expect(page.locator('[data-live-connection]')).toHaveText('Live updates are connected.');
  expect(stateCalls).toBe(1);
  expect(await page.evaluate(() => window.__controlledEventSources.length)).toBe(2);
});

test('pagehide fences a late generic event-fetch failure before authoritative pageshow', async ({page}) => {
  await installFakeEventSource(page);
  await installHeldStateFetch(page);
  await page.addInitScript(() => sessionStorage.removeItem('symphony.live-updates.paused'));
  let stateCalls = 0;
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify(issueSnapshot('e2e:4', [updatedCandidate]))});
  });
  await authorize(page, scenarioPath('/issues', 'empty'));
  await page.evaluate(() => window.__controlledEventSources[0].emit('queue.refreshed', {lastEventId: 'e2e:3'}));
  await expect.poll(() => page.evaluate(() => window.__heldStateRequestStarted)).toBe(true);
  await page.evaluate(async () => {
    window.dispatchEvent(new PageTransitionEvent('pagehide', {persisted: true}));
    window.__rejectHeldStateRequest();
    await new Promise(resolve => setTimeout(resolve, 0));
  });
  await expect(page.locator('[data-live-connection]')).toHaveText('Live updates are available.');
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', {persisted: true})));
  await expect(page.getByText('Recovered snapshot', {exact: true}).first()).toBeVisible();
  await expect(page.locator('[data-live-connection]')).toHaveText('Live updates are connected.');
  expect(stateCalls).toBe(1);
  expect(await page.evaluate(() => window.__controlledEventSources.length)).toBe(2);
  await expect(page.getByRole('status')).toHaveCount(0);
});

test('pagehide during Resume restores paused presentation without a stale failure announcement', async ({page}) => {
  await csrfForScenario(page, 'empty');
  let stateCalls = 0;
  let releaseState;
  const delayedState = new Promise(resolve => { releaseState = resolve; });
  await page.route('**/api/v1/state**', async route => {
    stateCalls += 1;
    await delayedState;
    await route.fulfill({status: 503, contentType: 'application/json', body: '{}'}).catch(() => {});
  });
  await authorize(page, scenarioPath('/issues', 'empty'));
  await page.getByRole('button', {name: 'Pause live updates'}).focus();
  await page.keyboard.press('Enter');
  await page.getByRole('button', {name: 'Resume live updates'}).focus();
  await page.keyboard.press('Enter');
  await expect.poll(() => stateCalls).toBe(1);
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pagehide', {persisted: true})));
  releaseState();
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', {persisted: true})));
  await expect(page.getByRole('button', {name: 'Resume live updates'})).toBeVisible();
  await expect(page.locator('[data-live-connection]')).toHaveText('Live updates are paused.');
  await expect(page.getByRole('status')).toHaveCount(0);
});

test('pagehide invalidates a queued resume-failure announcement before its first frame', async ({page}) => {
  await csrfForScenario(page, 'empty');
  await page.route('**/api/v1/state**', route => route.fulfill({status: 503, contentType: 'application/json', body: '{}'}));
  await page.getByRole('button', {name: 'Pause live updates'}).focus();
  await page.keyboard.press('Enter');
  await page.evaluate(() => {
    window.__nativeRequestAnimationFrame = window.requestAnimationFrame;
    window.__heldLiveFrames = [];
    window.requestAnimationFrame = callback => {
      window.__heldLiveFrames.push(callback);
      return 10000 + window.__heldLiveFrames.length;
    };
  });
  await page.getByRole('button', {name: 'Resume live updates'}).focus();
  await page.keyboard.press('Enter');
  await expect(page.locator('[data-live-connection]')).toHaveText('Live updates are paused.');
  await expect.poll(() => page.evaluate(() => window.__heldLiveFrames.length)).toBeGreaterThan(0);
  await page.evaluate(() => {
    window.dispatchEvent(new PageTransitionEvent('pagehide', {persisted: true}));
    const frames = window.__heldLiveFrames.splice(0);
    window.requestAnimationFrame = window.__nativeRequestAnimationFrame;
    for (const frame of frames) frame(performance.now());
    window.dispatchEvent(new PageTransitionEvent('pageshow', {persisted: true}));
  });
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await expect(page.getByRole('status')).toHaveCount(0);
  await expect(page.locator('[data-live-feedback]')).toHaveText('');
  await expect(page.getByRole('button', {name: 'Resume live updates'})).toBeFocused();
});

test('missing EventSource leaves manual controls and non-live server content intact', async ({page}) => {
  await page.addInitScript(() => {
    sessionStorage.removeItem('symphony.live-updates.paused');
    Object.defineProperty(window, 'EventSource', {configurable: true, value: undefined});
  });
  await authorize(page, scenarioPath('/', 'empty'));
  await expect(page.locator('[data-live-controls]')).toBeHidden();
  await expect(page.getByRole('button', {name: 'Refresh tracker work'})).toBeVisible();
  await expect(page.getByRole('status')).toHaveCount(0);
  await expectNoAxeViolations(page);
});

test('JavaScript-disabled context retains the complete manual live-page workflow', async ({browser, authenticatedContext}) => {
  const cookies = await authenticatedContext.cookies('http://127.0.0.1:43127');
  const context = await browser.newContext({javaScriptEnabled: false, viewport: {width: 320, height: 720}});
  await context.addCookies(cookies);
  const noScriptPage = await context.newPage();
  try {
    const response = await noScriptPage.goto(`http://127.0.0.1:43127${scenarioPath('/', 'populated')}`);
    expect(response?.ok()).toBe(true);
    await expect(noScriptPage.locator('[data-live-controls]')).toBeHidden();
    const refresh = noScriptPage.getByRole('button', {name: 'Refresh tracker work'});
    await expect(refresh).toBeVisible();
    await expect(noScriptPage.getByRole('heading', {name: 'Work summary'})).toBeVisible();
    await expect(noScriptPage.getByRole('status')).toHaveCount(0);
    expect(await noScriptPage.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
    await refresh.click();
    const current = new URL(noScriptPage.url());
    expect(current.searchParams.get('__e2e_scenario')).toBe('populated');
    expect(current.searchParams.get('result')).toBe('refresh-requested');
    await expect(noScriptPage.getByRole('status')).toHaveText('Refresh requested.');
    await expect(noScriptPage.getByRole('status')).toHaveCount(1);
    await expect(noScriptPage.getByRole('button', {name: 'Refresh tracker work'})).toBeFocused();
  } finally {
    await context.close();
  }
});
