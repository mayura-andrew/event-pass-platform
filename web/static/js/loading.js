const track = document.getElementById('loading-track');
const announcement = document.getElementById('loading-announcement');
let showTimer;

function show(message) {
  clearTimeout(showTimer);
  showTimer = setTimeout(() => {
    track.hidden = false;
    announcement.textContent = message;
  }, 120);
}

function hide() {
  clearTimeout(showTimer);
  track.hidden = true;
  announcement.textContent = '';
  document.querySelectorAll('[data-loading-disabled]').forEach((button) => {
    button.disabled = button.dataset.loadingDisabled === 'true';
    button.removeAttribute('data-loading-disabled');
    button.removeAttribute('aria-busy');
    button.classList.remove('is-loading');
  });
}

document.addEventListener('click', (event) => {
  if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  if (!(event.target instanceof Element)) return;
  const link = event.target.closest('a[href]');
  if (!link || link.hasAttribute('download') || link.target || link.dataset.noLoading !== undefined) return;

  const destination = new URL(link.href, location.href);
  if (destination.origin !== location.origin) return;
  if (destination.pathname === location.pathname && destination.search === location.search) return;
  if (destination.href === location.href) return;

  show('Loading page');
});

document.addEventListener('submit', (event) => {
  if (event.defaultPrevented || !(event.target instanceof HTMLFormElement)) return;
  const form = event.target;
  if (form.target || form.dataset.noLoading !== undefined || form.method.toLowerCase() === 'dialog') return;

  const destination = new URL(form.action, location.href);
  if (destination.origin !== location.origin) return;

  const button = event.submitter instanceof HTMLButtonElement || event.submitter instanceof HTMLInputElement
    ? event.submitter
    : null;
  if (button && !button.disabled) {
    button.dataset.loadingDisabled = 'false';
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
    button.classList.add('is-loading');
  }
  show('Submitting form');
});

window.addEventListener('pageshow', (event) => {
  hide();
  if (event.persisted && document.body.hasAttribute('data-authenticated')) location.reload();
});

window.addEventListener('pagehide', (event) => {
  if (!event.persisted || !document.body.hasAttribute('data-authenticated')) return;
  const main = document.createElement('main');
  main.className = 'wrap narrow';
  const message = document.createElement('p');
  message.setAttribute('role', 'status');
  message.textContent = 'Checking your session...';
  main.append(message);
  document.body.replaceChildren(main);
});

const accountId = document.body.dataset.accountId;
if (accountId) {
  try {
    localStorage.setItem('event-pass.account', accountId);
  } catch {
    // Cross-tab refresh is a convenience; the server still verifies every request.
  }
  window.addEventListener('storage', (event) => {
    if (event.key === 'event-pass.account' && event.newValue !== accountId) location.reload();
  });
}
