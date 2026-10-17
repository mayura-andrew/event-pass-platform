// =============================================================================
// Pass emails: queue a confirmation email with the QR pass for every
// confirmed delegate (or one at a time), watch the queue drain, pause and
// resume it. The server sends one message every couple of seconds so Gmail
// doesn't throttle; this page only shows progress.
// =============================================================================
import {
  $, MANAGER_ROLES, AUTH_EVENT, getSession, clearSession, api, el, fmtInt, fmtTime, toast,
} from './common.js';

const FAST_POLL_MS = 2000;  // while sending
const SLOW_POLL_MS = 10000; // idle

const state = { st: null, timer: null, busy: false };

const cleanMessage = (msg) => String(msg || '').replace(/^validation failed: /, '');

// -----------------------------------------------------------------------------
// Data
// -----------------------------------------------------------------------------

async function load() {
  clearTimeout(state.timer);
  try {
    state.st = await api('/api/v1/admin/emails/status');
    $('load-error').hidden = true;
    render();
  } catch (err) {
    if (err.status === 401 || err.body?.code === 'PASSWORD_CHANGE_REQUIRED') return;
    $('load-error').textContent = `Couldn't load the email queue: ${err.message}`;
    $('load-error').hidden = false;
  }
  const active = state.st && (state.st.pending + state.st.sending) > 0 && !state.st.paused;
  state.timer = setTimeout(() => {
    if (document.visibilityState === 'visible') load();
    else state.timer = setTimeout(load, SLOW_POLL_MS);
  }, active ? FAST_POLL_MS : SLOW_POLL_MS);
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

function fmtDuration(ms) {
  const m = Math.max(0, Math.round(ms / 60000));
  if (m < 1) return 'under a minute';
  if (m < 60) return `${m} min`;
  return `${Math.floor(m / 60)} h ${String(m % 60).padStart(2, '0')} min`;
}

function render() {
  const s = state.st;
  const total = s.sent + s.pending + s.sending + s.failed;
  const waiting = s.pending + s.sending;

  $('disabled-note').hidden = s.enabled;

  // Paused banner: by an organizer, or by the worker with a reason.
  $('paused-note').hidden = !s.paused;
  if (s.paused) {
    $('paused-text').replaceChildren(
      s.pause_reason
        ? el('span', {}, el('b', {}, 'Sending stopped by itself. '), s.pause_reason)
        : el('span', {}, el('b', {}, 'Sending is paused. '), `Paused ${fmtTime(s.updated_at)}. Queued emails wait until you resume.`),
    );
  }
  $('limit-note').hidden = !s.waiting_for_daily_limit;
  $('limit-note').textContent = s.waiting_for_daily_limit
    ? `Daily limit reached: ${fmtInt(s.sent_last_24h)} emails sent in the last 24 hours (limit ${fmtInt(s.daily_limit)}). Sending continues automatically as the oldest ones pass 24 hours.`
    : '';

  // Worker pill
  let pill = ['Not configured', 'border-stop/50 text-stop'];
  if (s.enabled && s.paused) pill = ['Paused', 'border-hold/60 text-hold'];
  else if (s.enabled && s.waiting_for_daily_limit) pill = ['Waiting for daily limit', 'border-hold/60 text-hold'];
  else if (s.enabled && waiting > 0) pill = [`Sending · 1 every ${s.send_interval_seconds} s`, 'border-action/60 text-action'];
  else if (s.enabled) pill = ['Idle', 'border-line text-ink-soft'];
  $('worker-pill').textContent = pill[0];
  $('worker-pill').className = `pill border ${pill[1]}`;

  const pause = $('pause-btn');
  pause.disabled = !s.enabled || state.busy;
  pause.textContent = s.paused ? 'Resume sending' : 'Pause sending';
  pause.className = s.paused ? 'btn-primary' : 'btn-ghost';

  // Progress
  const pct = (n) => (total ? (n / total) * 100 : 0);
  $('bar-sent').style.width = `${pct(s.sent)}%`;
  $('bar-failed').style.width = `${pct(s.failed)}%`;
  $('progress').setAttribute('aria-valuenow', String(Math.round(pct(s.sent + s.failed))));
  $('progress-label').textContent = total
    ? `${fmtInt(s.sent)} of ${fmtInt(total)} sent${s.failed ? `, ${fmtInt(s.failed)} failed` : ''}`
    : 'Nothing queued yet';
  let eta = '';
  if (s.estimated_finish) eta = `${fmtInt(waiting)} to go · done in about ${fmtDuration(new Date(s.estimated_finish) - Date.now())}`;
  else if (waiting && s.paused) eta = `${fmtInt(waiting)} waiting while paused`;
  else if (waiting && s.daily_limit) eta = `${fmtInt(waiting)} to go · some wait for the daily limit`;
  $('progress-eta').textContent = eta;

  $('n-total').textContent = fmtInt(total);
  $('n-sent').textContent = fmtInt(s.sent);
  $('n-sent-sub').textContent = s.last_sent_at ? `Last ${fmtTime(s.last_sent_at, true)}` : 'None yet';
  $('n-pending').textContent = fmtInt(waiting);
  $('n-pending-sub').textContent = s.retrying ? `${fmtInt(s.retrying)} waiting to retry` : 'In line to send';
  $('n-failed').textContent = fmtInt(s.failed);

  $('not-emailed').textContent = s.not_yet_emailed
    ? `${fmtInt(s.not_yet_emailed)} of ${fmtInt(s.confirmed_delegates)} confirmed delegates haven't been sent a pass yet.`
    : `All ${fmtInt(s.confirmed_delegates)} confirmed delegates have a pass email sent or queued.`;
  $('cap-line').textContent = s.daily_limit
    ? `${fmtInt(s.sent_last_24h)} of ${fmtInt(s.daily_limit)} emails used in the last 24 hours (Gmail's daily allowance, shared with staff emails).`
    : `${fmtInt(s.sent_last_24h)} emails sent in the last 24 hours.`;
  $('bulk-btn').disabled = !s.enabled || s.not_yet_emailed === 0 || state.busy;
  $('bulk-btn').textContent = s.not_yet_emailed
    ? `Bulk enqueue ${fmtInt(s.not_yet_emailed)} confirmation pass${s.not_yet_emailed === 1 ? '' : 'es'}`
    : 'Bulk enqueue confirmation passes';
  $('single-btn').disabled = !s.enabled;

  renderFailures(s.recent_failures);
}

function renderFailures(list) {
  $('fail-rows').replaceChildren(...list.map((f) => el('tr', {},
    el('td', { class: 'td' }, el('div', { class: 'font-bold' }, f.full_name),
      el('div', { class: 'text-xs tabular-nums text-ink-soft' }, f.membership_id)),
    el('td', { class: 'td' }, f.recipient_email),
    el('td', { class: 'td tabular-nums' }, String(f.attempts)),
    el('td', { class: 'td max-w-[340px] text-xs text-ink-soft' }, f.last_error || '–'),
    el('td', { class: 'td whitespace-nowrap text-ink-soft' }, fmtTime(f.updated_at)),
    el('td', { class: 'td text-right' }, el('button', {
      type: 'button', class: 'btn-ghost min-h-[34px] px-3 text-xs', disabled: !state.st.enabled,
      onclick: (e) => retry(f, e.currentTarget),
    }, 'Retry')),
  )));
  $('fail-empty').hidden = list.length > 0;
}

// -----------------------------------------------------------------------------
// Actions
// -----------------------------------------------------------------------------

async function togglePause() {
  const next = !state.st.paused;
  state.busy = true;
  render();
  try {
    await api('/api/v1/admin/emails/pause-toggle', { method: 'POST', body: { paused: next } });
    toast(next ? 'Sending paused. Queued emails will wait.' : 'Sending resumed.', next ? 'warn' : 'ok');
  } catch (err) {
    if (err.status !== 401) toast(cleanMessage(err.message), 'error');
  } finally {
    state.busy = false;
    load();
  }
}

function confirmBulk() {
  const s = state.st;
  const n = s.not_yet_emailed;
  const minutes = (n * s.send_interval_seconds) / 60;
  const room = s.daily_limit ? Math.max(0, s.daily_limit - s.sent_last_24h) : Infinity;
  $('bulk-title').textContent = `Email passes to ${fmtInt(n)} delegate${n === 1 ? '' : 's'}?`;
  const lines = [
    `Each confirmed delegate who hasn't had one gets an email with their QR pass, membership ID, team, venue and a link to the pass page.`,
    `They go out one every ${s.send_interval_seconds} s, so this takes about ${fmtDuration(minutes * 60000)}. You can pause at any time.`,
  ];
  if (n > room) {
    lines.push(`Only ${fmtInt(room)} more can go out in the next 24 hours (daily limit ${fmtInt(s.daily_limit)}). The rest wait and continue automatically.`);
  }
  if (s.paused) lines.push('Sending is paused: the emails will be queued but not sent until you resume.');
  $('bulk-body').replaceChildren(...lines.map((l) => el('p', {}, l)));
  const dlg = $('bulk-dialog');
  dlg.returnValue = '';
  dlg.showModal();
  dlg.addEventListener('close', () => { if (dlg.returnValue === 'ok') bulk(); }, { once: true });
}

async function bulk() {
  state.busy = true;
  render();
  try {
    const res = await api('/api/v1/admin/emails/send-bulk', { method: 'POST' });
    toast(res.message, res.queued ? 'ok' : 'warn');
  } catch (err) {
    if (err.status !== 401) toast(cleanMessage(err.message), 'error');
  } finally {
    state.busy = false;
    load();
  }
}

function singleMsg(text, tone) {
  const m = $('single-msg');
  m.textContent = text;
  m.className = `mt-2 text-sm ${tone === 'error' ? 'font-bold text-stop' : 'text-go'}`;
  m.hidden = !text;
}

async function sendSingle(e) {
  e.preventDefault();
  const id = $('single-id').value.trim();
  if (!id) { singleMsg('Enter a membership ID.', 'error'); return; }
  $('single-btn').disabled = true;
  try {
    const res = await api('/api/v1/admin/emails/send-single', { method: 'POST', body: { membership_id: id } });
    singleMsg(res.message, 'ok');
    $('single-id').value = '';
  } catch (err) {
    if (err.status === 401) return;
    singleMsg(err.status === 404 ? `No delegate has membership ID ${id}.` : cleanMessage(err.message), 'error');
  } finally {
    $('single-btn').disabled = false;
    load();
  }
}

async function retry(f, btn) {
  btn.disabled = true;
  try {
    const res = await api('/api/v1/admin/emails/send-single', { method: 'POST', body: { participant_id: f.participant_id } });
    toast(res.message);
  } catch (err) {
    if (err.status !== 401) toast(cleanMessage(err.message), 'error');
  } finally {
    load();
  }
}

// -----------------------------------------------------------------------------
// Boot
// -----------------------------------------------------------------------------

function toSignIn(message) {
  if (message) sessionStorage.setItem('nsac_signin_message', message);
  location.replace('/admin/dashboard.html');
}

function boot() {
  const session = getSession();
  if (!session) return toSignIn();
  if (!MANAGER_ROLES.includes(session.role)) {
    clearSession();
    return toSignIn('Scanner accounts can’t open the control panel.');
  }
  $('signed-in-as').textContent = session.full_name;
  $('app').hidden = false;

  window.addEventListener(AUTH_EVENT, (e) => toSignIn(e.detail?.message));
  $('logout').addEventListener('click', () => { clearSession(); toSignIn(); });
  $('pause-btn').addEventListener('click', togglePause);
  $('bulk-btn').addEventListener('click', confirmBulk);
  $('single-form').addEventListener('submit', sendSingle);
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') load(); });

  load();
}

boot();
