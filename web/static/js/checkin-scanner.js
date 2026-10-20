// Check-in scanner (SaaS, milestone M4): camera or pasted code -> /scan,
// result card with status band, Record / Undo / Add to workshop, manual
// lookup, recent scans (live), and a retry queue for records made while
// the connection dropped (same idempotency key, so retries never double up).
const root = document.getElementById('scanner');
const api = root.dataset.api;
const csrf = root.dataset.csrf;
const eventId = root.dataset.event;
const $ = (id) => document.getElementById(id);
const store = {
  get(k, d) { try { const v = localStorage.getItem(k); return v === null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* private mode */ } },
};

const deviceId = store.get('scanner.device', null) || crypto.randomUUID();
store.set('scanner.device', deviceId);
const cpSelect = $('cp');
const fast = $('fast');
// Start where this phone was last time, else at the main "Check in".
const saved = store.get(`scanner.cp.${eventId}`, '');
const checkInOption = cpSelect.querySelector('option[data-kind="check_in"]');
if (saved && cpSelect.querySelector(`option[value="${CSS.escape(saved)}"]`)) cpSelect.value = saved;
else if (checkInOption) cpSelect.value = checkInOption.value;
fast.checked = store.get('scanner.fast', true);
cpSelect.addEventListener('change', () => { store.set(`scanner.cp.${eventId}`, cpSelect.value); hideResult(); });
fast.addEventListener('change', () => store.set('scanner.fast', fast.checked));

async function call(method, path, body) {
  const res = await fetch(api + path, {
    method, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = res.status === 204 ? {} : await res.json().catch(() => ({}));
  if (!res.ok) throw Object.assign(new Error(data.error || `HTTP ${res.status}`), { status: res.status });
  return data;
}

// ---- Result card ------------------------------------------------------------
const icons = { green: '✓', amber: '!', red: '✕' };
let current = null; // { request, result }

function hideResult() { $('result').hidden = true; current = null; }

function show(result, request) {
  current = { result, request };
  const band = $('r-band');
  band.className = `band band-${result.tone}`;
  $('r-icon').textContent = icons[result.tone] || '';
  $('r-message').textContent = result.message;
  $('r-detail').hidden = !result.detail;
  $('r-detail').textContent = result.detail || '';
  const p = result.participant;
  $('r-name').textContent = p ? p.full_name : '';
  $('r-pass').textContent = p ? p.pass_id : '';
  const dl = $('r-answers');
  dl.replaceChildren();
  for (const a of (p && p.answers) || []) {
    const dt = document.createElement('dt'); dt.textContent = a.Label;
    const dd = document.createElement('dd'); dd.textContent = a.Value;
    dl.append(dt, dd);
  }
  $('r-record').hidden = result.result !== 'ready';
  $('r-undo').hidden = !result.undoable;
  $('r-add').hidden = !result.can_add;
  $('r-add').textContent = result.places_left > 0 ? `Add to workshop (${result.places_left} left)` : 'Add to workshop';
  $('result').hidden = false;
  if (navigator.vibrate) navigator.vibrate(result.tone === 'green' ? 80 : [60, 60, 60]);
}

function showError(msg) {
  show({ tone: 'red', message: msg, result: 'error' }, null);
}

// ---- Scanning -----------------------------------------------------------------
let lastPayload = '';
let lastAt = 0;

async function scan(payload, { record = fast.checked } = {}) {
  if (!cpSelect.value) return showError('Choose a checkpoint first');
  const now = Date.now();
  if (payload === lastPayload && now - lastAt < 3000) return; // the camera sees a code many times a second
  lastPayload = payload;
  lastAt = now;
  const req = { payload, checkpoint_id: cpSelect.value, record, idempotency_key: crypto.randomUUID(),
    device_id: deviceId, client_time: new Date().toISOString() };
  try {
    show(await call('POST', '/scan', req), req);
  } catch (err) {
    if (err.status === undefined && record) { // offline: keep it and retry
      queue.push(req);
      saveQueue();
      showQueued();
      return;
    }
    showError(err.message);
  }
}
window.__scan = (payload, opts) => { lastPayload = ''; return scan(payload, opts); }; // test hook (spec: e2e injects QR text)

$('r-record').addEventListener('click', () => {
  if (!current || !current.request) return;
  lastPayload = '';
  scan(current.request.payload, { record: true });
});

$('r-undo').addEventListener('click', async () => {
  const c = current && current.result.checkin;
  if (!c) return;
  try {
    await call('POST', `/checkins/${c.id}/undo`);
    show({ ...current.result, tone: 'amber', message: 'Undone', detail: '', undoable: false, result: 'undone' }, current.request);
    refreshRecent();
  } catch (err) { showError(err.message); }
});

$('r-add').addEventListener('click', async () => {
  const r = current && current.result;
  const cp = cpSelect.selectedOptions[0];
  if (!r || !r.participant) return;
  try {
    const cps = await call('GET', '/checkpoints');
    const wsId = (cps.find((c) => c.id === cp.value) || {}).workshop_id;
    await call('POST', `/workshops/${wsId}/signups`, { participant_id: r.participant.id });
    const out = await call('POST', `/checkpoints/${cp.value}/record`, { participant_id: r.participant.id,
      idempotency_key: crypto.randomUUID(), device_id: deviceId, client_time: new Date().toISOString() });
    show(out, current.request);
    refreshRecent();
  } catch (err) { showError(err.message); }
});

$('paste-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const v = $('paste').value.trim();
  if (v) { lastPayload = ''; scan(v); }
});

// ---- Camera -------------------------------------------------------------------
let qr = null;
$('cam').addEventListener('click', async () => {
  const btn = $('cam');
  if (qr) {
    await qr.stop().catch(() => {});
    qr = null;
    btn.textContent = 'Start camera';
    return;
  }
  try {
    qr = new window.Html5Qrcode('reader', { verbose: false });
    await qr.start({ facingMode: 'environment' }, { fps: 10, qrbox: { width: 240, height: 240 } }, (text) => scan(text), () => {});
    btn.textContent = 'Stop camera';
    $('cam-error').hidden = true;
  } catch (err) {
    qr = null;
    $('cam-error').textContent = 'The camera could not start. Allow camera access, or type the code below.';
    $('cam-error').hidden = false;
  }
});

// ---- Manual lookup -----------------------------------------------------------
$('lookup-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const q = $('lookup-q').value.trim();
  const list = $('lookup-results');
  list.replaceChildren();
  if (q.length < 2) return;
  try {
    for (const p of await call('GET', `/lookup?q=${encodeURIComponent(q)}`)) {
      const li = document.createElement('li');
      const who = document.createElement('div');
      who.className = 'who';
      const b = document.createElement('b'); b.textContent = p.full_name;
      const s = document.createElement('span'); s.textContent = `${p.pass_id} · ${p.status}`;
      who.append(b, s);
      const btn = document.createElement('button');
      btn.className = 'btn btn-quiet'; btn.type = 'button'; btn.textContent = 'Record';
      btn.addEventListener('click', async () => {
        try {
          show(await call('POST', `/checkpoints/${cpSelect.value}/record`, { participant_id: p.id,
            idempotency_key: crypto.randomUUID(), device_id: deviceId, client_time: new Date().toISOString() }), null);
          refreshRecent();
        } catch (err) { showError(err.message); }
      });
      li.append(who, btn);
      list.append(li);
    }
    if (!list.children.length) list.innerHTML = '<li class="muted">Nobody matches.</li>';
  } catch (err) { showError(err.message); }
});

// ---- Offline queue -----------------------------------------------------------
const queueKey = `scanner.queue.${eventId}`;
const queue = store.get(queueKey, []);
function saveQueue() { store.set(queueKey, queue); }
function showQueued() {
  const n = queue.length;
  $('queue-note').hidden = n === 0;
  $('queue-note').textContent = `${n} scan${n === 1 ? '' : 's'} saved on this phone, sending when the connection is back.`;
}
async function flush() {
  while (queue.length) {
    try {
      await call('POST', '/scan', queue[0]);
    } catch (err) {
      if (err.status === undefined) break; // still offline
    }
    queue.shift(); // sent (or refused by the server: nothing more to do)
    saveQueue();
  }
  showQueued();
}
setInterval(flush, 5000);
window.addEventListener('online', flush);
showQueued();

// ---- Recent scans (live) ----------------------------------------------------
async function refreshRecent() {
  try {
    const rows = await call('GET', '/checkins/recent');
    const ul = $('recent');
    ul.replaceChildren();
    for (const r of rows.slice(0, 15)) {
      const li = document.createElement('li');
      if (r.undone_at) li.className = 'undone';
      const b = document.createElement('b'); b.textContent = r.full_name;
      const s = document.createElement('span');
      s.className = 'muted';
      s.textContent = ` ${r.checkpoint} · ${r.staff_name} · ${new Date(r.created_at).toLocaleTimeString()}${r.undone_at ? ' · undone' : ''}`;
      li.append(b, s);
      ul.append(li);
    }
    if (!rows.length) ul.innerHTML = '<li class="muted">No scans yet.</li>';
  } catch { /* shown on the next change */ }
}
refreshRecent();
const es = new EventSource(api + '/stream');
let timer = null;
es.addEventListener('change', () => { clearTimeout(timer); timer = setTimeout(refreshRecent, 300); });
