const formatter = new Intl.RelativeTimeFormat(undefined, {numeric: 'always'});

function updateRequest(request) {
  const deadline = Date.parse(request.dataset.deadline ?? '');
  if (!Number.isFinite(deadline)) return;
  const remainingSeconds = Math.max(0, Math.ceil((deadline - Date.now()) / 1000));
  const deadlineNode = request.querySelector('[data-request-deadline]');
  if (deadlineNode instanceof HTMLTimeElement) {
    const minutes = Math.ceil(remainingSeconds / 60);
    deadlineNode.textContent = remainingSeconds === 0 ? 'Expired' : formatter.format(minutes, 'minute');
  }
  const warning = request.querySelector('[data-request-warning]');
  if (warning instanceof HTMLElement && remainingSeconds > 0 && remainingSeconds <= 20 && warning.hidden) {
    warning.hidden = false;
  }
}

const requests = [...document.querySelectorAll('[data-operator-request]')];
for (const request of requests) updateRequest(request);
if (requests.length > 0) {
  window.setInterval(() => {
    for (const request of requests) updateRequest(request);
  }, 15000);
}
