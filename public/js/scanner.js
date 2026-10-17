// =============================================================================
// Admin scanner: QR decode -> offline lookup -> verify -> dedupe -> queue -> sync
// =============================================================================
import {
  db, api, getSession, saveSession, clearSession, getMeta, setMeta,
  hydrateLocalManifest, hydrateScenarios, hydrateClaims, hydrateScannerFields, AuthExpiredError,
} from './db.js';

const SYNC_CHUNK = 200;            // server accepts up to 500 per request
const SYNC_INTERVAL_MS = 20_000;   // retry even if the 'online' event never fires
// After a failed upload, wait 5 s, 10 s, 20 s ... up to 5 min before the next
// automatic try, each wait randomized between half and all of it ("equal
// jitter"). When the server struggles, phones back off instead of retrying in
// step every 20 s, and after every scan.
const BACKOFF_BASE_MS = 5_000;
const BACKOFF_MAX_MS = 5 * 60_000;
// When venue Wi-Fi comes back, every phone sees 'online' within the same
// second. Each waits a random 0-8 s first, so the server isn't hit by all of
// them in one burst.
const RECONNECT_SPREAD_MS = 8_000;
const SAME_CODE_COOLDOWN_MS = 4000; // html5-qrcode reports the same code many times per second
const SUCCESS_AUTO_DISMISS_MS = 2200;
const HEARTBEAT_INTERVAL_MS = 60_000; // dashboard marks a phone stale after 2 min
const HEARTBEAT_MIN_GAP_MS = 30_000;  // flushes run every 20 s; don't report on each

let activeScenario = null;   // locked from the scenario dropdown
let qr = null;               // Html5Qrcode instance
let modalOpen = false;
let lastDecoded = { text: '', at: 0 };
let flushing = false;
const backoff = { failures: 0, until: 0 }; // next automatic upload not before `until`
let autoDismissTimer = null;
// Custom fields shown after a scan ({field_key, field_label, data_type}), in
// order. null until loaded once: then every value the manifest carries is
// shown under a label made from its key.
let scannerFields = null;

const $ = (id) => document.getElementById(id);

// -----------------------------------------------------------------------------
// QR decode entry point
// -----------------------------------------------------------------------------

/**
 * Called by html5-qrcode for every decoded frame.
 * Universal QR payload: {"id":"<participant uuid>","sig":"<hex hmac>"}
 */
export async function onQRDecoded(qrText) {
  if (modalOpen) return;
  const now = Date.now();
  if (qrText === lastDecoded.text && now - lastDecoded.at < SAME_CODE_COOLDOWN_MS) return;
  lastDecoded = { text: qrText, at: now };

  // The viewport cover already tells the volunteer to pick a checkpoint;
  // a modal per decoded frame would just get in the way.
  if (!activeScenario) return;

  let payload;
  try {
    payload = JSON.parse(qrText);
  } catch {
    payload = null;
  }
  if (!payload || typeof payload.id !== 'string' || typeof payload.sig !== 'string') {
    renderModal({ status: 'ERROR', title: 'Not a Space Apps pass', message: 'This QR code is not a delegate pass.' });
    return;
  }

  // 1. Instant offline lookup in IndexedDB.
  const participant = await db.participants.get(payload.id);
  if (!participant) {
    renderModal({
      status: 'ERROR',
      title: 'Not on the delegate list',
      message: 'No confirmed delegate matches this pass. Send them to the help desk.',
    });
    return;
  }

  // 2. Verify the signature against the one issued by the server. Without
  //    this check, anyone who learns a delegate's UUID could print a pass.
  if (!timingSafeEqual(payload.sig.toLowerCase(), String(participant.qr_signature).toLowerCase())) {
    renderModal({
      status: 'ERROR',
      title: 'Pass failed verification',
      participant,
      message: 'The signature does not match. The pass may be altered or out of date. Ask for photo ID and send them to the help desk.',
    });
    return;
  }

  await recordScan(participant, { manual: false });
}

/** Manual fallback for a damaged screen or a dead phone: look up by membership ID. */
async function onManualLookup(event) {
  event.preventDefault();
  const input = $('manual-id');
  const membershipId = input.value.trim();
  if (!membershipId || !activeScenario) return;

  const participant =
    (await db.participants.where('membership_id').equals(membershipId).first()) ||
    (await db.participants.where('membership_id').equalsIgnoreCase(membershipId).first());

  input.value = '';
  if (!participant) {
    renderModal({ status: 'ERROR', title: 'Not on the delegate list', message: `No confirmed delegate has membership ID ${membershipId}.` });
    return;
  }
  await recordScan(participant, { manual: true });
}

// -----------------------------------------------------------------------------
// Duplicate validation + queueing
// -----------------------------------------------------------------------------

async function recordScan(participant, { manual }) {
  const session = getSession();
  if (!session) {
    showLogin('Your session ended. Sign in again to keep scanning.');
    return;
  }
  const scenario = activeScenario;
  const adminId = session.admin_id;

  // Check-and-insert runs in one IndexedDB transaction so two rapid scans of
  // the same pass can't both pass the duplicate check.
  const outcome = await db.transaction('rw', db.pending_scans, db.claims, async () => {
    if (!scenario.allow_duplicates) {
      // a) claimed on this device (pending, synced, or lost a race)
      const local = await db.pending_scans
        .where('[participant_id+scenario_id]')
        .equals([participant.id, scenario.id])
        .filter((s) => s.sync_status !== 'invalid')
        .first();
      if (local) return { duplicate: true, at: local.scanned_at_device, by: local.scanned_by_name };

      // b) claimed on another device and already pulled from the server
      const remote = await db.claims.get([participant.id, scenario.id]);
      if (remote) return { duplicate: true, at: remote.synced_at, by: 'another scanner' };
    }

    // Single-claim scenarios use the documented key; repeatable scenarios add a
    // per-scan UUID so every scan is its own row.
    const baseKey = `${adminId}_${participant.id}_${scenario.id}`;
    const idempotencyKey = scenario.allow_duplicates ? `${baseKey}_${uuid()}` : baseKey;

    await db.pending_scans.add({
      idempotency_key: idempotencyKey,
      participant_id: participant.id,
      scenario_id: scenario.id,
      scanned_by_admin_id: adminId,
      scanned_by_name: session.full_name,
      scanned_at_device: new Date().toISOString(),
      manual_entry: manual,
      sync_status: 'pending',
      attempts: 0,
    });
    return { duplicate: false };
  });

  if (outcome.duplicate) {
    renderModal({
      status: 'WARNING',
      title: 'Already claimed',
      participant,
      scenarioName: scenario.name,
      message: `Claimed ${formatTime(outcome.at)} by ${outcome.by}.`,
    });
    return;
  }

  renderModal({
    status: 'SUCCESS',
    title: manual ? 'Approved (manual entry)' : 'Approved',
    participant,
    scenarioName: scenario.name,
  });

  refreshStatusBar();
  flushPendingScans(); // fire and forget
}

// -----------------------------------------------------------------------------
// Background sync engine
// -----------------------------------------------------------------------------

function backoffAfterFailure() {
  backoff.failures += 1;
  const ceiling = Math.min(BACKOFF_BASE_MS * 2 ** (backoff.failures - 1), BACKOFF_MAX_MS);
  backoff.until = Date.now() + ceiling / 2 + Math.random() * (ceiling / 2);
}

function resetBackoff() {
  backoff.failures = 0;
  backoff.until = 0;
}

/**
 * Uploads this admin's pending scans in chunks and applies per-scan results.
 * Safe to call at any time: concurrent calls are collapsed, and a failure
 * leaves every scan pending for the next attempt. Automatic calls respect the
 * backoff; `force` (sign-out, a person tapping retry) ignores it.
 */
export async function flushPendingScans({ force = false } = {}) {
  if (flushing || !navigator.onLine) return;
  if (!force && Date.now() < backoff.until) return;
  const session = getSession();
  if (!session) return;

  flushing = true;
  setSyncIndicator('syncing');
  let stalled = false;
  try {
    // Loop until the queue is empty or a request fails.
    for (;;) {
      const batch = await db.pending_scans
        .where('sync_status').equals('pending')
        .filter((s) => s.scanned_by_admin_id === session.admin_id)
        .limit(SYNC_CHUNK)
        .toArray();
      if (batch.length === 0) break;

      const res = await api('/api/v1/scans/sync', {
        method: 'POST',
        body: {
          scans: batch.map((s) => ({
            idempotency_key: s.idempotency_key,
            participant_id: s.participant_id,
            scenario_id: s.scenario_id,
            scanned_at_device: s.scanned_at_device,
          })),
        },
      });
      if (!res.ok) throw new Error(`Sync HTTP ${res.status}`);

      const { results } = await res.json();
      await applySyncResults(batch, results);

      // Nothing moved out of 'pending' (all FAILED, e.g. database busy): stop
      // and back off instead of hammering the server in a tight loop.
      if (results.every((r) => r.status === 'FAILED')) {
        stalled = true;
        break;
      }
    }
    if (stalled) {
      backoffAfterFailure();
      setSyncIndicator('error');
    } else {
      resetBackoff();
      await setMeta('last_sync_at', new Date().toISOString());
      setSyncIndicator('idle');
    }
  } catch (err) {
    // AuthExpiredError: api() has already shown sign-in with the reason
    // (expired, deactivated, password reset); don't overwrite it here.
    if (!(err instanceof AuthExpiredError)) {
      console.warn('Background sync paused:', err);
      backoffAfterFailure();
    }
    setSyncIndicator('error');
  } finally {
    flushing = false;
    refreshStatusBar();
    sendHeartbeat();
  }
}

// -----------------------------------------------------------------------------
// Device heartbeat (dashboard sync queue health)
// -----------------------------------------------------------------------------
// The server can't see this phone's IndexedDB, so it reports its queue counts
// while online. When a phone goes quiet, the dashboard shows the last counts
// it reported and flags the phone as stale.

const DEVICE_KEY = 'nsac_device_id';
let lastHeartbeatAt = 0;

/** Stable per-browser ID so the dashboard can tell phones apart. */
function deviceId() {
  let id = localStorage.getItem(DEVICE_KEY);
  if (!id) {
    id = uuid();
    localStorage.setItem(DEVICE_KEY, id);
  }
  return id;
}

async function sendHeartbeat({ force = false } = {}) {
  const session = getSession();
  if (!session || !navigator.onLine) return;
  if (!force && Date.now() - lastHeartbeatAt < HEARTBEAT_MIN_GAP_MS) return;
  lastHeartbeatAt = Date.now();

  try {
    // Counted with cursors: after a day offline the queue can hold thousands
    // of rows, and loading them all every minute would be wasted memory.
    const pendingAll = db.pending_scans.where('sync_status').equals('pending');
    const [pendingTotal, mine] = await Promise.all([
      pendingAll.count(),
      db.pending_scans.where('sync_status').equals('pending')
        .filter((s) => s.scanned_by_admin_id === session.admin_id).count(),
    ]);
    const [rejected, invalid, manifest, lastSync] = await Promise.all([
      db.pending_scans.where('sync_status').equals('rejected').count(),
      db.pending_scans.where('sync_status').equals('invalid').count(),
      db.participants.count(),
      getMeta('last_sync_at'),
    ]);
    await api('/api/v1/devices/heartbeat', {
      method: 'POST',
      body: {
        device_id: deviceId(),
        scenario_id: activeScenario?.id || '',
        pending_count: mine,
        pending_others: pendingTotal - mine,
        rejected_count: rejected,
        invalid_count: invalid,
        manifest_count: manifest,
        last_sync_at: lastSync,
        is_online: navigator.onLine,
      },
    });
  } catch (err) {
    // Monitoring only: never let it disturb scanning.
    if (!(err instanceof AuthExpiredError)) console.warn('Heartbeat not sent:', err);
  }
}

async function applySyncResults(batch, results) {
  const byKey = new Map(batch.map((s) => [s.idempotency_key, s]));
  const now = new Date().toISOString();
  const updates = []; // [{key, changes}] for one bulkUpdate
  const claims = [];  // claims won by other devices, for one bulkPut

  for (const r of results) {
    const scan = byKey.get(r.idempotency_key);
    if (!scan) continue;
    switch (r.status) {
      case 'SUCCESS':
      case 'ALREADY_SYNCED':
        updates.push({ key: scan.id, changes: { sync_status: 'synced', synced_at: now } });
        break;
      case 'DUPLICATE_REJECTED':
        // Another device got there first. Keep the row (so this device still
        // treats the claim as taken) and record who won.
        updates.push({ key: scan.id, changes: { sync_status: 'rejected', server_message: r.message, claimed_by: r.claimed_by || null } });
        claims.push({ participant_id: scan.participant_id, scenario_id: scan.scenario_id, synced_at: r.claimed_at || now });
        break;
      case 'INVALID':
        updates.push({ key: scan.id, changes: { sync_status: 'invalid', server_message: r.message } });
        break;
      default: // FAILED: stay pending, retry later
        updates.push({ key: scan.id, changes: { attempts: (scan.attempts || 0) + 1, server_message: r.message } });
    }
  }

  // One IndexedDB transaction, two bulk calls, however large the batch.
  await db.transaction('rw', db.pending_scans, db.claims, async () => {
    if (updates.length) await db.pending_scans.bulkUpdate(updates);
    if (claims.length) await db.claims.bulkPut(claims);
  });
}

/** Full refresh used on start, on reconnect and when the app returns to the foreground. */
async function refreshFromServer() {
  try {
    const [{ count }, , fields] = await Promise.all([hydrateLocalManifest(), hydrateClaims(), hydrateScannerFields()]);
    scannerFields = fields;
    await loadScenarioOptions(await hydrateScenarios());
    if (count === 0 && navigator.onLine) {
      setBanner('The delegate list is empty. Ask an organizer to confirm participants.');
    }
  } catch (err) {
    if (!(err instanceof AuthExpiredError)) console.warn('Refresh failed:', err); // sign-in already shown
  }
  await flushPendingScans();
  refreshStatusBar();
}

// -----------------------------------------------------------------------------
// Result modal
// -----------------------------------------------------------------------------

const STATUS_GLYPH = { SUCCESS: '✓', WARNING: '!', ERROR: '✕' };

/**
 * Renders the full-screen result. Color is never the only signal: each state
 * also has its own glyph, title, vibration pattern and tone.
 *
 * @param {{status:'SUCCESS'|'WARNING'|'ERROR', title:string, participant?:object,
 *          message?:string, scenarioName?:string}} opts
 */
export function renderModal({ status, title, participant, message, scenarioName }) {
  const modal = $('scan-result-modal');
  modal.dataset.status = status.toLowerCase();

  $('modal-glyph').textContent = STATUS_GLYPH[status];
  $('modal-status-title').textContent = title;
  $('modal-message').textContent = message || '';
  $('modal-message').hidden = !message;

  const details = $('modal-details');
  details.hidden = !participant;
  if (participant) {
    $('participant-name').textContent = participant.full_name;
    $('participant-id').textContent = participant.membership_id;
    $('participant-team').textContent = participant.team_name || 'No team';
    $('participant-email').textContent = participant.email;
    $('participant-venue').textContent =
      participant.venue_name === 'Unassigned' ? 'No venue assigned' : `${participant.venue_name}, ${participant.hall_section}`;
    renderAttributes(participant.custom_attributes);
  }
  $('scenario-name').textContent = scenarioName || activeScenario?.name || '';

  modalOpen = true;
  modal.hidden = false;
  pauseCamera();
  feedback(status);
  $('btn-continue').focus();

  clearTimeout(autoDismissTimer);
  modal.classList.remove('auto-dismiss');
  if (status === 'SUCCESS') {
    // Success clears itself so the line keeps moving; warnings and errors
    // wait for the volunteer to read them and tap.
    void modal.offsetWidth; // restart the countdown bar animation
    modal.classList.add('auto-dismiss');
    autoDismissTimer = setTimeout(dismissModalAndResume, SUCCESS_AUTO_DISMISS_MS);
  }
}

const prettyKey = (k) => k.split('_').filter(Boolean).map((w) => w[0].toUpperCase() + w.slice(1)).join(' ');

function fmtAttr(value, type) {
  if (type === 'boolean' || typeof value === 'boolean') return value ? 'Yes' : 'No';
  if (typeof value === 'number') return value.toLocaleString();
  return String(value);
}

/**
 * Custom details an organizer chose to show at checkpoints, as badges
 * ("Food Preference: Vegetarian"). Only fields still marked for the scanner
 * are shown, so turning one off hides it here at once, even before this
 * phone downloads the trimmed delegate list.
 */
function renderAttributes(attrs) {
  const box = $('participant-attrs');
  const values = attrs && typeof attrs === 'object' ? attrs : {};
  const fields = scannerFields ?? Object.keys(values).map((k) => ({ field_key: k, field_label: prettyKey(k) }));
  const badges = fields
    .filter((f) => values[f.field_key] != null && values[f.field_key] !== '')
    .map((f) => {
      const badge = document.createElement('li');
      badge.className = 'attr';
      const label = document.createElement('span');
      label.className = 'attr-label';
      label.textContent = f.field_label;
      const value = document.createElement('span');
      value.className = 'attr-value';
      value.textContent = fmtAttr(values[f.field_key], f.data_type);
      badge.append(label, value);
      return badge;
    });
  box.replaceChildren(...badges);
  box.hidden = badges.length === 0;
}

export function dismissModalAndResume() {
  clearTimeout(autoDismissTimer);
  $('scan-result-modal').hidden = true;
  modalOpen = false;
  lastDecoded = { text: lastDecoded.text, at: Date.now() }; // don't re-fire on the pass still in frame
  resumeCamera();
}

let audioCtx = null;
function feedback(status) {
  const pattern = { SUCCESS: [60], WARNING: [120, 80, 120], ERROR: [300] }[status];
  if (navigator.vibrate) navigator.vibrate(pattern);
  try {
    audioCtx ||= new AudioContext();
    const tones = { SUCCESS: [880], WARNING: [520, 520], ERROR: [220] }[status];
    tones.forEach((freq, i) => {
      const osc = audioCtx.createOscillator();
      const gain = audioCtx.createGain();
      osc.frequency.value = freq;
      osc.type = status === 'ERROR' ? 'square' : 'sine';
      gain.gain.value = 0.12;
      osc.connect(gain).connect(audioCtx.destination);
      const t = audioCtx.currentTime + i * 0.18;
      osc.start(t);
      osc.stop(t + (status === 'ERROR' ? 0.35 : 0.12));
    });
  } catch { /* audio is optional */ }
}

// -----------------------------------------------------------------------------
// Camera
// -----------------------------------------------------------------------------

async function startCamera() {
  if (qr) return;
  const Html5Qrcode = window.Html5Qrcode;
  qr = new Html5Qrcode('reader', { verbose: false });
  try {
    await qr.start(
      { facingMode: 'environment' },
      {
        fps: 12,
        qrbox: (w, h) => {
          const size = Math.floor(Math.min(w, h) * 0.72);
          return { width: size, height: size };
        },
        aspectRatio: 1,
      },
      (text) => { onQRDecoded(text); },
      () => { /* per-frame "no code found" is normal; ignore */ },
    );
    $('camera-error').hidden = true;
  } catch (err) {
    qr = null;
    $('camera-error').hidden = false;
    $('camera-error-text').textContent = String(err).includes('NotAllowed')
      ? 'Camera access is blocked. Allow the camera in your browser settings, then reload.'
      : 'The camera could not start. The scanner needs HTTPS and a rear camera. Use membership ID lookup below meanwhile.';
  }
}

function pauseCamera() {
  try { if (qr?.getState() === 2 /* SCANNING */) qr.pause(true); } catch { /* not running */ }
}

function resumeCamera() {
  try { if (qr?.getState() === 3 /* PAUSED */) qr.resume(); } catch { /* not running */ }
}

async function stopCamera() {
  if (!qr) return;
  try { await qr.stop(); } catch { /* already stopped */ }
  qr.clear();
  qr = null;
}

// -----------------------------------------------------------------------------
// Scenario selection
// -----------------------------------------------------------------------------

const SCENARIO_KEY = 'nsac_active_scenario_id';

async function loadScenarioOptions(list) {
  const select = $('scenario-select');
  const scenarios = list ?? (await db.scenarios.toArray());
  const saved = localStorage.getItem(SCENARIO_KEY);

  // Scanner accounts only receive the checkpoints granted to them.
  select.replaceChildren(new Option(scenarios.length ? 'Choose checkpoint…' : 'No checkpoints assigned to you', ''));
  for (const s of scenarios) {
    select.add(new Option(s.allow_duplicates ? `${s.name} (repeatable)` : s.name, s.id));
  }
  const keep = scenarios.find((s) => s.id === (activeScenario?.id || saved));
  select.value = keep ? keep.id : '';
  setActiveScenario(keep || null);
  if (!scenarios.length) {
    $('scenario-rule').textContent = 'Ask an organizer to assign your checkpoints, then reload this page.';
  }
}

function setActiveScenario(scenario) {
  activeScenario = scenario;
  if (scenario) localStorage.setItem(SCENARIO_KEY, scenario.id);
  document.body.dataset.ready = scenario ? 'yes' : 'no';
  $('scenario-rule').textContent = scenario
    ? (scenario.allow_duplicates ? 'Repeat scans allowed' : 'One claim per delegate')
    : 'Scanning is off until you pick a checkpoint';
}

// -----------------------------------------------------------------------------
// Status bar
// -----------------------------------------------------------------------------

export async function refreshStatusBar() {
  const session = getSession();
  const [directory, pending, rejected, lastSync] = await Promise.all([
    db.participants.count(),
    db.pending_scans.where('sync_status').equals('pending').count(),
    db.pending_scans.where('sync_status').equals('rejected').count(),
    getMeta('last_sync_at'),
  ]);
  const others = session
    ? await db.pending_scans.where('sync_status').equals('pending')
        .filter((s) => s.scanned_by_admin_id !== session.admin_id).count()
    : 0;

  $('stat-online').textContent = navigator.onLine ? 'Online' : 'Offline';
  $('stat-online').dataset.state = navigator.onLine ? 'on' : 'off';
  $('stat-directory').textContent = directory.toLocaleString();
  $('stat-pending').textContent = pending.toLocaleString();
  $('stat-pending').closest('.stat').dataset.alert = pending > 0 ? 'yes' : 'no';
  $('stat-last-sync').textContent = lastSync ? formatTime(lastSync, true) : 'Never';
  $('stat-conflicts').textContent = rejected.toLocaleString();
  $('stat-conflicts').closest('.stat').hidden = rejected === 0;

  if (others > 0) {
    setBanner(`${others} scan${others === 1 ? '' : 's'} by a different volunteer are waiting on this phone. They upload when that volunteer signs in here again.`);
  }
}

function setSyncIndicator(state) {
  $('sync-state').dataset.state = state;
  $('sync-state').textContent = { syncing: 'Uploading…', idle: 'Up to date', error: 'Will retry' }[state];
}

function setBanner(text) {
  const b = $('banner');
  b.textContent = text || '';
  b.hidden = !text;
}

// -----------------------------------------------------------------------------
// Login / logout
// -----------------------------------------------------------------------------

function showLogin(message) {
  stopCamera();
  pendingChange = null;
  $('pwchange-form').hidden = true;
  $('login-form').hidden = false;
  $('login-view').hidden = false;
  $('app-view').hidden = true;
  $('login-error').textContent = message || '';
  $('login-error').hidden = !message;
  $('login-email').focus();
}

async function showApp() {
  const session = getSession();
  scannerFields = await getMeta('scanner_fields', null); // offline start: last known labels
  $('login-view').hidden = true;
  $('app-view').hidden = false;
  $('signed-in-as').textContent = session.full_name;
  await loadScenarioOptions();
  refreshStatusBar();
  startCamera();
  refreshFromServer();
}

// First sign-in with emailed credentials: the token only works for setting a
// password, so it is kept in memory and never saved as the session.
let pendingChange = null;

async function onLogin(event) {
  event.preventDefault();
  const btn = $('login-submit');
  btn.disabled = true;
  $('login-error').hidden = true;
  try {
    if (!navigator.onLine) throw new Error('You are offline. Signing in needs a connection once; after that the scanner works offline.');
    const password = $('login-password').value;
    const res = await fetch('/api/v1/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: $('login-email').value, password }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || `Sign-in failed (HTTP ${res.status})`);
    $('login-password').value = '';
    if (body.force_password_change) {
      pendingChange = { token: body.token, current: password };
      $('login-form').hidden = true;
      $('pwchange-form').hidden = false;
      $('pw-new').focus();
      return;
    }
    saveSession(body);
    await showApp();
  } catch (err) {
    $('login-error').textContent = err.message;
    $('login-error').hidden = false;
  } finally {
    btn.disabled = false;
  }
}

async function onPasswordChange(event) {
  event.preventDefault();
  const err = $('pw-error');
  err.hidden = true;
  const next = $('pw-new').value;
  let problem = '';
  if (next.length < 10) problem = 'Use at least 10 characters.';
  else if (new TextEncoder().encode(next).length > 72) problem = 'That password is too long (72 bytes maximum).';
  else if (next !== $('pw-confirm').value) problem = 'The two passwords don’t match.';
  if (problem) {
    err.textContent = problem;
    err.hidden = false;
    return;
  }

  const btn = $('pw-submit');
  btn.disabled = true;
  try {
    const res = await fetch('/api/v1/auth/change-password', {
      method: 'POST',
      headers: { Authorization: `Bearer ${pendingChange.token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ current_password: pendingChange.current, new_password: next }),
    });
    const body = await res.json().catch(() => ({}));
    if (res.status === 401) {
      showLogin('That sign-in expired. Sign in again with the temporary password.');
      return;
    }
    if (!res.ok) throw new Error((body.error || `Could not set the password (HTTP ${res.status})`).replace(/^validation failed: /, ''));
    pendingChange = null;
    $('pw-new').value = '';
    $('pw-confirm').value = '';
    saveSession(body);
    await showApp();
  } catch (e) {
    err.textContent = e.message;
    err.hidden = false;
  } finally {
    btn.disabled = false;
  }
}

async function onLogout() {
  const session = getSession();
  if (session) {
    await flushPendingScans({ force: true });
    const mine = await db.pending_scans.where('sync_status').equals('pending')
      .filter((s) => s.scanned_by_admin_id === session.admin_id).count();
    if (mine > 0 && !confirm(
      `${mine} scan${mine === 1 ? ' has' : 's have'} not uploaded yet. They stay on this phone and upload next time you sign in here. Sign out anyway?`,
    )) return;
  }
  clearSession();
  showLogin();
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

function timingSafeEqual(a, b) {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

function uuid() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

/** "at 3:02 PM" / "on Sat 9:15 AM" for sentences; bare=true drops the preposition. */
function formatTime(iso, bare = false) {
  const d = new Date(iso);
  const sameDay = d.toDateString() === new Date().toDateString();
  const text = sameDay
    ? d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
    : d.toLocaleString([], { weekday: 'short', hour: 'numeric', minute: '2-digit' });
  if (bare) return text;
  return `${sameDay ? 'at' : 'on'} ${text}`;
}

// -----------------------------------------------------------------------------
// Boot
// -----------------------------------------------------------------------------

function boot() {
  $('login-form').addEventListener('submit', onLogin);
  $('pwchange-form').addEventListener('submit', onPasswordChange);
  $('pw-cancel').addEventListener('click', () => showLogin());
  $('logout').addEventListener('click', onLogout);
  $('manual-form').addEventListener('submit', onManualLookup);
  $('btn-continue').addEventListener('click', dismissModalAndResume);
  $('scenario-select').addEventListener('change', async (e) => {
    setActiveScenario((await db.scenarios.get(e.target.value)) || null);
    sendHeartbeat({ force: true }); // dashboard shows which checkpoint each phone is on
  });
  document.addEventListener('keydown', (e) => {
    if (modalOpen && (e.key === 'Enter' || e.key === 'Escape')) dismissModalAndResume();
  });

  window.addEventListener('online', () => {
    refreshStatusBar();
    resetBackoff(); // the network is back: earlier failures say nothing now
    setTimeout(() => { if (getSession()) refreshFromServer(); }, Math.random() * RECONNECT_SPREAD_MS);
  });
  window.addEventListener('offline', refreshStatusBar);
  window.addEventListener('auth-expired', (e) => showLogin(e.detail?.message || 'Your session ended. Sign in again; unsynced scans are kept on this device.'));
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && getSession()) refreshFromServer();
  });
  setInterval(() => { if (getSession()) flushPendingScans(); }, SYNC_INTERVAL_MS);
  setInterval(() => { if (getSession()) hydrateClaims().then(refreshStatusBar); }, 60_000);
  setInterval(() => { if (getSession()) sendHeartbeat({ force: true }); }, HEARTBEAT_INTERVAL_MS);

  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js').catch((err) => console.warn('Service worker not registered:', err));
  }
  // Ask the browser not to evict IndexedDB under storage pressure.
  navigator.storage?.persist?.();

  if (getSession()) showApp();
  else showLogin();
}

boot();
