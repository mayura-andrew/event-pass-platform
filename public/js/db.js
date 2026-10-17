// =============================================================================
// Local offline store (Dexie.js / IndexedDB), session handling, and hydration.
// =============================================================================
// Dexie is vendored and version-pinned so the service worker can cache it and
// the scanner keeps working with no network. Never load @latest from a CDN in
// a device that has to run offline all day.
import Dexie from '../vendor/dexie-4.0.11.mjs';

export const db = new Dexie('SpaceAppsScannerDB');

db.version(1).stores({
  // Confirmed delegates. Only indexed fields are listed; Dexie stores the
  // whole object regardless, including custom_attributes (the values of the
  // fields an organizer chose to show after a scan, e.g. food preference).
  participants: 'id, membership_id, email',

  // Active scenarios as last fetched from the server.
  scenarios: 'id, code',

  // Every scan this device made. Rows are kept after syncing (status changes
  // to 'synced') so the local duplicate check still works afterwards.
  //   sync_status: 'pending' | 'synced' | 'rejected' | 'invalid'
  pending_scans: '++id, &idempotency_key, sync_status, scanned_by_admin_id, [participant_id+scenario_id]',

  // Single-claim scans recorded on the server by ANY device.
  claims: '[participant_id+scenario_id]',

  // Small key/value store: ETags, watermarks, timestamps, and the labels of
  // the custom fields shown after a scan ('scanner_fields').
  meta: 'key',
});

// -----------------------------------------------------------------------------
// Session
// -----------------------------------------------------------------------------
// The scanner keeps its JWT in localStorage, not sessionStorage: a volunteer
// whose phone kills the browser tab while offline must be able to reopen the
// app and keep scanning. The token's lifetime (JWT_TTL, 14h default) bounds
// the exposure if a device is lost.
const SESSION_KEY = 'nsac_scanner_session';

export function getSession() {
  try {
    const s = JSON.parse(localStorage.getItem(SESSION_KEY) || 'null');
    if (!s || !s.token || new Date(s.expires_at) <= new Date()) return null;
    return s;
  } catch {
    return null;
  }
}

export function saveSession(loginResponse) {
  localStorage.setItem(SESSION_KEY, JSON.stringify({
    token: loginResponse.token,
    expires_at: loginResponse.expires_at,
    admin_id: loginResponse.admin_id,
    full_name: loginResponse.full_name,
    role: loginResponse.role,
  }));
}

export function clearSession() {
  localStorage.removeItem(SESSION_KEY);
}

export class AuthExpiredError extends Error {}

function endSession(message) {
  clearSession();
  window.dispatchEvent(new CustomEvent('auth-expired', { detail: { message } }));
}

/**
 * fetch() wrapper that adds the bearer token and ends the session when the
 * server says it can't continue: expired token, deactivated account (401), or
 * new credentials issued by a superadmin (403 PASSWORD_CHANGE_REQUIRED).
 * Unsynced scans stay in IndexedDB and upload after signing in again.
 */
export async function api(path, { method = 'GET', body, headers = {} } = {}) {
  const session = getSession();
  if (!session) {
    endSession('Your session ended. Sign in again; unsynced scans are kept on this device.');
    throw new AuthExpiredError('Not signed in');
  }

  const res = await fetch(path, {
    method,
    headers: {
      Authorization: `Bearer ${session.token}`,
      ...(body ? { 'Content-Type': 'application/json' } : {}),
      ...headers,
    },
    body: body ? JSON.stringify(body) : undefined,
    cache: 'no-store',
  });
  if (res.status === 401) {
    const body = await res.clone().json().catch(() => ({}));
    if (body.error === 'account has been deactivated') {
      // A deactivated account usually means a lost phone or a volunteer who
      // left: remove the delegate directory (names, emails, pass signatures)
      // now instead of keeping it on the device indefinitely. Unsent scans
      // stay, so nothing is lost if the account is reactivated; the next
      // sign-in downloads the directory again.
      await wipeDirectory();
      endSession('This account has been deactivated, and the delegate list was removed from this phone. Unsent scans are kept; contact the event lead.');
    } else {
      endSession('Your session ended. Sign in again; unsynced scans are kept on this device.');
    }
    throw new AuthExpiredError('Session expired');
  }
  if (res.status === 403) {
    const body = await res.clone().json().catch(() => ({}));
    if (body.code === 'PASSWORD_CHANGE_REQUIRED') {
      endSession('Your password was reset. Sign in with the temporary password from your email; unsynced scans are kept on this device.');
      throw new AuthExpiredError('Password change required');
    }
  }
  return res;
}

/** Removes delegate data from the device, keeping the scan queue. */
export async function wipeDirectory() {
  await db.transaction('rw', db.participants, db.claims, db.meta, async () => {
    await db.participants.clear();
    await db.claims.clear();
    await db.meta.bulkDelete(['manifest_etag', 'claims_watermark', 'manifest_hydrated_at', 'scanner_fields']);
  });
}

export async function getMeta(key, fallback = null) {
  const row = await db.meta.get(key);
  return row ? row.value : fallback;
}

export function setMeta(key, value) {
  return db.meta.put({ key, value });
}

// -----------------------------------------------------------------------------
// Hydration
// -----------------------------------------------------------------------------

/**
 * Downloads the confirmed delegate directory into IndexedDB.
 *
 * The table is replaced inside one transaction (clear + bulkPut): a plain
 * bulkPut would keep delegates who were un-confirmed or removed on the server.
 * The server's ETag makes repeat calls cheap (304, no body).
 *
 * @returns {Promise<{updated: boolean, count: number}>}
 */
export async function hydrateLocalManifest() {
  if (!navigator.onLine) return { updated: false, count: await db.participants.count() };

  try {
    // Only ask "has it changed?" when the phone actually holds a list; with an
    // empty table a 304 would leave it empty for good.
    const etag = (await db.participants.count()) > 0 ? await getMeta('manifest_etag') : null;
    const res = await api('/api/v1/participants/manifest', {
      headers: etag ? { 'If-None-Match': etag } : {},
    });

    if (res.status === 304) {
      await setMeta('manifest_checked_at', new Date().toISOString());
      return { updated: false, count: await db.participants.count() };
    }
    if (!res.ok) throw new Error(`Manifest HTTP ${res.status}`);

    const data = await res.json();
    await db.transaction('rw', db.participants, db.meta, async () => {
      await db.participants.clear();
      await db.participants.bulkPut(data);
      await db.meta.bulkPut([
        { key: 'manifest_etag', value: res.headers.get('ETag') },
        { key: 'manifest_hydrated_at', value: new Date().toISOString() },
        { key: 'manifest_checked_at', value: new Date().toISOString() },
      ]);
    });
    return { updated: true, count: data.length };
  } catch (err) {
    if (err instanceof AuthExpiredError) throw err;
    console.warn('Manifest hydration postponed:', err);
    return { updated: false, count: await db.participants.count() };
  }
}

/**
 * Caches the labels of the custom fields shown after a scan, in display
 * order. The values travel with each delegate in the manifest; the server
 * only includes the fields listed here.
 *
 * @returns {Promise<Array<{field_key:string, field_label:string, data_type:string}>>}
 */
export async function hydrateScannerFields() {
  if (!navigator.onLine) return getMeta('scanner_fields', []);
  try {
    const res = await api('/api/v1/fields/scanner');
    if (!res.ok) throw new Error(`Scanner fields HTTP ${res.status}`);
    const list = await res.json();
    await setMeta('scanner_fields', list);
    return list;
  } catch (err) {
    if (err instanceof AuthExpiredError) throw err;
    console.warn('Scanner field refresh postponed:', err);
    return getMeta('scanner_fields', []);
  }
}

/** Replaces the cached list of active scenarios. */
export async function hydrateScenarios() {
  if (!navigator.onLine) return db.scenarios.toArray();
  try {
    const res = await api('/api/v1/scenarios/active');
    if (!res.ok) throw new Error(`Scenarios HTTP ${res.status}`);
    const list = await res.json();
    await db.transaction('rw', db.scenarios, async () => {
      await db.scenarios.clear();
      await db.scenarios.bulkPut(list);
    });
    return list;
  } catch (err) {
    if (err instanceof AuthExpiredError) throw err;
    console.warn('Scenario refresh postponed:', err);
    return db.scenarios.toArray();
  }
}

// The server stamps claims with transaction start time, and transactions can
// commit out of order, so each request re-reads a few minutes behind the last
// watermark. Re-applying a claim is harmless.
const CLAIM_OVERLAP_MS = 5 * 60 * 1000;

/** Pulls claims made by other devices so duplicates are caught locally. */
export async function hydrateClaims() {
  if (!navigator.onLine) return 0;
  try {
    const watermark = await getMeta('claims_watermark');
    const since = watermark
      ? new Date(new Date(watermark).getTime() - CLAIM_OVERLAP_MS).toISOString()
      : '';
    const res = await api(`/api/v1/scans/claims${since ? `?since=${encodeURIComponent(since)}` : ''}`);
    if (!res.ok) throw new Error(`Claims HTTP ${res.status}`);

    const { claims, server_time } = await res.json();
    await db.transaction('rw', db.claims, db.meta, async () => {
      await db.claims.bulkPut(claims.map((c) => ({
        participant_id: c.participant_id,
        scenario_id: c.scenario_id,
        synced_at: c.synced_at,
      })));
      await db.meta.put({ key: 'claims_watermark', value: server_time });
    });
    return claims.length;
  } catch (err) {
    if (err instanceof AuthExpiredError) throw err;
    console.warn('Claim refresh postponed:', err);
    return 0;
  }
}
