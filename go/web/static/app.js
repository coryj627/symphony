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
]);

if (focusTarget && allowedFocusTargets.has(focusTarget)) {
  const target = document.getElementById(focusTarget);
  if (target instanceof HTMLElement) {
    target.focus({preventScroll: true});
    target.scrollIntoView({block: 'nearest'});
  }
}

const deleteButton = document.getElementById('delete-credential');
const deleteDialog = document.getElementById('credential-delete-dialog');
const deleteCancel = document.getElementById('credential-delete-cancel');

function restoreDeleteFocus() {
  if (deleteButton instanceof HTMLButtonElement && !deleteButton.disabled) {
    deleteButton.focus();
  }
}

function openDeleteDialog() {
  if (!(deleteDialog instanceof HTMLDialogElement)) return;
  if (deleteDialog.open) deleteDialog.close();
  deleteDialog.showModal();
  if (deleteCancel instanceof HTMLButtonElement) deleteCancel.focus();
}

if (deleteButton instanceof HTMLButtonElement && deleteDialog instanceof HTMLDialogElement) {
  deleteButton.addEventListener('click', event => {
    if (deleteButton.disabled) return;
    event.preventDefault();
    openDeleteDialog();
  });

  deleteDialog.addEventListener('cancel', event => {
    event.preventDefault();
    deleteDialog.close();
    restoreDeleteFocus();
  });

  if (deleteCancel instanceof HTMLButtonElement) {
    deleteCancel.addEventListener('click', () => {
      deleteDialog.close();
      restoreDeleteFocus();
    });
  }

  if (deleteDialog.open) openDeleteDialog();
}
