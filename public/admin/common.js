// =============================================================================
// Shared by the dashboard (app.js) and the staff page (staff.js): session,
// API client, DOM helpers, toasts.
// =============================================================================
// Server data reaches the DOM only through textContent (see el()), never
// innerHTML: names, teams, emails and reasons are user-supplied.

// sessionStorage, not localStorage: the dashboard is online-only, so there is
// no reason to keep an organizer token around after the tab closes. Both
// admin pages share this key, so signing in on one signs in on the other.
export const SESSION_KEY = 'nsac_dashboard_session';
export const MANAGER_ROLES = ['admin', 'superadmin'];

export const ROLE_LABELS = { superadmin: 'Superadmin', admin: 'Admin', scanner: 'Scanner' };

export const $ = (id) => document.getElementById(id);

// ---- Session -------------------------------------------------------------------

export function getSession() {
  try {
    const s = JSON.parse(sessionStorage.getItem(SESSION_KEY) || 'null');
    if (!s || !s.token || new Date(s.expires_at) <= new Date()) return null;
    return s;
  } catch {
    return null;
  }
}

export function saveSession(r) {
  sessionStorage.setItem(SESSION_KEY, JSON.stringify({
    token: r.token, expires_at: r.expires_at, admin_id: r.admin_id, full_name: r.full_name, role: r.role,
  }));
}

export function clearSession() {
  sessionStorage.removeItem(SESSION_KEY);
}

/**
 * Fired when the session can't continue: token expired, account deactivated,
 * or a superadmin issued new credentials. Pages listen and show sign-in.
 */
export const AUTH_EVENT = 'nsac-auth-expired';

function endSession(message) {
  clearSession();
  window.dispatchEvent(new CustomEvent(AUTH_EVENT, { detail: { message } }));
}

// ---- API -----------------------------------------------------------------------

export class ApiError extends Error {
  constructor(status, body) {
    super(body?.error || `Request failed (HTTP ${status})`);
    this.status = status;
    this.body = body || {};
  }
}

/** JSON fetch with the bearer token. Throws ApiError for non-2xx responses. */
export async function api(path, { method = 'GET', body, signal, token } = {}) {
  const bearer = token || getSession()?.token;
  if (!bearer) {
    endSession('Your session ended. Sign in again.');
    throw new ApiError(401, { error: 'Not signed in' });
  }
  const res = await fetch(path, {
    method,
    headers: {
      Authorization: `Bearer ${bearer}`,
      ...(body ? { 'Content-Type': 'application/json' } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
    cache: 'no-store',
    signal,
  });
  const data = res.status === 204 ? null : await res.json().catch(() => null);

  if (res.status === 401 && !token) {
    // "account has been deactivated" is worth showing as-is.
    endSession(data?.error === 'account has been deactivated'
      ? 'This account has been deactivated. Contact the event lead.'
      : 'Your session ended. Sign in again.');
    throw new ApiError(401, data);
  }
  if (res.status === 403 && data?.code === 'PASSWORD_CHANGE_REQUIRED' && !token) {
    endSession('Your password was reset. Sign in with the temporary password from your email.');
    throw new ApiError(403, data);
  }
  if (!res.ok) throw new ApiError(res.status, data);
  return data;
}

/** POST /auth/login. Returns the login result (may require a password change). */
export async function signIn(email, password) {
  const res = await fetch('/api/v1/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `Sign-in failed (HTTP ${res.status})`);
  return body;
}

/** Sets a new password using the (temporary-password) token; returns a fresh session. */
export function changePassword(token, currentPassword, newPassword) {
  return api('/api/v1/auth/change-password', {
    method: 'POST',
    token,
    body: { current_password: currentPassword, new_password: newPassword },
  });
}

// ---- DOM helpers ---------------------------------------------------------------

/** Creates an element. Strings become text nodes, so data is never parsed as HTML. */
export function el(tag, props = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v == null || v === false) continue;
    if (k === 'class') n.className = v;
    else if (k === 'text') n.textContent = v;
    else if (k === 'dataset') Object.assign(n.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    n.append(c instanceof Node ? c : String(c));
  }
  return n;
}

export const fmtInt = (n) => Number(n || 0).toLocaleString();

export function fmtTime(iso, withSeconds = false) {
  if (!iso) return '';
  const d = new Date(iso);
  const opts = { hour: 'numeric', minute: '2-digit', ...(withSeconds ? { second: '2-digit' } : {}) };
  return d.toDateString() === new Date().toDateString()
    ? d.toLocaleTimeString([], opts)
    : d.toLocaleString([], { weekday: 'short', ...opts });
}

export function debounce(fn, ms) {
  let t;
  return (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export function toast(message, tone = 'ok') {
  const tones = {
    ok: 'border-go/50',
    warn: 'border-hold/60',
    error: 'border-stop/60',
  };
  const glyph = { ok: '✓', warn: '!', error: '✕' }[tone];
  const t = el('div', { class: `pointer-events-auto card flex gap-2 border-2 ${tones[tone]} px-3 py-2 text-sm shadow-lg`, role: 'status' },
    el('b', { 'aria-hidden': 'true' }, glyph), el('span', {}, message));
  $('toasts').append(t);
  setTimeout(() => t.remove(), 6000);
}

/** Password rules shown to users; the server enforces the same ones. */
export function passwordProblem(next, confirm) {
  if (next.length < 10) return 'Use at least 10 characters.';
  if (new TextEncoder().encode(next).length > 72) return 'That password is too long (72 bytes maximum).';
  if (next !== confirm) return 'The two passwords don’t match.';
  return '';
}
