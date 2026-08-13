// Native links provide all navigation. This enhancement makes the skip-link
// destination consistent in browsers that do not focus fragment targets.
const skipLink = document.querySelector('.skip-link');
const main = document.querySelector('#main-content');

if (skipLink instanceof HTMLAnchorElement && main instanceof HTMLElement) {
  skipLink.addEventListener('click', () => {
    main.focus({preventScroll: true});
    main.scrollIntoView({block: 'start'});
  });
}

const focusTarget = document.body.dataset.focusTarget;
const allowedFocusTargets = new Set([
  'error-summary',
  'save-structured',
  'save-raw',
  'validate-raw',
  'replace-credential',
  'delete-credential',
  'credential-delete-cancel',
  'refresh',
  'start-runtime',
  'stop-runtime',
  'requests-heading',
]);

if (focusTarget && allowedFocusTargets.has(focusTarget)) {
  const target = document.getElementById(focusTarget);
  if (target instanceof HTMLElement) {
    target.focus({preventScroll: true});
    target.scrollIntoView({block: 'nearest'});
  }
}

const responsiveBreakpoint = window.matchMedia('(max-width: 40rem)');
let responsiveFocusKey = '';

document.addEventListener('focusin', event => {
  if (!(event.target instanceof HTMLElement)) return;
  if (event.target.dataset.responsiveFocusKey) {
    responsiveFocusKey = event.target.dataset.responsiveFocusKey;
    return;
  }
  if (event.target !== document.body && event.target !== document.documentElement) {
    responsiveFocusKey = '';
  }
});

document.addEventListener('pointerdown', event => {
  if (event.target instanceof Element && event.target.closest('[data-responsive-focus-key]')) return;
  window.requestAnimationFrame(() => {
    const active = document.activeElement;
    if (!(active instanceof HTMLElement) || !active.dataset.responsiveFocusKey) {
      responsiveFocusKey = '';
    }
  });
});

responsiveBreakpoint.addEventListener('change', () => {
  const key = responsiveFocusKey;
  if (!key) return;
  window.requestAnimationFrame(() => {
    if (responsiveFocusKey !== key) return;
    const branch = responsiveBreakpoint.matches ? '.responsive-narrow' : '.responsive-wide';
    const targets = [...document.querySelectorAll(`${branch} [data-responsive-focus-key]`)];
    const target = targets.find(candidate => candidate instanceof HTMLElement
      && candidate.dataset.responsiveFocusKey === key
      && candidate.getClientRects().length > 0);
    if (!(target instanceof HTMLElement)) return;
    target.focus();
    target.scrollIntoView({behavior: 'instant', block: 'center', inline: 'nearest'});
  });
});

const deleteButton = document.getElementById('delete-credential');
const deleteDialog = document.getElementById('credential-delete-dialog');
const deleteCancel = document.getElementById('credential-delete-cancel');

function restoreDeleteFocus() {
  if (deleteButton instanceof HTMLButtonElement && !deleteButton.disabled) {
    deleteButton.focus();
  }
}

function focusDeleteDialogTarget(target) {
  if (target instanceof HTMLElement) target.focus();
}

function openDeleteDialog(target = deleteCancel) {
  if (!(deleteDialog instanceof HTMLDialogElement)) return;
  if (deleteDialog.open) deleteDialog.close();
  deleteDialog.showModal();
  focusDeleteDialogTarget(target);
}

if (deleteButton instanceof HTMLButtonElement && deleteDialog instanceof HTMLDialogElement) {
  deleteButton.addEventListener('click', event => {
    if (deleteButton.disabled) return;
    event.preventDefault();
    openDeleteDialog(deleteCancel);
  });

  deleteDialog.addEventListener('cancel', event => {
    event.preventDefault();
    deleteDialog.close();
    restoreDeleteFocus();
  });

  deleteDialog.addEventListener('keydown', event => {
    if (event.key !== 'Tab') return;
    const focusable = [...deleteDialog.querySelectorAll('a[href], button:not(:disabled)')]
      .filter(element => element instanceof HTMLElement && element.getClientRects().length > 0);
    if (focusable.length === 0) return;
    const currentIndex = focusable.indexOf(document.activeElement);
    const nextIndex = event.shiftKey
      ? (currentIndex <= 0 ? focusable.length - 1 : currentIndex - 1)
      : (currentIndex < 0 || currentIndex === focusable.length - 1 ? 0 : currentIndex + 1);
    event.preventDefault();
    focusable[nextIndex].focus();
  });

  if (deleteCancel instanceof HTMLAnchorElement) {
    deleteCancel.addEventListener('click', event => {
      event.preventDefault();
      deleteDialog.close();
      restoreDeleteFocus();
    });
  }

  if (deleteDialog.open) {
    const initialTarget = focusTarget === 'error-summary'
      ? document.getElementById('error-summary')
      : deleteCancel;
    openDeleteDialog(initialTarget);
  }
}

const liveRoot = document.querySelector('[data-live-root]');

if (liveRoot instanceof HTMLElement && typeof window.EventSource === 'function') {
  const liveControls = liveRoot.querySelector('[data-live-controls]');
  const liveToggle = liveRoot.querySelector('[data-live-toggle]');
  const liveConnection = liveRoot.querySelector('[data-live-connection]');
  const liveApply = liveRoot.querySelector('[data-live-apply]');
  const liveFeedback = liveRoot.querySelector('[data-live-feedback]');
  const stateURL = liveRoot.dataset.stateUrl;
  const eventsURL = liveRoot.dataset.eventsUrl;
  const route = liveRoot.dataset.liveRoute;
  const pausedStorageKey = 'symphony.live-updates.paused';

  if (liveControls instanceof HTMLElement
      && liveToggle instanceof HTMLButtonElement
      && liveConnection instanceof HTMLElement
      && liveApply instanceof HTMLButtonElement
      && liveFeedback instanceof HTMLElement
      && typeof stateURL === 'string'
      && typeof eventsURL === 'string'
      && typeof route === 'string') {
    let currentCursor = parseLiveCursor(liveRoot.dataset.eventCursorId);
    let renderedCursor = currentCursor;
    let source = null;
    let stateController = null;
    let stateRequest = null;
    let generation = 0;
    let pendingSnapshot = null;
    let resuming = false;
    let pageSuspended = false;
    let feedbackGeneration = 0;
    let hideApplyAfterFocus = false;
    let reconciliationTimer = null;
    let reconciliationDelay = 1000;
    let reconciliationRequired = false;
    let staleRetryTimer = null;
    let staleRetryDelay = 1000;
    let paused = readPausedPreference(pausedStorageKey);
    const stateApplied = 'applied';
    const stateStale = 'stale';
    const stateFailed = 'failed';
    const stateAborted = 'aborted';

    liveControls.hidden = false;
    setPausedPresentation(paused);

    if (!paused && currentCursor !== null) {
      connectLiveSource();
    }

    liveToggle.addEventListener('click', () => {
      if (resuming) return;
      if (paused) {
        void resumeLiveUpdates();
      } else {
        pauseLiveUpdates();
      }
    });

    liveApply.addEventListener('click', () => {
      const viewport = {left: window.scrollX, top: window.scrollY};
      reservePresentationHeight();
      applyPendingSnapshot();
      window.scrollTo({...viewport, behavior: 'instant'});
      window.requestAnimationFrame(() => {
        window.scrollTo({...viewport, behavior: 'instant'});
        window.requestAnimationFrame(() => window.scrollTo({...viewport, behavior: 'instant'}));
      });
    });

    liveRoot.addEventListener('focusout', () => {
      window.requestAnimationFrame(() => {
        const controlsContainFocus = liveControls.contains(document.activeElement);
        if (!paused && !presentationContainsFocus() && !controlsContainFocus) {
          applyPendingSnapshot();
          if (hideApplyAfterFocus && document.activeElement !== liveApply) {
            hideApplyAfterFocus = false;
            liveApply.hidden = true;
          }
        }
        if (!controlsContainFocus) releasePresentationHeight();
      });
    });

    window.addEventListener('pagehide', () => {
      pageSuspended = true;
      generation++;
      invalidatePendingSnapshot();
      clearLiveFeedback();
      abortStateRequest();
      closeLiveSource();
      clearReconciliationTimer();
      clearStaleRetryTimer();
    });

    window.addEventListener('pageshow', () => {
      pageSuspended = false;
      if (!paused && source === null && renderedCursor !== null) void reconcileAfterPageShow();
    });

    function setPausedPresentation(isPaused) {
      liveToggle.textContent = isPaused ? 'Resume live updates' : 'Pause live updates';
      liveConnection.textContent = isPaused ? 'Live updates are paused.' : 'Live updates are available.';
      if (isPaused) liveApply.hidden = true;
    }

    function pauseLiveUpdates() {
      paused = true;
      writePausedPreference(pausedStorageKey, true);
      generation++;
      abortStateRequest();
      closeLiveSource();
      clearReconciliationTimer();
      clearStaleRetryTimer();
      reconciliationRequired = false;
      pendingSnapshot = null;
      hideApplyAfterFocus = false;
      liveApply.hidden = true;
      setPausedPresentation(true);
    }

    async function resumeLiveUpdates() {
      if (resuming) return;
      resuming = true;
      liveToggle.setAttribute('aria-disabled', 'true');
      clearLiveFeedback();
      liveConnection.textContent = 'Resuming live updates.';
      const resumeGeneration = ++generation;
      abortStateRequest();
      closeLiveSource();
      const outcome = await requestState({authoritative: true, allowPaused: true});
      if (generation !== resumeGeneration || pageSuspended) {
        paused = true;
        setPausedPresentation(true);
        liveToggle.removeAttribute('aria-disabled');
        resuming = false;
        return;
      }
      if (outcome === stateApplied) {
        paused = false;
        reconciliationRequired = false;
        reconciliationDelay = 1000;
        clearStaleRetryTimer();
        writePausedPreference(pausedStorageKey, false);
        setPausedPresentation(false);
        if (!pageSuspended) connectLiveSource();
      } else {
        paused = true;
        setPausedPresentation(true);
        announceResumeFailure();
      }
      liveToggle.removeAttribute('aria-disabled');
      resuming = false;
    }

    function connectLiveSource() {
      if (paused || pageSuspended || currentCursor === null || source !== null) return;
      const target = liveEventURL(eventsURL, currentCursor.raw);
      if (target === null) return;
      const opened = new window.EventSource(target, {withCredentials: true});
      source = opened;
      opened.addEventListener('open', () => {
        if (source === opened) liveConnection.textContent = 'Live updates are connected.';
      });
      opened.addEventListener('error', () => {
        if (source !== opened || paused) return;
        liveConnection.textContent = 'Live updates are reconnecting.';
        if (cursorIsAhead(currentCursor, renderedCursor)) beginReconciliation();
      });
      for (const eventName of ['queue.refreshed', 'queue.failed', 'configuration.changed', 'runtime.changed']) {
        opened.addEventListener(eventName, event => {
          if (source === opened) handleLiveEvent(event);
        });
      }
      opened.addEventListener('reset', () => {
        if (source === opened) void reconcileAfterReset();
      });
    }

    function closeLiveSource() {
      if (source !== null) source.close();
      source = null;
    }

    function handleLiveEvent(event) {
      let data;
      try {
        data = JSON.parse(event.data);
      } catch {
        void reconcileAfterReset();
        return;
      }
      if (!isRecord(data)) {
        void reconcileAfterReset();
        return;
      }
      const received = parseLiveCursor(event.lastEventId);
      if (received === null) {
        void reconcileAfterReset();
        return;
      }
      if (currentCursor !== null) {
        if (received.epoch !== currentCursor.epoch) {
          void reconcileAfterReset();
          return;
        }
        if (received.sequence <= currentCursor.sequence) return;
      }
      currentCursor = received;
      liveRoot.dataset.eventCursorId = received.raw;
      if (pendingSnapshot !== null && !sameLiveCursor(pendingSnapshot.cursor, received)) {
        invalidatePendingSnapshot();
      }
      clearStaleRetryTimer();
      void reconcileLiveEvent();
    }

    async function reconcileLiveEvent() {
      const eventGeneration = generation;
      const outcome = await requestState({authoritative: false, allowPaused: false});
      if (generation !== eventGeneration || paused || pageSuspended) return;
      if (outcome === stateFailed) beginReconciliation();
      else if (outcome === stateStale) scheduleStaleRetry();
    }

    async function reconcileAfterReset() {
      const resetGeneration = ++generation;
      invalidatePendingSnapshot();
      abortStateRequest();
      closeLiveSource();
      clearStaleRetryTimer();
      if (paused || pageSuspended) return;
      reconciliationRequired = true;
      const outcome = await requestState({authoritative: true, allowPaused: false});
      if (generation !== resetGeneration || paused || pageSuspended) return;
      if (outcome === stateApplied) {
        reconciliationRequired = false;
        reconciliationDelay = 1000;
        connectLiveSource();
      } else if (!paused && !pageSuspended) {
        scheduleReconciliation();
      }
    }

    async function reconcileAfterPageShow() {
      const pageShowGeneration = ++generation;
      invalidatePendingSnapshot();
      abortStateRequest();
      closeLiveSource();
      clearStaleRetryTimer();
      const outcome = await requestState({authoritative: true, allowPaused: false});
      if (generation !== pageShowGeneration || paused || pageSuspended) return;
      if (outcome === stateApplied) {
        reconciliationRequired = false;
        reconciliationDelay = 1000;
        connectLiveSource();
      } else if (!paused && !pageSuspended) {
        reconciliationRequired = true;
        scheduleReconciliation();
      }
    }

    function beginReconciliation() {
      if (paused || pageSuspended) return;
      generation++;
      invalidatePendingSnapshot();
      abortStateRequest();
      closeLiveSource();
      clearStaleRetryTimer();
      reconciliationRequired = true;
      if (renderedCursor !== null) {
        currentCursor = renderedCursor;
        liveRoot.dataset.eventCursorId = renderedCursor.raw;
      }
      scheduleReconciliation();
    }

    function scheduleReconciliation() {
      if (!reconciliationRequired || paused || pageSuspended || reconciliationTimer !== null) return;
      const delay = reconciliationDelay;
      reconciliationTimer = window.setTimeout(async () => {
        reconciliationTimer = null;
        if (!reconciliationRequired || paused || pageSuspended) return;
        const retryGeneration = generation;
        const outcome = await requestState({authoritative: true, allowPaused: false});
        if (retryGeneration !== generation || paused || pageSuspended) return;
        if (outcome === stateApplied) {
          reconciliationRequired = false;
          reconciliationDelay = 1000;
          connectLiveSource();
          return;
        }
        reconciliationDelay = Math.min(reconciliationDelay * 2, 20000);
        scheduleReconciliation();
      }, delay);
    }

    function clearReconciliationTimer() {
      if (reconciliationTimer !== null) window.clearTimeout(reconciliationTimer);
      reconciliationTimer = null;
      reconciliationDelay = 1000;
    }

    function scheduleStaleRetry() {
      if (paused || pageSuspended || staleRetryTimer !== null) return;
      const delay = staleRetryDelay;
      const retryGeneration = generation;
      staleRetryTimer = window.setTimeout(async () => {
        staleRetryTimer = null;
        if (retryGeneration !== generation || paused || pageSuspended) return;
        const outcome = await requestState({authoritative: false, allowPaused: false});
        if (retryGeneration !== generation || paused || pageSuspended) return;
        if (outcome === stateApplied) {
          staleRetryDelay = 1000;
          return;
        }
        if (outcome === stateFailed) {
          beginReconciliation();
          return;
        }
        if (outcome === stateStale) {
          staleRetryDelay = Math.min(staleRetryDelay * 2, 20000);
          scheduleStaleRetry();
        }
      }, delay);
    }

    function clearStaleRetryTimer() {
      if (staleRetryTimer !== null) window.clearTimeout(staleRetryTimer);
      staleRetryTimer = null;
      staleRetryDelay = 1000;
    }

    async function requestState({authoritative, allowPaused}) {
      if (stateRequest !== null) {
        return stateRequest;
      }
      const requestGeneration = generation;
      const pendingRequest = (async () => {
        for (let attempt = 0; attempt < 2; attempt += 1) {
          const controller = new AbortController();
          stateController = controller;
          try {
            const response = await fetch(stateURL, {
              credentials: 'same-origin',
              headers: {Accept: 'application/json'},
              signal: controller.signal,
            });
            if (!response.ok) throw new Error('state request failed');
            const snapshot = await response.json();
            if (requestGeneration !== generation || (paused && !allowPaused)) return stateAborted;
            const snapshotCursor = validateLiveSnapshot(snapshot, route);
            if (!authoritative && currentCursor !== null
                && snapshotCursor.epoch === currentCursor.epoch
                && snapshotCursor.sequence < currentCursor.sequence) {
              if (attempt === 0) continue;
              return stateStale;
            }
            const presentationUpdated = renderLiveSnapshot(snapshot, snapshotCursor);
            currentCursor = snapshotCursor;
            if (presentationUpdated) renderedCursor = snapshotCursor;
            liveRoot.dataset.eventCursorId = snapshotCursor.raw;
            liveConnection.textContent = paused ? 'Live updates are paused.' : 'Live updates are connected.';
            return stateApplied;
          } catch (error) {
            if (requestGeneration !== generation || pageSuspended || (paused && !allowPaused)) return stateAborted;
            if (error instanceof DOMException && error.name === 'AbortError') return stateAborted;
            if (!allowPaused && !paused) liveConnection.textContent = 'Live updates are temporarily unavailable.';
            return stateFailed;
          }
        }
        return stateStale;
      })();
      stateRequest = pendingRequest;
      try {
        return await pendingRequest;
      } finally {
        if (stateRequest === pendingRequest) {
          stateController = null;
          stateRequest = null;
        }
      }
    }

    function abortStateRequest() {
      if (stateController !== null) stateController.abort();
      stateController = null;
      stateRequest = null;
    }

    function renderLiveSnapshot(snapshot, snapshotCursor) {
      if (route === 'overview') {
        updateOverview(snapshot);
        pendingSnapshot = null;
        finishPendingPresentation();
        return true;
      }
      if (route === 'issues') {
        return updateIssues(snapshot, snapshotCursor);
      }
      if (route === 'activity') {
        return updateActivity(snapshot, snapshotCursor);
      }
      return false;
    }

    function updateOverview(snapshot) {
      for (const name of ['candidates', 'routable', 'running', 'retrying', 'requests', 'errors']) {
        const target = liveRoot.querySelector(`[data-live-count="${name}"]`);
        if (target instanceof HTMLElement) setTextWithoutReplacingFocus(target, String(snapshot.counts[name]));
      }
      updateOverviewText('tracker-scope', snapshot.tracker.scope);
      updateOverviewText('scheduler', snapshot.scheduler.available
        ? (snapshot.scheduler.state === 'stopping' ? 'Stopping' : (snapshot.scheduler.enabled ? 'Running' : 'Paused'))
        : 'Unavailable');
      updateOptionalOverviewText('scheduler-state', snapshot.scheduler.state);
      updateOptionalOverviewText('scheduler-message', snapshot.scheduler.message);
      updateOptionalOverviewText('tracker-state', snapshot.tracker.state);
      updateOptionalOverviewText('tracker-message', snapshot.tracker.message);
      updateOptionalOverviewText('config-state', snapshot.config.state);
      updateOptionalOverviewText('config-message', snapshot.config.message);
      const stale = liveRoot.querySelector('[data-live-overview-stale]');
      if (stale instanceof HTMLElement) stale.hidden = !snapshot.tracker.stale;
      updateOverviewTime('last-attempt', snapshot.tracker.last_attempt_at, 'Not yet attempted');
      updateOverviewTime('last-success', snapshot.tracker.last_success_at, 'No successful refresh yet');
      updateOverviewErrors(snapshot);
      updateRuntimeControls(snapshot.scheduler);
    }

    function updateRuntimeControls(scheduler) {
      const start = liveRoot.querySelector('[data-runtime-start]');
      const stop = liveRoot.querySelector('[data-runtime-stop]');
      const startReason = liveRoot.querySelector('[data-runtime-start-reason]');
      const stopReason = liveRoot.querySelector('[data-runtime-stop-reason]');
      if (!(start instanceof HTMLButtonElement) || !(stop instanceof HTMLButtonElement)
          || !(startReason instanceof HTMLElement) || !(stopReason instanceof HTMLElement)) return;
      if (!scheduler.available) {
        setRuntimeControlDisabled(start, true);
        setRuntimeControlDisabled(stop, true);
        setTextWithoutReplacingFocus(startReason, scheduler.message || 'Agent runtime will be enabled in Phase 4.');
        setTextWithoutReplacingFocus(stopReason, scheduler.message || 'Agent runtime will be enabled in Phase 4.');
        return;
      }
      if (scheduler.state === 'stopping') {
        setRuntimeControlDisabled(start, true);
        setRuntimeControlDisabled(stop, true);
        setTextWithoutReplacingFocus(startReason, 'Wait for active work to stop before restarting the scheduler.');
        setTextWithoutReplacingFocus(stopReason, 'The scheduler is already stopping.');
        return;
      }
      setRuntimeControlDisabled(start, scheduler.enabled);
      setRuntimeControlDisabled(stop, !scheduler.enabled);
      setTextWithoutReplacingFocus(startReason, scheduler.enabled ? 'The scheduler is already running.' : 'Start polling and dispatch for this project.');
      setTextWithoutReplacingFocus(stopReason, scheduler.enabled ? 'Stop the scheduler after active work is canceled safely.' : 'The scheduler is already paused.');
    }

    function setRuntimeControlDisabled(button, disabled) {
      if (button.dataset.runtimeDisableReady !== 'true') {
        button.dataset.runtimeDisableReady = 'true';
        button.addEventListener('click', event => {
          if (button.getAttribute('aria-disabled') === 'true') event.preventDefault();
        });
        button.addEventListener('focusout', () => {
          window.requestAnimationFrame(() => {
            if (document.activeElement === button || button.dataset.runtimeDisabled !== 'true') return;
            button.disabled = true;
            button.removeAttribute('aria-disabled');
          });
        });
      }
      button.dataset.runtimeDisabled = disabled ? 'true' : 'false';
      if (!disabled) {
        button.disabled = false;
        button.removeAttribute('aria-disabled');
        return;
      }
      if (document.activeElement === button) {
        button.disabled = false;
        button.setAttribute('aria-disabled', 'true');
        return;
      }
      button.disabled = true;
      button.removeAttribute('aria-disabled');
    }

    function updateOverviewText(name, value) {
      let target = liveRoot.querySelector(`[data-live-overview-field="${name}"]`);
      if (!(target instanceof HTMLElement)) {
        const container = liveRoot.querySelector(`[data-live-overview-field-container="${name}"]`);
        target = container instanceof HTMLElement ? container.querySelector('dd') : null;
      }
      if (target instanceof HTMLElement) setTextWithoutReplacingFocus(target, value);
    }

    function updateOptionalOverviewText(name, value) {
      const row = liveRoot.querySelector(`[data-live-overview-row="${name}"]`);
      const target = liveRoot.querySelector(`[data-live-overview-field="${name}"]`);
      if (!(row instanceof HTMLElement) || !(target instanceof HTMLElement)) return;
      row.hidden = value === '';
      setTextWithoutReplacingFocus(target, value);
    }

    function updateOverviewTime(name, value, emptyText) {
      const target = liveRoot.querySelector(`[data-live-overview-time="${name}"]`);
      if (!(target instanceof HTMLElement) || containsDocumentFocus(target)) return;
      if (value === null) {
        target.textContent = emptyText;
        return;
      }
      const time = document.createElement('time');
      time.dateTime = value;
      const parsed = new Date(value);
      time.textContent = parsed.toLocaleString();
      target.replaceChildren(time);
    }

    function updateOverviewErrors(snapshot) {
      const list = liveRoot.querySelector('[data-live-overview-errors]');
      const empty = liveRoot.querySelector('[data-live-overview-errors-empty]');
      const config = liveRoot.querySelector('[data-live-overview-error="config"]');
      const tracker = liveRoot.querySelector('[data-live-overview-error="tracker"]');
      if (!(list instanceof HTMLElement)
          || !(empty instanceof HTMLElement)
          || !(config instanceof HTMLElement)
          || !(tracker instanceof HTMLElement)) return;
      const configError = snapshot.config.has_error;
      const trackerError = snapshot.tracker.has_error;
      config.hidden = !configError;
      tracker.hidden = !trackerError;
      list.hidden = !configError && !trackerError;
      empty.hidden = configError || trackerError;
      const configMessage = config.querySelector('[data-field="message"]');
      const trackerMessage = tracker.querySelector('[data-field="message"]');
      if (configMessage instanceof HTMLElement) setTextWithoutReplacingFocus(configMessage, snapshot.config.message);
      if (trackerMessage instanceof HTMLElement) setTextWithoutReplacingFocus(trackerMessage, snapshot.tracker.message);
    }

    function updateIssues(snapshot, snapshotCursor) {
      const results = liveRoot.querySelector('[data-live-issues-results]');
      const wide = liveRoot.querySelector('[data-live-issues-wide]');
      const narrow = liveRoot.querySelector('[data-live-issues-narrow]');
      const empty = liveRoot.querySelector('[data-live-issues-empty]');
      if (!(results instanceof HTMLElement)) {
        throw new Error('live issue targets are unavailable');
      }
      let currentWide;
      let currentNarrow;
      if (wide instanceof HTMLTableSectionElement && narrow instanceof HTMLUListElement && empty === null) {
        currentWide = issueElementMap(wide);
        currentNarrow = issueElementMap(narrow);
      } else if (wide === null && narrow === null && empty instanceof HTMLElement) {
        currentWide = new Map();
        currentNarrow = new Map();
      } else {
        throw new Error('live issue targets are inconsistent');
      }
      const nextIDs = snapshot.candidates.map(candidate => candidate.issue_id);
      if (currentWide === null || currentNarrow === null
          || !sameStrings([...currentWide.keys()], [...currentNarrow.keys()])) {
        throw new Error('live issue identities are invalid');
      }
      const structural = !sameStrings([...currentWide.keys()], nextIDs);
      if (structural && presentationContainsFocus()) {
        pendingSnapshot = {snapshot, cursor: snapshotCursor, generation};
        hideApplyAfterFocus = false;
        liveApply.hidden = false;
        return false;
      }
      if (structural) {
        results.replaceChildren(createIssueResults(snapshot.candidates, stateURL));
      } else {
        let skippedFocusedField = false;
        for (const candidate of snapshot.candidates) {
          const wideRow = currentWide.get(candidate.issue_id);
          const narrowRow = currentNarrow.get(candidate.issue_id);
          const focusedFields = focusedIssueFields(wideRow, narrowRow);
          skippedFocusedField = patchIssueRow(wideRow, candidate, stateURL, focusedFields) || skippedFocusedField;
          skippedFocusedField = patchIssueRow(narrowRow, candidate, stateURL, focusedFields) || skippedFocusedField;
        }
        if (skippedFocusedField) {
          pendingSnapshot = {snapshot, cursor: snapshotCursor, generation};
          hideApplyAfterFocus = false;
          liveApply.hidden = false;
          return false;
        }
      }
      pendingSnapshot = null;
      finishPendingPresentation();
      return true;
    }

    function updateActivity(snapshot, snapshotCursor) {
      const list = liveRoot.querySelector('[data-live-activity]');
      const reset = liveRoot.querySelector('[data-live-activity-reset]');
      const empty = liveRoot.querySelector('[data-live-activity-empty]');
      if (!(list instanceof HTMLOListElement) || !(reset instanceof HTMLElement) || !(empty instanceof HTMLElement)) {
        throw new Error('live activity targets are unavailable');
      }
      if (list.contains(document.activeElement)) {
        pendingSnapshot = {snapshot, cursor: snapshotCursor, generation};
        hideApplyAfterFocus = false;
        liveApply.hidden = false;
        return false;
      }
      const items = document.createDocumentFragment();
      for (const event of snapshot.activity_events) items.append(createActivityItem(event));
      list.replaceChildren(items);
      list.hidden = snapshot.activity_events.length === 0;
      empty.hidden = snapshot.activity_events.length !== 0;
      reset.hidden = !snapshot.activity_events_reset;
      pendingSnapshot = null;
      finishPendingPresentation();
      return true;
    }

    function applyPendingSnapshot() {
      if (paused || pageSuspended || reconciliationRequired || pendingSnapshot === null) return;
      if (pendingSnapshot.generation !== generation
          || currentCursor === null
          || !sameLiveCursor(pendingSnapshot.cursor, currentCursor)) {
        invalidatePendingSnapshot();
        return;
      }
      const pending = pendingSnapshot;
      pendingSnapshot = null;
      if (renderLiveSnapshot(pending.snapshot, pending.cursor)) renderedCursor = pending.cursor;
    }

    function invalidatePendingSnapshot() {
      pendingSnapshot = null;
      finishPendingPresentation();
    }

    function finishPendingPresentation() {
      if (liveApply === document.activeElement) {
        hideApplyAfterFocus = true;
        liveApply.hidden = false;
      } else {
        hideApplyAfterFocus = false;
        liveApply.hidden = true;
      }
    }

    function presentationContainsFocus() {
      const presentation = liveRoot.querySelector('[data-live-presentation]');
      return presentation instanceof HTMLElement && presentation.contains(document.activeElement);
    }

    function reservePresentationHeight() {
      const presentation = liveRoot.querySelector('[data-live-presentation]');
      if (!(presentation instanceof HTMLElement) || !liveControls.contains(document.activeElement)) return;
      liveRoot.style.overflowAnchor = 'none';
      presentation.style.minBlockSize = `${presentation.getBoundingClientRect().height}px`;
    }

    function releasePresentationHeight() {
      const presentation = liveRoot.querySelector('[data-live-presentation]');
      if (presentation instanceof HTMLElement) presentation.style.removeProperty('min-block-size');
      liveRoot.style.removeProperty('overflow-anchor');
    }

    function announceResumeFailure() {
      const announcement = ++feedbackGeneration;
      for (const existing of document.querySelectorAll('.persistent-status[role="status"]')) {
        if (existing === liveFeedback) continue;
        existing.removeAttribute('role');
        existing.removeAttribute('aria-live');
        existing.removeAttribute('aria-atomic');
      }
      liveFeedback.removeAttribute('role');
      liveFeedback.removeAttribute('aria-live');
      liveFeedback.removeAttribute('aria-atomic');
      liveFeedback.textContent = '';
      window.requestAnimationFrame(() => {
        if (announcement !== feedbackGeneration || pageSuspended) return;
        liveFeedback.setAttribute('role', 'status');
        liveFeedback.setAttribute('aria-live', 'polite');
        liveFeedback.setAttribute('aria-atomic', 'true');
        window.requestAnimationFrame(() => {
          if (announcement === feedbackGeneration && !pageSuspended) {
            liveFeedback.textContent = 'Live updates could not be resumed.';
          }
        });
      });
    }

    function clearLiveFeedback() {
      feedbackGeneration++;
      liveFeedback.textContent = '';
      liveFeedback.removeAttribute('role');
      liveFeedback.removeAttribute('aria-live');
      liveFeedback.removeAttribute('aria-atomic');
    }
  }
}

function parseLiveCursor(value) {
  if (typeof value !== 'string' || value.length === 0 || value.length > 149) return null;
  const separator = value.lastIndexOf(':');
  if (separator < 1 || separator === value.length - 1) return null;
  const epoch = value.slice(0, separator);
  const sequenceText = value.slice(separator + 1);
  if (epoch.length > 128
      || sequenceText.length > 20
      || !/^[A-Za-z0-9._~-]+$/.test(epoch)
      || !/^(0|[1-9][0-9]*)$/.test(sequenceText)) return null;
  try {
    const sequence = BigInt(sequenceText);
    if (sequence > 18446744073709551615n) return null;
    return {raw: value, epoch, sequence};
  } catch {
    return null;
  }
}

function liveEventURL(base, cursor) {
  try {
    const target = new URL(base, window.location.href);
    if (target.origin !== window.location.origin) return null;
    target.searchParams.set('after', cursor);
    return `${target.pathname}${target.search}`;
  } catch {
    return null;
  }
}

function validateLiveSnapshot(snapshot, route) {
  if (!isRecord(snapshot)) throw new Error('invalid live snapshot');
  const cursor = parseLiveCursor(snapshot.event_cursor);
  if (cursor === null) throw new Error('invalid live cursor');
  if (route === 'overview') {
    if (!isRecord(snapshot.counts)) throw new Error('invalid live counts');
    for (const name of ['candidates', 'routable', 'running', 'retrying', 'requests', 'errors']) {
      if (!Number.isSafeInteger(snapshot.counts[name]) || snapshot.counts[name] < 0) throw new Error('invalid live count');
    }
    if (!validLiveScheduler(snapshot.scheduler)
        || !validLiveTracker(snapshot.tracker)
        || !validLiveConfig(snapshot.config)) {
      throw new Error('invalid live overview status');
    }
  }
  if (route === 'issues') validateLiveCandidates(snapshot.candidates);
  if (route === 'activity') validateLiveActivity(snapshot.activity_events, snapshot.activity_events_reset);
  return cursor;
}

function validLiveScheduler(scheduler) {
  return isRecord(scheduler)
    && typeof scheduler.available === 'boolean'
    && typeof scheduler.enabled === 'boolean'
    && validLiveText(scheduler.state, 128)
    && validLiveText(scheduler.message, 512);
}

function validLiveTracker(tracker) {
  return isRecord(tracker)
    && validLiveText(tracker.scope, 512)
    && validLiveText(tracker.state, 128)
    && typeof tracker.stale === 'boolean'
    && typeof tracker.has_error === 'boolean'
    && validLiveText(tracker.error_code, 128)
    && validLiveText(tracker.message, 512)
    && validLiveTime(tracker.last_attempt_at)
    && validLiveTime(tracker.last_success_at);
}

function validLiveConfig(config) {
  return isRecord(config)
    && validLiveText(config.state, 128)
    && typeof config.has_error === 'boolean'
    && validLiveText(config.error_code, 128)
    && validLiveText(config.message, 512);
}

function validLiveTime(value) {
  return value === null
    || (validLiveText(value, 128) && !Number.isNaN(new Date(value).valueOf()));
}

function validateLiveCandidates(candidates) {
  if (!Array.isArray(candidates)) throw new Error('invalid live candidates');
  const seen = new Set();
  for (const candidate of candidates) {
    if (!isRecord(candidate)
        || !validOpaqueIdentity(candidate.issue_id)
        || seen.has(candidate.issue_id)
        || !validLiveText(candidate.identifier, 256)
        || !validLiveText(candidate.title, 512)
        || !validLiveText(candidate.state, 128)
        || typeof candidate.routable !== 'boolean'
        || typeof candidate.stale !== 'boolean'
        || !Array.isArray(candidate.routing_reasons)) {
      throw new Error('invalid live candidate');
    }
    for (const reason of candidate.routing_reasons) {
      if (!isRecord(reason) || !validLiveText(reason.message, 512)) throw new Error('invalid live candidate reason');
    }
    seen.add(candidate.issue_id);
  }
}

function validateLiveActivity(events, reset) {
  if (!Array.isArray(events) || typeof reset !== 'boolean') throw new Error('invalid live activity');
  const seen = new Set();
  for (const event of events) {
    if (!isRecord(event)
        || parseLiveCursor(event.event_cursor) === null
        || seen.has(event.event_cursor)
        || !validLiveText(event.at, 128)
        || !validLiveText(event.summary, 512)) {
      throw new Error('invalid live activity event');
    }
    seen.add(event.event_cursor);
  }
}

function validOpaqueIdentity(value) {
  return typeof value === 'string'
    && value.length > 0
    && value.length <= 256
    && value.trim() === value
    && !/[\u0000-\u001f\u007f]/.test(value);
}

function validLiveText(value, maximum) {
  return typeof value === 'string' && value.length <= maximum && !/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/.test(value);
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function sameStrings(left, right) {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

function cursorIsAhead(candidate, rendered) {
  if (candidate === null || rendered === null) return false;
  return candidate.epoch !== rendered.epoch || candidate.sequence > rendered.sequence;
}

function sameLiveCursor(left, right) {
  return left !== null && right !== null
    && left.epoch === right.epoch
    && left.sequence === right.sequence;
}

function issueElementMap(container) {
  const rows = new Map();
  for (const child of container.children) {
    if (!(child instanceof HTMLElement) || !validOpaqueIdentity(child.dataset.liveIssueId) || rows.has(child.dataset.liveIssueId)) return null;
    rows.set(child.dataset.liveIssueId, child);
  }
  return rows;
}

function issueDetailURL(identifier, stateURL) {
  const target = new URL(`/issues/${pathEscapeSegment(identifier)}`, window.location.origin);
  let canonical;
  try {
    canonical = new URL(stateURL, window.location.origin);
  } catch {
    return target.pathname;
  }
  if (canonical.origin !== window.location.origin
      || canonical.pathname !== '/api/v1/state'
      || canonical.username !== ''
      || canonical.password !== ''
      || canonical.hash !== '') return target.pathname;
  const allowed = new Set(['query', 'state', 'eligibility', 'sort', '__e2e_scenario']);
  const names = new Set();
  for (const [name, value] of canonical.searchParams) {
    if (!allowed.has(name) || names.has(name) || !validLiveText(value, 256)) return target.pathname;
    names.add(name);
  }
  return `${target.pathname}${canonical.search}`;
}

function pathEscapeSegment(value) {
  let escaped = '';
  for (const byte of new TextEncoder().encode(value)) {
    const allowed = (byte >= 0x41 && byte <= 0x5a)
      || (byte >= 0x61 && byte <= 0x7a)
      || (byte >= 0x30 && byte <= 0x39)
      || byte === 0x2d || byte === 0x2e || byte === 0x5f || byte === 0x7e
      || byte === 0x3a || byte === 0x40 || byte === 0x26 || byte === 0x3d
      || byte === 0x2b || byte === 0x24;
    escaped += allowed ? String.fromCharCode(byte) : `%${byte.toString(16).toUpperCase().padStart(2, '0')}`;
  }
  return escaped;
}

function createIssueLink(candidate, stateURL) {
  const link = document.createElement('a');
  link.dataset.field = 'identifier';
  link.dataset.liveIssueId = candidate.issue_id;
  link.dataset.responsiveFocusKey = `issue-${candidate.issue_id}`;
  link.href = issueDetailURL(candidate.identifier, stateURL);
  link.textContent = candidate.identifier;
  return link;
}

function createIssueResults(candidates, stateURL) {
  if (candidates.length === 0) {
    const empty = document.createElement('p');
    empty.dataset.liveIssuesEmpty = '';
    empty.textContent = 'No tracker work candidates match these filters.';
    return empty;
  }
  const fragment = document.createDocumentFragment();
  const wideContainer = document.createElement('div');
  wideContainer.className = 'issue-table responsive-wide';
  wideContainer.dataset.liveIssuesWideContainer = '';
  const table = document.createElement('table');
  const caption = document.createElement('caption');
  caption.textContent = 'Tracker work candidates';
  const head = document.createElement('thead');
  const headingRow = document.createElement('tr');
  for (const label of ['Identifier', 'Title', 'State', 'Eligibility']) {
    const heading = document.createElement('th');
    heading.scope = 'col';
    heading.textContent = label;
    headingRow.append(heading);
  }
  head.append(headingRow);
  const body = document.createElement('tbody');
  body.dataset.liveIssuesWide = '';
  const narrow = document.createElement('ul');
  narrow.className = 'issue-list responsive-narrow';
  narrow.dataset.liveIssuesNarrow = '';
  narrow.setAttribute('aria-label', 'Tracker work candidates');
  for (const candidate of candidates) {
    body.append(createWideIssueRow(candidate, stateURL));
    narrow.append(createNarrowIssueRow(candidate, stateURL));
  }
  table.append(caption, head, body);
  wideContainer.append(table);
  fragment.append(wideContainer, narrow);
  return fragment;
}

function createWideIssueRow(candidate, stateURL) {
  const row = document.createElement('tr');
  row.dataset.liveIssueId = candidate.issue_id;
  const identifier = document.createElement('th');
  identifier.scope = 'row';
  identifier.append(createIssueLink(candidate, stateURL));
  const title = document.createElement('td');
  title.dataset.field = 'title';
  title.textContent = candidate.title;
  const state = document.createElement('td');
  state.dataset.field = 'state';
  populateIssueState(state, candidate);
  const eligibility = document.createElement('td');
  eligibility.dataset.field = 'eligibility';
  populateIssueEligibility(eligibility, candidate);
  row.append(identifier, title, state, eligibility);
  return row;
}

function createNarrowIssueRow(candidate, stateURL) {
  const item = document.createElement('li');
  item.dataset.liveIssueId = candidate.issue_id;
  const heading = document.createElement('h2');
  heading.append(createIssueLink(candidate, stateURL));
  const title = document.createElement('p');
  title.dataset.field = 'title';
  title.textContent = candidate.title;
  const details = document.createElement('dl');
  details.append(
    createIssueDetail('State', 'state', element => populateIssueState(element, candidate)),
    createIssueDetail('Eligibility', 'eligibility', element => populateIssueEligibility(element, candidate)),
  );
  item.append(heading, title, details);
  return item;
}

function createIssueDetail(label, field, populate) {
  const group = document.createElement('div');
  const term = document.createElement('dt');
  term.textContent = label;
  const description = document.createElement('dd');
  description.dataset.field = field;
  populate(description);
  group.append(term, description);
  return group;
}

function focusedIssueFields(...rows) {
  const focused = new Set();
  for (const field of ['identifier', 'title', 'state', 'eligibility']) {
    for (const row of rows) {
      const target = row instanceof HTMLElement ? row.querySelector(`[data-field="${field}"]`) : null;
      if (target instanceof HTMLElement && containsDocumentFocus(target)) {
        focused.add(field);
        break;
      }
    }
  }
  return focused;
}

function patchIssueRow(row, candidate, stateURL, focusedFields) {
  if (!(row instanceof HTMLElement)) throw new Error('live issue row is unavailable');
  const identifier = row.querySelector('[data-field="identifier"]');
  const title = row.querySelector('[data-field="title"]');
  const state = row.querySelector('[data-field="state"]');
  const eligibility = row.querySelector('[data-field="eligibility"]');
  if (!(identifier instanceof HTMLAnchorElement)
      || !(title instanceof HTMLElement)
      || !(state instanceof HTMLElement)
      || !(eligibility instanceof HTMLElement)) {
    throw new Error('live issue fields are unavailable');
  }
  let skippedFocusedField = false;
  const detailURL = issueDetailURL(candidate.identifier, stateURL);
  if (focusedFields.has('identifier')) {
    const expectedURL = new URL(detailURL, window.location.origin).href;
    skippedFocusedField = identifier.textContent !== candidate.identifier || identifier.href !== expectedURL;
  } else {
    identifier.textContent = candidate.identifier;
    identifier.href = detailURL;
  }
  if (focusedFields.has('title')) {
    skippedFocusedField = title.textContent !== candidate.title || skippedFocusedField;
  } else {
    title.textContent = candidate.title;
  }
  if (focusedFields.has('state')) {
    const expectedState = `${candidate.state}${candidate.stale ? ' (last known)' : ''}`;
    skippedFocusedField = state.textContent !== expectedState || skippedFocusedField;
  } else {
    populateIssueState(state, candidate);
  }
  if (focusedFields.has('eligibility')) {
    skippedFocusedField = issueEligibilityChanges(eligibility, candidate) || skippedFocusedField;
  } else {
    populateIssueEligibility(eligibility, candidate);
  }
  return skippedFocusedField;
}

function issueEligibilityChanges(target, candidate) {
  const expectedPrimary = candidate.routable ? 'Routable' : 'Needs attention';
  const primary = [...target.childNodes]
    .filter(node => node.nodeType === Node.TEXT_NODE)
    .map(node => node.textContent)
    .join('');
  const reasons = [...target.children];
  const expectedReasons = candidate.routable ? [] : candidate.routing_reasons.map(reason => reason.message);
  return primary !== expectedPrimary
    || reasons.length !== expectedReasons.length
    || reasons.some((reason, index) => !(reason instanceof HTMLElement)
      || !reason.classList.contains('reason')
      || reason.textContent !== expectedReasons[index]);
}

function populateIssueState(target, candidate) {
  target.replaceChildren(document.createTextNode(candidate.state));
  if (candidate.stale) {
    target.append(document.createTextNode(' '));
    const stale = document.createElement('span');
    stale.className = 'muted';
    stale.textContent = '(last known)';
    target.append(stale);
  }
}

function populateIssueEligibility(target, candidate) {
  target.replaceChildren(document.createTextNode(candidate.routable ? 'Routable' : 'Needs attention'));
  if (!candidate.routable) {
    for (const reason of candidate.routing_reasons) {
      const detail = document.createElement('span');
      detail.className = 'reason';
      detail.textContent = reason.message;
      target.append(detail);
    }
  }
}

function createActivityItem(event) {
  const item = document.createElement('li');
  item.dataset.liveEventId = event.event_cursor;
  const time = document.createElement('time');
  time.dataset.field = 'at';
  time.dateTime = event.at;
  const parsed = new Date(event.at);
  time.textContent = Number.isNaN(parsed.valueOf()) ? event.at : parsed.toLocaleString();
  const summary = document.createElement('p');
  summary.dataset.field = 'summary';
  summary.textContent = event.summary;
  item.append(time, summary);
  return item;
}

function containsDocumentFocus(target) {
  return target.contains(document.activeElement);
}

function setTextWithoutReplacingFocus(target, value) {
  if (containsDocumentFocus(target)) return true;
  target.textContent = value;
  return false;
}

function readPausedPreference(key) {
  try {
    return window.sessionStorage.getItem(key) === 'true';
  } catch {
    return false;
  }
}

function writePausedPreference(key, value) {
  try {
    if (value) window.sessionStorage.setItem(key, 'true');
    else window.sessionStorage.removeItem(key);
  } catch {
    // The control still works for this page when storage is unavailable.
  }
}
