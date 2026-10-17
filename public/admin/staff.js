// =============================================================================
// Staff accounts: list, provision (emails credentials), edit role/checkpoints,
// resend credentials, deactivate. Changes are superadmin-only; the server
// enforces that, this page only hides controls the viewer can't use.
// =============================================================================
import {
  $, MANAGER_ROLES, ROLE_LABELS, AUTH_EVENT, getSession, clearSession, api,
  el, fmtTime, debounce, toast,
} from './common.js';

const state = {
  staff: [],
  scenarios: [],
  isSuper: false,
  me: '',
  editing: null,       // StaffMember being edited, or null when provisioning
  filters: { q: '', role: '', status: '' },
};

// -----------------------------------------------------------------------------
// Data
// -----------------------------------------------------------------------------

async function load() {
  $('staff-error').hidden = true;
  try {
    const [staff, scenarios] = await Promise.all([
      api('/api/v1/admin/staff/list'),
      api('/api/v1/scenarios'),
    ]);
    state.staff = staff;
    state.scenarios = scenarios;
    render();
  } catch (err) {
    if (err.status === 401 || err.body?.code === 'PASSWORD_CHANGE_REQUIRED') return;
    $('staff-error').textContent = err.status === 403
      ? 'Your account can’t view staff. Ask a superadmin.'
      : `Could not load staff: ${err.message}`;
    $('staff-error').hidden = false;
  }
}

/** Status shown in the table: deactivated beats waiting-for-first-sign-in. */
function statusOf(m) {
  if (!m.is_active) return 'disabled';
  if (m.force_password_change) return 'invited';
  return 'active';
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

function render() {
  renderSummary();
  const { q, role, status } = state.filters;
  const needle = q.trim().toLowerCase();
  const rows = state.staff.filter((m) =>
    (!role || m.role === role) &&
    (!status || statusOf(m) === status) &&
    (!needle || m.full_name.toLowerCase().includes(needle) || m.email.includes(needle)));

  $('staff-rows').replaceChildren(...rows.map(staffRow));
  $('staff-empty').hidden = rows.length > 0;
}

function renderSummary() {
  const active = state.staff.filter((m) => m.is_active);
  const scanners = active.filter((m) => m.role === 'scanner');
  const managers = active.length - scanners.length;
  const invited = active.filter((m) => m.force_password_change).length;
  const noGrant = scanners.filter((m) => m.grants.length === 0).length;
  const disabled = state.staff.length - active.length;

  $('sum-active').textContent = String(active.length);
  $('sum-active-sub').textContent = `${managers} admin${managers === 1 ? '' : 's'} · ${disabled} deactivated`;
  $('sum-scanners').textContent = String(scanners.length);
  const covered = new Set(scanners.flatMap((m) => m.grants.map((g) => g.scenario_id)));
  $('sum-scanners-sub').textContent = `Covering ${covered.size} of ${state.scenarios.length} checkpoints`;
  $('sum-invited').textContent = String(invited);
  $('sum-nogrant').textContent = String(noGrant);
  $('sum-nogrant').className = `text-3xl font-bold tabular-nums ${noGrant ? 'text-hold' : ''}`;
  $('sum-nogrant-card').classList.toggle('border-hold/50', noGrant > 0);
}

const ROLE_PILL = {
  superadmin: 'pill bg-stop/10 text-stop',
  admin: 'pill bg-action/10 text-action',
  scanner: 'pill border border-line text-ink-soft',
};

const STATUS_PILL = {
  active: ['pill bg-go/15 text-go', '✓ Active'],
  invited: ['pill bg-hold/15 text-hold', '✉ Waiting for first sign-in'],
  disabled: ['pill border border-line text-ink-soft', '○ Deactivated'],
};

function grantBadges(m) {
  if (MANAGER_ROLES.includes(m.role)) {
    return el('span', { class: 'text-ink-soft' }, 'All checkpoints');
  }
  if (m.grants.length === 0) {
    return el('span', { class: 'pill bg-hold/15 text-hold' }, '⚠ None: can’t scan');
  }
  return el('div', { class: 'flex max-w-[340px] flex-wrap gap-1' }, m.grants.map((g) =>
    el('span', {
      class: g.is_active ? 'pill border border-action/40 text-action' : 'pill border border-line text-ink-soft line-through',
      title: g.is_active ? g.code : `${g.code} (switched off)`,
    }, g.name)));
}

function staffRow(m) {
  const self = m.id === state.me;
  const [statusCls, statusText] = STATUS_PILL[statusOf(m)];

  const actions = el('div', { class: 'flex justify-end gap-1.5' });
  if (state.isSuper) {
    actions.append(
      el('button', { type: 'button', class: 'btn-ghost min-h-[32px] px-3 text-xs', onclick: () => openEdit(m) }, 'Edit'),
      el('button', {
        type: 'button',
        class: 'btn-ghost min-h-[32px] px-3 text-xs',
        disabled: self || !m.is_active,
        title: self ? 'Change your own password from your sign-in' : (!m.is_active ? 'Reactivate first' : 'Email a new temporary password'),
        onclick: () => resend(m),
      }, 'Resend credentials'),
      el('button', {
        type: 'button',
        class: m.is_active ? 'btn-danger min-h-[32px] px-3 text-xs' : 'btn-ghost min-h-[32px] px-3 text-xs',
        disabled: self,
        title: self ? 'You can’t deactivate your own account' : null,
        onclick: () => toggleActive(m),
      }, m.is_active ? 'Deactivate' : 'Reactivate'),
    );
  }

  return el('tr', { class: m.is_active ? '' : 'opacity-60' },
    el('td', { class: 'td' },
      el('div', { class: 'font-bold' }, m.full_name, self ? el('span', { class: 'pill ml-2 bg-line/60 text-ink-soft' }, 'You') : null),
      m.created_by_name ? el('div', { class: 'text-xs text-ink-soft' }, `Added by ${m.created_by_name}`) : null),
    el('td', { class: 'td' }, m.email),
    el('td', { class: 'td' }, el('span', { class: ROLE_PILL[m.role] || ROLE_PILL.scanner }, ROLE_LABELS[m.role] || m.role)),
    el('td', { class: 'td' }, grantBadges(m)),
    el('td', { class: 'td' }, el('span', { class: statusCls }, statusText)),
    el('td', { class: 'td whitespace-nowrap' }, m.last_login_at ? fmtTime(m.last_login_at) : el('span', { class: 'text-ink-soft' }, 'Never')),
    el('td', { class: 'td' }, actions));
}

// -----------------------------------------------------------------------------
// Provision / edit modal
// -----------------------------------------------------------------------------

const selectedRole = () => document.querySelector('input[name="sf-role"]:checked')?.value || 'scanner';

function renderGrantChoices(selected) {
  const box = $('sf-grants');
  box.replaceChildren(...state.scenarios.map((s) =>
    el('label', { class: 'flex cursor-pointer items-start gap-2 rounded-md px-2 py-1.5 hover:bg-paper' },
      el('input', { type: 'checkbox', value: s.id, class: 'mt-1 h-4 w-4', checked: selected.has(s.id) }),
      el('span', { class: 'text-sm' },
        el('b', {}, s.name), ' ',
        el('span', { class: 'text-xs text-ink-soft' }, s.code, s.is_active ? '' : ' · off')))));
  if (state.scenarios.length === 0) {
    box.append(el('p', { class: 'p-2 text-sm text-ink-soft' }, 'No checkpoints exist yet. Create them on the dashboard first.'));
  }
  updateGrantNote();
}

function checkedGrants() {
  return [...$('sf-grants').querySelectorAll('input[type=checkbox]:checked')].map((c) => c.value);
}

function updateGrantNote() {
  const n = checkedGrants().length;
  const note = $('sf-grants-note');
  note.className = `mt-1 text-xs ${n ? 'text-ink-soft' : 'font-bold text-hold'}`;
  note.textContent = n
    ? `${n} checkpoint${n === 1 ? '' : 's'} selected. The scanner only shows these.`
    : '⚠ No checkpoints selected: this account will be able to sign in but not scan.';
}

/** Show checkpoints for scanners; explain that managers need none. */
function syncRoleUI() {
  const manager = MANAGER_ROLES.includes(selectedRole());
  $('sf-grants-set').hidden = manager;
  $('sf-manager-note').hidden = !manager;

  const note = $('sf-role-note');
  const e = state.editing;
  if (e && e.role !== selectedRole() && MANAGER_ROLES.includes(e.role) && !manager) {
    note.textContent = 'Moving to Scanner removes dashboard access within 30 seconds.';
    note.hidden = false;
  } else if (e && e.role === 'scanner' && manager) {
    note.textContent = 'This gives dashboard access and removes the checkpoint list (admins scan everywhere).';
    note.hidden = false;
  } else {
    note.hidden = true;
  }
}

function showForm() {
  $('staff-form').hidden = false;
  $('staff-success').hidden = true;
  $('sf-error').hidden = true;
}

function openProvision() {
  state.editing = null;
  showForm();
  $('staff-form').reset();
  $('staff-dialog-title').textContent = 'Provision staff';
  $('staff-dialog-lede').textContent = 'They get an email with a temporary password and set their own at first sign-in.';
  $('sf-email').disabled = false;
  $('sf-email-note').hidden = true;
  $('sf-submit').textContent = 'Create account & email credentials';
  document.querySelectorAll('input[name="sf-role"]').forEach((r) => { r.disabled = false; });
  renderGrantChoices(new Set());
  syncRoleUI();
  $('staff-dialog').showModal();
  $('sf-name').focus();
}

function openEdit(m) {
  state.editing = m;
  showForm();
  $('staff-dialog-title').textContent = `Edit ${m.full_name}`;
  $('staff-dialog-lede').textContent = 'Changes reach signed-in devices within 30 seconds.';
  $('sf-name').value = m.full_name;
  $('sf-email').value = m.email;
  $('sf-email').disabled = true;
  $('sf-email-note').hidden = false;
  $('sf-submit').textContent = 'Save changes';
  const self = m.id === state.me;
  document.querySelectorAll('input[name="sf-role"]').forEach((r) => {
    r.checked = r.value === m.role;
    r.disabled = self && r.value !== m.role; // can't change your own role
  });
  renderGrantChoices(new Set(m.grants.map((g) => g.scenario_id)));
  syncRoleUI();
  $('staff-dialog').showModal();
  $('sf-name').focus();
}

function formError(msg) {
  $('sf-error').textContent = msg.replace(/^validation failed: /, '');
  $('sf-error').hidden = false;
}

async function submitStaff(e) {
  e.preventDefault();
  $('sf-error').hidden = true;

  const name = $('sf-name').value.trim();
  const email = $('sf-email').value.trim();
  const role = selectedRole();
  const grants = MANAGER_ROLES.includes(role) ? [] : checkedGrants();
  if (!name) return formError('Enter their full name.');
  if (!state.editing && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) return formError('Enter a valid email address.');

  const btn = $('sf-submit');
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = state.editing ? 'Saving…' : 'Creating & sending email…';
  try {
    if (state.editing) {
      const updated = await api(`/api/v1/admin/staff/${encodeURIComponent(state.editing.id)}`, {
        method: 'PATCH',
        body: { full_name: name, role, scenario_ids: grants },
      });
      $('staff-dialog').close();
      toast(`Saved ${updated.full_name}.`);
    } else {
      const res = await api('/api/v1/admin/staff/create', {
        method: 'POST',
        body: { full_name: name, email, role, scenario_ids: grants },
      });
      showSuccess(res);
    }
    await load();
  } catch (err) {
    if (err.status === 401) return;
    if (err.status === 503) {
      formError(`${err.message} No account was created.`);
    } else {
      formError(err.message);
    }
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

function showSuccess(res) {
  const m = res.staff;
  $('staff-form').hidden = true;
  $('staff-success').hidden = false;
  $('success-email').textContent = res.email_sent_to;
  $('success-detail').textContent = m.role === 'scanner'
    ? `${m.full_name} can scan at ${m.grants.length} checkpoint${m.grants.length === 1 ? '' : 's'}.`
    : `${m.full_name} is now ${m.role === 'superadmin' ? 'a superadmin' : 'an admin'}.`;
  $('success-title').focus();
}

// -----------------------------------------------------------------------------
// Row actions
// -----------------------------------------------------------------------------

/** Native-dialog confirm. Resolves true when the user presses the OK button. */
function confirmAction({ title, body, ok, danger = false }) {
  const dlg = $('confirm-dialog');
  $('confirm-title').textContent = title;
  $('confirm-body').textContent = body;
  $('confirm-ok').textContent = ok;
  $('confirm-ok').className = danger ? 'btn-danger' : 'btn-primary';
  dlg.returnValue = '';
  dlg.showModal();
  return new Promise((resolve) => {
    dlg.addEventListener('close', () => resolve(dlg.returnValue === 'ok'), { once: true });
  });
}

async function resend(m) {
  const ok = await confirmAction({
    title: `Send new credentials to ${m.full_name}?`,
    body: `A new temporary password goes to ${m.email}. Their current password stops working, and signed-in devices ask them to set a new one within 30 seconds. Unsynced scans stay on their phone.`,
    ok: 'Send new credentials',
  });
  if (!ok) return;
  try {
    const res = await api(`/api/v1/admin/staff/${encodeURIComponent(m.id)}/resend-credentials`, { method: 'POST' });
    toast(`New credentials emailed to ${res.email_sent_to}.`);
    await load();
  } catch (err) {
    if (err.status !== 401) toast(err.message.replace(/^validation failed: /, ''), 'error');
  }
}

async function toggleActive(m) {
  const next = !m.is_active;
  if (!next) {
    const ok = await confirmAction({
      title: `Deactivate ${m.full_name}?`,
      body: 'They are signed out everywhere within 30 seconds and can’t sign in again until reactivated. Scans they already uploaded are kept; scans still on their phone can’t upload while deactivated.',
      ok: 'Deactivate',
      danger: true,
    });
    if (!ok) return;
  }
  try {
    await api(`/api/v1/admin/staff/${encodeURIComponent(m.id)}`, { method: 'PATCH', body: { is_active: next } });
    toast(`${m.full_name} ${next ? 'reactivated' : 'deactivated'}.`, next ? 'ok' : 'warn');
    await load();
  } catch (err) {
    if (err.status !== 401) toast(err.message.replace(/^validation failed: /, ''), 'error');
  }
}

// -----------------------------------------------------------------------------
// Boot
// -----------------------------------------------------------------------------

function toSignIn(message) {
  // Sign-in (including the first-password step) lives on the dashboard page.
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

  state.me = session.admin_id;
  state.isSuper = session.role === 'superadmin';
  $('signed-in-as').textContent = session.full_name;
  $('readonly-note').hidden = state.isSuper;
  $('open-provision').hidden = !state.isSuper;
  $('app').hidden = false;

  window.addEventListener(AUTH_EVENT, (e) => toSignIn(e.detail?.message));
  $('logout').addEventListener('click', () => { clearSession(); toSignIn(); });

  const onSearch = debounce(() => { state.filters.q = $('s-search').value; render(); }, 150);
  $('s-search').addEventListener('input', onSearch);
  $('s-role').addEventListener('change', (e) => { state.filters.role = e.target.value; render(); });
  $('s-status').addEventListener('change', (e) => { state.filters.status = e.target.value; render(); });

  $('open-provision').addEventListener('click', openProvision);
  $('staff-form').addEventListener('submit', submitStaff);
  $('sf-roles').addEventListener('change', syncRoleUI);
  $('sf-grants').addEventListener('change', updateGrantNote);
  $('sf-all').addEventListener('click', () => {
    $('sf-grants').querySelectorAll('input[type=checkbox]').forEach((c) => { c.checked = true; });
    updateGrantNote();
  });
  $('sf-none').addEventListener('click', () => {
    $('sf-grants').querySelectorAll('input[type=checkbox]').forEach((c) => { c.checked = false; });
    updateGrantNote();
  });
  $('success-another').addEventListener('click', openProvision);
  $('staff-dialog').querySelectorAll('[data-close]').forEach((b) => b.addEventListener('click', () => $('staff-dialog').close()));

  load();
  // Pick up first sign-ins and changes made by other superadmins.
  setInterval(() => { if (document.visibilityState === 'visible') load(); }, 30_000);
}

boot();
