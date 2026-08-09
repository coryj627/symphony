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
