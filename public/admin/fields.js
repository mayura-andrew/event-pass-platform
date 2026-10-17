// =============================================================================
// Custom fields: list the keys found in delegate CSVs, rename them, and choose
// whether each appears in the scanner popup and the dashboard filter bar.
// Deleting a field (superadmin) also erases its values.
// =============================================================================
import {
  $, MANAGER_ROLES, AUTH_EVENT, getSession, clearSession, api, el, fmtInt, toast,
  FIELD_TYPE_LABELS, RESERVED_KEYS, HEADER_ALIASES, fieldKey,
} from './common.js';

const SOURCE_LABELS = { seed: 'Built in', manual: 'Added here', csv: 'From a CSV' };

const state = { fields: [], isSuper: false, busy: new Set() };

const cleanMessage = (msg) => String(msg || '').replace(/^validation failed: /, '');

// -----------------------------------------------------------------------------
// Data
// -----------------------------------------------------------------------------

async function load() {
  $('fields-error').hidden = true;
  try {
    state.fields = await api('/api/v1/admin/fields');
    render();
  } catch (err) {
    if (err.status === 401 || err.body?.code === 'PASSWORD_CHANGE_REQUIRED') return;
    $('fields-error').textContent = `Could not load fields: ${err.message}`;
    $('fields-error').hidden = false;
  }
}

async function patch(f, body, success) {
  state.busy.add(f.field_key);
  render();
  try {
    const updated = await api(`/api/v1/admin/fields/${encodeURIComponent(f.field_key)}`, { method: 'PATCH', body });
    Object.assign(f, updated);
    if (success) toast(success);
    await load(); // usage counts and filter options may change
  } catch (err) {
    if (err.status !== 401) toast(cleanMessage(err.message), 'error');
  } finally {
    state.busy.delete(f.field_key);
    render();
  }
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

function render() {
  $('field-rows').replaceChildren(...state.fields.map(fieldRow));
  $('fields-empty').hidden = state.fields.length > 0;
  $('field-count').textContent = state.fields.length ? `(${state.fields.length})` : '';
}

/** A checkbox with its state spelled out, so it doesn't rely on the tick alone. */
function toggle(f, prop, label) {
  const on = f[prop];
  return el('label', { class: 'inline-flex cursor-pointer items-center gap-2' },
    el('input', {
      type: 'checkbox',
      class: 'h-5 w-5 accent-[rgb(var(--action))]',
      checked: on,
      disabled: state.busy.has(f.field_key),
      'aria-label': `${label}: ${f.field_label}`,
      onchange: (e) => onToggle(f, prop, e.target),
    }),
    el('span', { class: on ? 'font-bold' : 'text-ink-soft' }, on ? 'On' : 'Off'));
}

function fieldRow(f) {
  const busy = state.busy.has(f.field_key);

  const labelInput = el('input', {
    class: 'field min-h-[36px] py-1',
    value: f.field_label,
    maxlength: 100,
    disabled: busy,
    'aria-label': `Label for ${f.field_key}`,
    onkeydown: (e) => { if (e.key === 'Enter') { e.preventDefault(); e.target.blur(); } if (e.key === 'Escape') { e.target.value = f.field_label; e.target.blur(); } },
    onchange: (e) => {
      const v = e.target.value.replace(/\s+/g, ' ').trim();
      if (!v) { e.target.value = f.field_label; return; }
      if (v !== f.field_label) patch(f, { field_label: v }, `Renamed to “${v}”.`);
    },
  });

  const used = f.usage_count > 0;
  const typeSelect = el('select', {
    class: 'field min-h-[36px] py-1',
    disabled: busy || used,
    title: used ? 'Delegates already have values, so the type is fixed' : 'Change type',
    'aria-label': `Type of ${f.field_label}`,
    onchange: (e) => patch(f, { data_type: e.target.value }, `${f.field_label} is now ${FIELD_TYPE_LABELS[e.target.value].toLowerCase()}.`),
  }, ...Object.entries(FIELD_TYPE_LABELS).map(([v, l]) => el('option', { value: v, selected: v === f.data_type }, l)));

  const del = state.isSuper
    ? el('button', {
      type: 'button', class: 'btn-danger min-h-[34px] px-3 text-xs', disabled: busy,
      onclick: () => onDelete(f),
    }, 'Delete')
    : null;

  return el('tr', { class: busy ? 'opacity-60' : '' },
    el('td', { class: 'td w-[260px]' }, labelInput),
    el('td', { class: 'td font-mono text-xs' }, f.field_key),
    el('td', { class: 'td w-[150px]' }, typeSelect),
    el('td', { class: 'td text-right tabular-nums' }, fmtInt(f.usage_count)),
    el('td', { class: 'td text-center' }, toggle(f, 'show_in_scanner_popup', 'Scanner popup')),
    el('td', { class: 'td text-center' }, toggle(f, 'show_in_dashboard_filter', 'Dashboard filter')),
    el('td', { class: 'td whitespace-nowrap text-ink-soft' }, SOURCE_LABELS[f.source] || f.source),
    el('td', { class: 'td text-right' }, del));
}

// -----------------------------------------------------------------------------
// Actions
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

async function onToggle(f, prop, input) {
  const next = input.checked;
  if (prop === 'show_in_scanner_popup' && next) {
    const n = f.usage_count;
    const ok = await confirmAction({
      title: `Show “${f.field_label}” on scanners?`,
      body: `${n ? `The values for ${fmtInt(n)} delegate${n === 1 ? '' : 's'}` : 'Its values'} will be copied to every scanner phone the next time it syncs, and shown to the volunteer after each scan. Only do this if volunteers need it at the checkpoint.`,
      ok: 'Show on scanners',
    });
    if (!ok) { input.checked = false; return; }
  }
  const what = prop === 'show_in_scanner_popup' ? 'scanner popup' : 'dashboard filters';
  patch(f, { [prop]: next }, `${f.field_label} ${next ? 'added to' : 'removed from'} ${what}.`);
}

async function onDelete(f) {
  const n = f.usage_count;
  const ok = await confirmAction({
    title: `Delete “${f.field_label}”?`,
    body: n
      ? `This also erases the ${f.field_label} value of ${fmtInt(n)} delegate${n === 1 ? '' : 's'}. It can't be undone: to get the values back, upload a CSV with the column again.`
      : 'No delegate has a value for it yet.',
    ok: n ? `Delete field and ${fmtInt(n)} value${n === 1 ? '' : 's'}` : 'Delete field',
    danger: true,
  });
  if (!ok) return;
  try {
    const res = await api(`/api/v1/admin/fields/${encodeURIComponent(f.field_key)}`, { method: 'DELETE' });
    toast(`${f.field_label} deleted${res.values_removed ? `, with ${fmtInt(res.values_removed)} values` : ''}.`, 'warn');
    await load();
  } catch (err) {
    if (err.status !== 401) toast(cleanMessage(err.message), 'error');
  }
}

// ---- Add field -----------------------------------------------------------------

function keyProblem(key) {
  if (!key) return 'Use a label with at least one letter.';
  if (RESERVED_KEYS.has(key) || HEADER_ALIASES[key]) return `“${key}” is a built-in delegate column, not a custom field.`;
  if (state.fields.some((f) => f.field_key === key)) return `There is already a field with key ${key}.`;
  return '';
}

function updateKeyPreview() {
  const key = fieldKey($('af-label').value);
  $('af-key').textContent = key || '–';
}

function openAdd() {
  $('add-form').reset();
  $('af-error').hidden = true;
  updateKeyPreview();
  $('add-dialog').showModal();
  $('af-label').focus();
}

async function submitAdd(e) {
  e.preventDefault();
  const label = $('af-label').value.replace(/\s+/g, ' ').trim();
  const key = fieldKey(label);
  const problem = label ? keyProblem(key) : 'Enter a label.';
  if (problem) {
    $('af-error').textContent = problem;
    $('af-error').hidden = false;
    return;
  }
  const btn = $('af-submit');
  btn.disabled = true;
  try {
    const f = await api('/api/v1/admin/fields', {
      method: 'POST',
      body: {
        field_key: key,
        field_label: label,
        data_type: $('af-type').value,
        show_in_scanner_popup: $('af-popup').checked,
        show_in_dashboard_filter: $('af-filter').checked,
      },
    });
    $('add-dialog').close();
    toast(`${f.field_label} added. Use the column name ${f.field_key} in CSVs.`);
    await load();
  } catch (err) {
    if (err.status === 401) return;
    $('af-error').textContent = cleanMessage(err.message);
    $('af-error').hidden = false;
  } finally {
    btn.disabled = false;
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
  state.isSuper = session.role === 'superadmin';
  $('signed-in-as').textContent = session.full_name;
  $('app').hidden = false;

  window.addEventListener(AUTH_EVENT, (e) => toSignIn(e.detail?.message));
  $('logout').addEventListener('click', () => { clearSession(); toSignIn(); });
  $('open-add').addEventListener('click', openAdd);
  $('add-form').addEventListener('submit', submitAdd);
  $('af-label').addEventListener('input', updateKeyPreview);
  $('add-dialog').querySelectorAll('[data-close]').forEach((b) => b.addEventListener('click', () => $('add-dialog').close()));

  load();
}

boot();
