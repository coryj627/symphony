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
