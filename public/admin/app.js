// =============================================================================
// Space Apps Control Panel: metrics, delegate filters, manual override,
// live scan stream, checkpoint control and scanner fleet health.
// =============================================================================
// Online-only by design (the scanner is the offline app). Every piece of
// server data reaches the DOM through textContent, never innerHTML: names,
// teams and override reasons are user-supplied.

const STATS_INTERVAL_MS = 10_000;
const DEVICES_INTERVAL_MS = 15_000;
const SCENARIOS_INTERVAL_MS = 60_000;
const SEARCH_DEBOUNCE_MS = 300;
const TABLE_REFRESH_DEBOUNCE_MS = 2_000; // after a live scan touches the shown scenario
const STREAM_MAX_ITEMS = 100;
const STREAM_BACKOFF_MAX_MS = 15_000;
const PAGE_SIZE = 50;
const MIN_RATIONALE = 10;

import {
  $, MANAGER_ROLES, AUTH_EVENT, getSession, saveSession, clearSession, api, signIn, changePassword,
  el, fmtInt, fmtTime, debounce, sleep, toast, passwordProblem, fmtCustom,
} from './common.js';

const state = {
  scenarios: [],        // all scenarios, including inactive
  venues: [],
  stats: null,
  rows: [],             // current table page
  filter: {
    search: '', age_group: '', scenario_id: '', claim_status: '',
    venue_id: '', confirmation: 'CONFIRMED', custom: [], page: 1, page_size: PAGE_SIZE,
  },
  filterFields: [],     // custom fields marked "Dashboard filter", with their common values
  customFilter: {},     // field_key -> { op, value } chosen in the filter bar
  total: 0,
  tableSeq: 0,          // drops responses from superseded filter requests
  serverSkewMs: 0,      // server clock minus browser clock, from device health
  timers: [],
  overrideRow: null,
  scenarioDefaulted: false,
};

// -----------------------------------------------------------------------------
// Formatting
// -----------------------------------------------------------------------------

/** "just now", "40 s ago", "3 min ago", "2 h ago", corrected for server clock skew. */
function fmtAgo(iso) {
  if (!iso) return 'Never';
  const s = Math.max(0, Math.round((Date.now() + state.serverSkewMs - new Date(iso).getTime()) / 1000));
  if (s < 10) return 'just now';
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86_400) return `${Math.floor(s / 3600)} h ago`;
  return fmtTime(iso);
}

/** Under-18 badge: icon + words + color, so it never relies on color alone. */
function minorBadge(age) {
  return el('span', { class: 'pill border border-hold/50 bg-hold/15 text-hold', title: 'Under 18: follow the minor safeguarding policy' },
    '⚠️ UNDER 18', age != null ? el('span', { class: 'font-normal' }, `(${age})`) : null);
}

// -----------------------------------------------------------------------------
// Reference data: scenarios and venues
// -----------------------------------------------------------------------------

async function loadScenarios() {
  state.scenarios = await api('/api/v1/scenarios');
  renderScenarioOptions();
  if ($('scenario-drawer').open) renderDrawer();
}

async function loadVenues() {
  state.venues = await api('/api/v1/admin/venues');
  const sel = $('f-venue');
  const keep = sel.value;
  sel.replaceChildren(new Option('All venues', ''), new Option('Unassigned', 'UNASSIGNED'));
  for (const v of state.venues) sel.add(new Option(`${v.name}, ${v.hall_section}`, v.id));
  sel.value = keep;
}

// ---- Custom field filters --------------------------------------------------------

/**
 * Builds one control per filter field: a dropdown of the common values when
 * there are few (exact match), otherwise a text box (contains, for text).
 * Both offer "Not given".
 */
async function loadFilterFields() {
  let all;
  try {
    all = await api('/api/v1/admin/fields');
  } catch (err) {
    if (err.status === 401) throw err;
    console.warn('Custom field filters unavailable:', err); // the rest of the dashboard still works
    return;
  }
  state.filterFields = all.filter((f) => f.show_in_dashboard_filter);
  // Drop choices for fields that are no longer filters.
  for (const key of Object.keys(state.customFilter)) {
    if (!state.filterFields.some((f) => f.field_key === key)) delete state.customFilter[key];
  }
  renderCustomFilters();
}

const MISSING = '__missing__';

function renderCustomFilters() {
  $('f-custom').replaceChildren(...state.filterFields.map((f) => {
    const id = `f-cf-${f.field_key}`;
    const current = state.customFilter[f.field_key];
    let control;
    const useSelect = f.data_type === 'boolean' || (f.options?.length && !f.options_truncated) || f.usage_count === 0;
    if (useSelect) {
      const opts = f.data_type === 'boolean' ? ['true', 'false'] : (f.options || []);
      control = el('select', { id, class: 'field', onchange: (e) => setCustomFilter(f, e.target.value) },
        el('option', { value: '' }, 'Any'),
        ...opts.map((o) => el('option', { value: o }, f.data_type === 'boolean' ? (o === 'true' ? 'Yes' : 'No') : o)),
        el('option', { value: MISSING }, 'Not given'));
      control.value = current ? (current.op === 'missing' ? MISSING : current.value) : '';
    } else {
      control = el('input', {
        id, class: 'field', type: 'search', autocomplete: 'off', maxlength: 100,
        placeholder: f.data_type === 'text' ? 'Contains…' : `Exact ${f.data_type}`,
        value: current?.value || '',
        oninput: debounce((e) => setCustomFilter(f, e.target.value.trim(), true), SEARCH_DEBOUNCE_MS),
      });
    }
    return el('div', {}, el('label', { class: 'label', for: id }, f.field_label), control);
  }));
}

function setCustomFilter(f, value, typed = false) {
  if (!value) delete state.customFilter[f.field_key];
  else if (value === MISSING) state.customFilter[f.field_key] = { op: 'missing', value: '' };
  else if (typed && f.data_type === 'text') state.customFilter[f.field_key] = { op: 'contains', value };
  else state.customFilter[f.field_key] = { op: 'eq', value };
  applyFilter({ custom: Object.entries(state.customFilter).map(([key, c]) => ({ key, ...c })) });
}

function renderScenarioOptions() {
  const sel = $('f-scenario');
  const keep = sel.value || state.filter.scenario_id;
  sel.replaceChildren(new Option('None (hide status)', ''));
  for (const s of state.scenarios) {
    sel.add(new Option(s.is_active ? s.name : `${s.name} (off)`, s.id));
  }
  sel.value = state.scenarios.some((s) => s.id === keep) ? keep : '';
  if (sel.value !== state.filter.scenario_id) {
    state.filter.scenario_id = sel.value;
    syncClaimSelect();
  }
  updateScenarioColumn();
}

const scenarioById = (id) => state.scenarios.find((s) => s.id === id);

function updateScenarioColumn() {
  const s = scenarioById(state.filter.scenario_id);
  $('scenario-col').textContent = s ? `Status: ${s.name}` : 'Scenario status';
}

// -----------------------------------------------------------------------------
// Metric cards + throughput
// -----------------------------------------------------------------------------

let statsBusy = false;

async function loadStats() {
  if (statsBusy) return;
  statsBusy = true;
  try {
    state.stats = await api('/api/v1/admin/dashboard/stats');
    renderStats(state.stats);
    // First load: show the check-in scenario's status in the table by default.
    if (!state.scenarioDefaulted) {
      state.scenarioDefaulted = true;
      const def = state.stats.check_in_scenario_id || state.scenarios.find((s) => s.is_active)?.id || '';
      if (def && !state.filter.scenario_id) {
        $('f-scenario').value = def;
        state.filter.scenario_id = def;
        syncClaimSelect();
        updateScenarioColumn();
      }
    }
    if ($('scenario-drawer').open) renderDrawer();
  } catch (err) {
    if (err.status !== 401) console.warn('Stats refresh failed:', err);
  } finally {
    statsBusy = false;
  }
}

function renderStats(st) {
  const unconfirmed = st.total_participants - st.total_confirmed;
  $('stat-confirmed').textContent = fmtInt(st.total_confirmed);
  $('stat-confirmed-sub').textContent = `${fmtInt(st.total_participants)} registered · ${fmtInt(unconfirmed)} unconfirmed`;

  const pct = st.total_confirmed ? Math.round((st.checked_in / st.total_confirmed) * 100) : 0;
  $('stat-checked-in').textContent = fmtInt(st.checked_in);
  $('stat-checked-in-bar').style.width = `${Math.min(100, pct)}%`;
  $('stat-checked-in-sub').textContent = st.check_in_scenario_id
    ? `${pct}% of confirmed · ${fmtInt(st.total_confirmed - st.checked_in)} still to arrive`
    : `No scenario has code ${st.check_in_scenario_code}`;

  $('stat-minors').textContent = fmtInt(st.minor_count);
  $('stat-minors-sub').textContent =
    `${fmtInt(st.minors_checked_in)} checked in · ${fmtInt(st.unknown_age_count)} with no date of birth`;

  $('stat-scenarios').textContent = `${st.active_scenarios} of ${st.total_scenarios}`;
  const active = st.scenarios.filter((s) => s.is_active).map((s) => s.name);
  $('stat-scenarios-sub').textContent = active.length ? active.join(' · ') : 'All checkpoints are off';

  renderSyncCard(st.sync);
  renderThroughput(st);
}

function renderSyncCard(sync) {
  $('stat-sync').textContent = fmtInt(sync.pending_on_devices);
  const card = $('stat-sync-card');
  const sub = $('stat-sync-sub');
  card.classList.remove('border-hold/60', 'border-go/40');
  sub.className = 'mt-1 text-sm';
  if (sync.stale_with_pending > 0) {
    card.classList.add('border-hold/60');
    sub.classList.add('text-hold', 'font-bold');
    sub.textContent = `⚠ ${sync.stale_with_pending} quiet phone${sync.stale_with_pending === 1 ? ' is' : 's are'} still holding scans`;
  } else if (sync.pending_on_devices > 0) {
    sub.classList.add('text-ink-soft');
    sub.textContent = `${sync.devices_online} phone${sync.devices_online === 1 ? '' : 's'} online · uploading`;
  } else {
    card.classList.add('border-go/40');
    sub.classList.add('text-go', 'font-bold');
    sub.textContent = `✓ All up to date · ${sync.devices_online} phone${sync.devices_online === 1 ? '' : 's'} online`;
  }
}

function renderThroughput(st) {
  $('rate-now').textContent = fmtInt(st.scans_last_minute);
  $('rate-avg').textContent = `${(st.scans_last_15_min / 15).toFixed(1)} per minute on average over 15 minutes`;

  const chart = $('rate-chart');
  const tip = $('rate-tooltip');
  const max = Math.max(1, ...st.per_minute.map((s) => s.count));
  chart.replaceChildren(...st.per_minute.map((slot) => {
    const h = slot.count === 0 ? 0 : Math.max(4, Math.round((slot.count / max) * 100));
    const label = `${fmtTime(slot.minute)}: ${fmtInt(slot.count)} scan${slot.count === 1 ? '' : 's'}`;
    // The full-height column is the hover target; the visible bar sits at its base.
    const col = el('div', { class: 'group flex h-full flex-1 items-end' },
      el('div', { class: 'w-full rounded-t-[4px] bg-bar group-hover:brightness-125', style: `height:${h}%` }));
    col.addEventListener('mouseenter', () => {
      tip.textContent = label;
      tip.hidden = false;
      const box = chart.getBoundingClientRect();
      const c = col.getBoundingClientRect();
      const host = chart.offsetParent.getBoundingClientRect();
      tip.style.left = `${Math.min(c.left - host.left, box.right - host.left - tip.offsetWidth)}px`;
      tip.style.top = `${box.top - host.top - tip.offsetHeight - 6}px`;
    });
    col.addEventListener('mouseleave', () => { tip.hidden = true; });
    return col;
  }));
  $('rate-chart-desc').textContent = 'Scans per minute, last 15 minutes: ' +
    st.per_minute.map((s) => `${fmtTime(s.minute)} ${s.count}`).join(', ');
}

// -----------------------------------------------------------------------------
// Delegate table
// -----------------------------------------------------------------------------

async function loadTable() {
  const seq = ++state.tableSeq;
  $('table-error').hidden = true;
  try {
    const page = await api('/api/v1/admin/participants/filter', { method: 'POST', body: state.filter });
    if (seq !== state.tableSeq) return; // a newer filter is already in flight

    // Filters narrowed under us (e.g. a delegate got claimed): step back to
    // the last page that still has rows.
    if (page.rows.length === 0 && page.total > 0 && state.filter.page > 1) {
      state.filter.page = Math.max(1, Math.ceil(page.total / state.filter.page_size));
      return loadTable();
    }
    state.rows = page.rows;
    state.total = page.total;
    renderTable();
  } catch (err) {
    if (seq !== state.tableSeq || err.status === 401) return;
    $('table-error').textContent = err.message;
    $('table-error').hidden = false;
  }
}

const refreshTableSoon = debounce(loadTable, TABLE_REFRESH_DEBOUNCE_MS);

function renderTable() {
  const scenario = scenarioById(state.filter.scenario_id);
  $('delegate-rows').replaceChildren(...state.rows.map((r) => delegateRow(r, scenario)));
  $('table-empty').hidden = state.rows.length > 0;

  const pages = Math.max(1, Math.ceil(state.total / state.filter.page_size));
  $('result-count').textContent = `${fmtInt(state.total)} delegate${state.total === 1 ? '' : 's'} match`;
  $('page-info').textContent = `Page ${state.filter.page} of ${pages}`;
  $('page-prev').disabled = state.filter.page <= 1;
  $('page-next').disabled = state.filter.page >= pages;
}

function delegateRow(r, scenario) {
  let age;
  if (r.is_minor) age = minorBadge(r.age_years);
  else if (r.age_years != null) age = el('span', { class: 'tabular-nums' }, String(r.age_years));
  else age = el('span', { class: 'text-ink-soft' }, 'Unknown');

  const venue = r.venue_name
    ? [el('div', {}, r.venue_name), el('div', { class: 'text-xs text-ink-soft' }, r.hall_section)]
    : el('span', { class: 'text-ink-soft' }, 'Unassigned');

  let status;
  if (!scenario) {
    status = el('span', { class: 'text-ink-soft' }, '–');
  } else if (r.claimed) {
    const what = scenario.allow_duplicates && r.claim_count > 1 ? `${r.claim_count} scans` : 'Claimed';
    status = [
      el('span', { class: 'pill bg-go/15 text-go' }, `✓ ${what}`),
      el('div', { class: 'mt-0.5 text-xs text-ink-soft' },
        `${fmtTime(r.claimed_at)} by ${r.claimed_by}`, r.claim_method === 'MANUAL_OVERRIDE' ? ' (manual)' : ''),
    ];
  } else {
    status = el('span', { class: 'pill border border-line text-ink-soft' }, '○ Not scanned');
  }

  const btn = el('button', {
    type: 'button',
    class: 'btn-ghost min-h-[34px] px-3 text-xs',
    disabled: !r.is_confirmed,
    title: r.is_confirmed ? 'Record a scan without a QR code' : 'Only confirmed delegates can be checked in',
    onclick: () => openOverride(r),
  }, 'Manual override');

  return el('tr', { class: r.is_minor ? 'bg-hold/5' : '' },
    el('td', { class: 'td' },
      el('div', { class: 'font-bold' }, r.full_name,
        r.is_confirmed ? null : el('span', { class: 'pill ml-2 bg-stop/10 text-stop' }, 'Unconfirmed')),
      el('div', { class: 'text-xs text-ink-soft' }, r.email),
      customChips(r)),
    el('td', { class: 'td whitespace-nowrap tabular-nums' }, r.membership_id),
    el('td', { class: 'td whitespace-nowrap' }, age),
    el('td', { class: 'td' }, r.team_name || el('span', { class: 'text-ink-soft' }, 'No team')),
    el('td', { class: 'td' }, venue),
    el('td', { class: 'td' }, status),
    el('td', { class: 'td text-right' }, btn));
}

/** Values of the filter fields, so a filtered table shows why each row matched. */
function customChips(r) {
  const attrs = r.custom_attributes || {};
  const chips = state.filterFields
    .filter((f) => attrs[f.field_key] != null && attrs[f.field_key] !== '')
    .map((f) => el('span', { class: 'pill border border-line bg-paper/60 font-normal text-ink-soft' },
      `${f.field_label}: `, el('b', { class: 'text-ink' }, fmtCustom(attrs[f.field_key], f.data_type))));
  return chips.length ? el('div', { class: 'mt-1 flex flex-wrap gap-1' }, chips) : null;
}

// ---- Filter bar ----------------------------------------------------------------

/** Claim status only means something for one scenario, so it follows that select. */
function syncClaimSelect() {
  const claim = $('f-claim');
  claim.disabled = !state.filter.scenario_id;
  if (claim.disabled) {
    claim.value = '';
    state.filter.claim_status = '';
  }
}

function applyFilter(patch) {
  Object.assign(state.filter, patch, { page: 1 });
  loadTable();
}

function bindFilters() {
  const onSearch = debounce(() => applyFilter({ search: $('f-search').value.trim() }), SEARCH_DEBOUNCE_MS);
  $('f-search').addEventListener('input', onSearch);
  $('filters').addEventListener('submit', (e) => e.preventDefault());
  $('f-age').addEventListener('change', (e) => applyFilter({ age_group: e.target.value }));
  $('f-venue').addEventListener('change', (e) => applyFilter({ venue_id: e.target.value }));
  $('f-confirm').addEventListener('change', (e) => applyFilter({ confirmation: e.target.value }));
  $('f-claim').addEventListener('change', (e) => applyFilter({ claim_status: e.target.value }));
  $('f-scenario').addEventListener('change', (e) => {
    state.filter.scenario_id = e.target.value;
    syncClaimSelect();
    updateScenarioColumn();
    applyFilter({});
  });
  $('f-reset').addEventListener('click', () => {
    $('f-search').value = '';
    $('f-age').value = '';
    $('f-venue').value = '';
    $('f-confirm').value = 'CONFIRMED';
    $('f-claim').value = '';
    state.customFilter = {};
    renderCustomFilters();
    applyFilter({ search: '', age_group: '', venue_id: '', confirmation: 'CONFIRMED', claim_status: '', custom: [] });
  });
  $('page-prev').addEventListener('click', () => { state.filter.page--; loadTable(); });
  $('page-next').addEventListener('click', () => { state.filter.page++; loadTable(); });
}

// -----------------------------------------------------------------------------
// Manual override modal
// -----------------------------------------------------------------------------

function openOverride(row) {
  state.overrideRow = row;
  $('ov-name').textContent = row.full_name;
  $('ov-minor').replaceChildren(row.is_minor ? minorBadge(row.age_years) : '');
  $('ov-id').textContent = row.membership_id;
  $('ov-team').textContent = row.team_name || 'No team';
  $('ov-admin').textContent = getSession()?.full_name || '';

  const sel = $('ov-scenario');
  const active = state.scenarios.filter((s) => s.is_active);
  sel.replaceChildren(...active.map((s) => new Option(s.allow_duplicates ? `${s.name} (repeatable)` : s.name, s.id)));
  if (active.some((s) => s.id === state.filter.scenario_id)) sel.value = state.filter.scenario_id;
  updateOverrideNote();

  $('ov-rationale').value = '';
  $('ov-count').textContent = '0';
  $('ov-error').hidden = true;
  $('ov-submit').disabled = active.length === 0;
  if (active.length === 0) showOverrideError('No checkpoint is active. Switch one on under Checkpoints first.');

  $('override-dialog').showModal();
  $('ov-rationale').focus();
}

function updateOverrideNote() {
  const s = scenarioById($('ov-scenario').value);
  const row = state.overrideRow;
  const note = $('ov-scenario-note');
  note.className = 'mt-1 text-xs text-ink-soft';
  if (!s) { note.textContent = ''; return; }
  if (!s.allow_duplicates && row && row.claimed && s.id === state.filter.scenario_id) {
    note.className = 'mt-1 text-xs font-bold text-hold';
    note.textContent = `⚠ Already claimed ${fmtTime(row.claimed_at)} by ${row.claimed_by}. The server will refuse a second claim.`;
  } else {
    note.textContent = s.allow_duplicates ? 'Repeat scans allowed.' : 'One claim per delegate. Refused if already claimed.';
  }
}

function showOverrideError(msg) {
  $('ov-error').textContent = msg;
  $('ov-error').hidden = false;
}

async function submitOverride(e) {
  e.preventDefault();
  const row = state.overrideRow;
  const rationale = $('ov-rationale').value.replace(/\s+/g, ' ').trim();
  const scenarioId = $('ov-scenario').value;
  $('ov-error').hidden = true;

  if (!scenarioId) return showOverrideError('Choose a scenario.');
  if (rationale.length < MIN_RATIONALE) {
    $('ov-rationale').focus();
    return showOverrideError(`Write a reason of at least ${MIN_RATIONALE} characters. It goes into the audit log.`);
  }

  const btn = $('ov-submit');
  btn.disabled = true;
  try {
    const res = await api('/api/v1/admin/scans/manual-override', {
      method: 'POST',
      body: { participant_id: row.id, scenario_id: scenarioId, rationale },
    });
    $('override-dialog').close();
    toast(`Recorded ${res.scenario_name} for ${res.participant_name}.`);
    loadTable();
    loadStats();
  } catch (err) {
    if (err.status === 409 && err.body.claimed_by) {
      const how = err.body.claim_method === 'MANUAL_OVERRIDE' ? 'a manual override' : 'a QR scan';
      showOverrideError(`Already claimed ${fmtTime(err.body.claimed_at)} by ${err.body.claimed_by} (${how}). Nothing was recorded.`);
      loadTable();
    } else if (err.status !== 401) {
      showOverrideError(err.message);
    }
  } finally {
    btn.disabled = false;
  }
}

function bindOverride() {
  const dlg = $('override-dialog');
  $('override-form').addEventListener('submit', submitOverride);
  dlg.querySelectorAll('[data-close]').forEach((b) => b.addEventListener('click', () => dlg.close()));
  $('ov-scenario').addEventListener('change', updateOverrideNote);
  $('ov-rationale').addEventListener('input', (e) => { $('ov-count').textContent = String(e.target.value.length); });
  $('ov-presets').addEventListener('click', (e) => {
    const b = e.target.closest('button');
    if (!b) return;
    $('ov-rationale').value = b.textContent;
    $('ov-count').textContent = String(b.textContent.length);
    $('ov-rationale').focus();
  });
}

// -----------------------------------------------------------------------------
// Live scan stream (server-sent events over fetch)
// -----------------------------------------------------------------------------
// EventSource can't send an Authorization header, and a token in the URL would
// end up in access logs, so the stream is read with fetch() and parsed here.

const feed = {
  running: false,
  abort: null,
  backoff: 1000,
  items: [],           // newest first
  ids: new Set(),
  paused: false,
  unseen: 0,
};

const FEED_STATES = {
  connecting:   { text: 'Connecting…',   cls: 'pill bg-line/50 text-ink-soft' },
  live:         { text: 'Live',          cls: 'pill bg-go/15 text-go' },
  reconnecting: { text: 'Reconnecting…', cls: 'pill bg-hold/15 text-hold' },
  paused:       { text: 'Paused (tab hidden)', cls: 'pill bg-line/50 text-ink-soft' },
};

function setFeedStatus(name) {
  const s = FEED_STATES[name];
  $('feed-status').className = s.cls;
  $('feed-status').dataset.state = name;
  $('feed-status-text').textContent = s.text;
}

function addScans(events, { live }) {
  let touched = false;
  for (const e of events) {
    if (feed.ids.has(e.id)) continue;
    feed.ids.add(e.id);
    feed.items.push({ ...e, receivedAt: live ? Date.now() : 0 });
    if (e.scenario_id === state.filter.scenario_id) touched = true;
    if (live && feed.paused) feed.unseen++;
  }
  feed.items.sort((a, b) => (a.synced_at < b.synced_at ? 1 : a.synced_at > b.synced_at ? -1 : 0));
  for (const dropped of feed.items.splice(STREAM_MAX_ITEMS)) feed.ids.delete(dropped.id);

  if (!feed.paused) renderStream();
  else $('stream-pause').parentElement.lastChild.textContent = ` Paused · ${feed.unseen} new`;
  if (live && touched) refreshTableSoon();
}

function renderStream() {
  $('stream-empty').hidden = feed.items.length > 0;
  $('stream').replaceChildren(...feed.items.map(streamItem));
}

function streamItem(e) {
  const ok = e.status === 'VALID';
  const fresh = e.receivedAt && Date.now() - e.receivedAt < 5000;
  return el('li', { class: `px-4 py-2.5 text-sm ${fresh ? 'bg-action/10' : ''}` },
    el('div', { class: 'flex items-center justify-between gap-2' },
      el('span', { class: ok ? 'pill bg-go/15 text-go' : 'pill bg-hold/15 text-hold' },
        ok ? '✓ Approved' : '! Duplicate refused'),
      el('time', { class: 'text-xs tabular-nums text-ink-soft', datetime: e.scanned_at, title: `Uploaded ${fmtTime(e.synced_at, true)}` },
        fmtTime(e.scanned_at, true))),
    el('div', { class: 'mt-1 flex flex-wrap items-center gap-1.5' },
      el('b', {}, e.participant_name),
      el('span', { class: 'text-xs text-ink-soft tabular-nums' }, e.membership_id),
      e.is_minor ? minorBadge(null) : null),
    el('div', { class: 'text-xs text-ink-soft' },
      `${e.scenario_name} · by ${e.admin_name}`,
      e.entry_method === 'MANUAL_OVERRIDE' ? el('span', { class: 'pill ml-1 border border-action/40 text-action' }, 'Manual') : null),
    e.override_reason ? el('div', { class: 'mt-0.5 text-xs italic text-ink-soft' }, `“${e.override_reason}”`) : null);
}

async function loadBacklog() {
  const list = await api('/api/v1/admin/scans/recent?limit=50');
  addScans(list, { live: false });
}

async function runStream() {
  while (feed.running) {
    feed.abort = new AbortController();
    const { signal } = feed.abort;
    try {
      setFeedStatus(feed.backoff > 1000 ? 'reconnecting' : 'connecting');
      await loadBacklog(); // covers anything missed while disconnected

      const session = getSession();
      if (!session) return onAuthExpired();
      const res = await fetch('/api/v1/admin/scans/stream', {
        headers: { Authorization: `Bearer ${session.token}`, Accept: 'text/event-stream' },
        cache: 'no-store',
        signal,
      });
      if (res.status === 401) return onAuthExpired();
      if (!res.ok || !res.body) throw new Error(`Stream HTTP ${res.status}`);

      setFeedStatus('live');
      feed.backoff = 1000;
      await readSSE(res.body, (event, data) => {
        if (event === 'scan') addScans([JSON.parse(data)], { live: true });
        // 'reset': the server dropped us (too slow, or shutting down). The
        // loop reconnects and reloads the backlog.
      });
    } catch (err) {
      if (!feed.running) return;
      if (err.status === 401) return;
      console.warn('Scan stream interrupted:', err);
    }
    if (!feed.running) return;
    setFeedStatus('reconnecting');
    await sleep(feed.backoff);
    feed.backoff = Math.min(feed.backoff * 2, STREAM_BACKOFF_MAX_MS);
  }
}

/** Minimal text/event-stream parser: calls onEvent(eventName, data) per message. */
async function readSSE(body, onEvent) {
  const reader = body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buf += value.replace(/\r\n/g, '\n');
    let end;
    while ((end = buf.indexOf('\n\n')) >= 0) {
      const block = buf.slice(0, end);
      buf = buf.slice(end + 2);
      let event = 'message';
      const data = [];
      for (const line of block.split('\n')) {
        if (line.startsWith(':')) continue; // keep-alive comment
        const i = line.indexOf(':');
        const field = i < 0 ? line : line.slice(0, i);
        const val = i < 0 ? '' : line.slice(i + 1).replace(/^ /, '');
        if (field === 'event') event = val;
        else if (field === 'data') data.push(val);
      }
      if (data.length) onEvent(event, data.join('\n'));
    }
  }
}

function startStream() {
  if (feed.running) return;
  feed.running = true;
  feed.backoff = 1000;
  runStream();
}

function stopStream() {
  feed.running = false;
  feed.abort?.abort();
}

function bindStream() {
  $('stream-pause').addEventListener('change', (e) => {
    feed.paused = e.target.checked;
    if (!feed.paused) {
      feed.unseen = 0;
      e.target.parentElement.lastChild.textContent = ' Pause';
      renderStream();
    }
  });
  // Re-render periodically so the "just arrived" highlight fades out.
  setInterval(() => { if (!feed.paused && feed.items.some((i) => i.receivedAt)) renderStream(); }, 5000);
}

// -----------------------------------------------------------------------------
// Scanner devices
// -----------------------------------------------------------------------------

const DEVICE_STATES = {
  ONLINE:  { label: '✓ Reporting',     cls: 'pill bg-go/15 text-go' },
  STALE:   { label: '⚠ Not reporting', cls: 'pill bg-hold/15 text-hold' },
  OFFLINE: { label: '○ Inactive',      cls: 'pill border border-line text-ink-soft' },
};

let devicesBusy = false;

async function loadDevices() {
  if (devicesBusy) return;
  devicesBusy = true;
  try {
    const res = await api('/api/v1/admin/devices/health');
    state.serverSkewMs = new Date(res.server_time).getTime() - Date.now();
    renderDevices(res.devices);
  } catch (err) {
    if (err.status !== 401) console.warn('Device refresh failed:', err);
  } finally {
    devicesBusy = false;
  }
}

function renderDevices(devices) {
  const confirmed = state.stats?.total_confirmed ?? null;
  $('devices-empty').hidden = devices.length > 0;
  $('device-rows').replaceChildren(...devices.map((d) => {
    const st = DEVICE_STATES[d.state] || DEVICE_STATES.OFFLINE;
    const waiting = d.pending_count + d.pending_others;
    const behind = confirmed != null && d.manifest_count < confirmed;
    return el('tr', { class: d.state === 'STALE' && waiting > 0 ? 'bg-hold/5' : '' },
      el('td', { class: 'td' }, el('span', { class: st.cls }, st.label)),
      el('td', { class: 'td' },
        el('div', { class: 'font-bold' }, d.admin_name),
        el('div', { class: 'text-xs text-ink-soft' }, d.admin_email)),
      el('td', { class: 'td' }, d.scenario_name || el('span', { class: 'text-ink-soft' }, 'None chosen')),
      el('td', { class: 'td text-right tabular-nums' },
        el('b', { class: waiting > 0 ? 'text-hold' : '' }, fmtInt(waiting)),
        d.pending_others > 0
          ? el('div', { class: 'text-xs text-ink-soft' }, `${fmtInt(d.pending_others)} from an earlier volunteer`)
          : null),
      el('td', { class: 'td text-right tabular-nums' }, fmtInt(d.rejected_count)),
      el('td', { class: 'td text-right tabular-nums' },
        el('span', { class: behind ? 'font-bold text-hold' : '', title: behind ? 'Older than the current delegate list; it updates when the phone refreshes online' : null },
          behind ? `⚠ ${fmtInt(d.manifest_count)}` : fmtInt(d.manifest_count))),
      el('td', { class: 'td whitespace-nowrap' }, d.last_sync_at ? fmtTime(d.last_sync_at) : 'Never'),
      el('td', { class: 'td whitespace-nowrap', title: new Date(d.last_seen_at).toLocaleString() }, fmtAgo(d.last_seen_at)));
  }));
}

// -----------------------------------------------------------------------------
// Scenario control drawer
// -----------------------------------------------------------------------------

function openDrawer() {
  renderDrawer();
  $('drawer-error').hidden = true;
  $('scenario-drawer').showModal();
  loadScenarios().catch(() => {});
}

function drawerError(msg) {
  $('drawer-error').textContent = msg;
  $('drawer-error').hidden = !msg;
}

function renderDrawer() {
  const stats = new Map((state.stats?.scenarios || []).map((s) => [s.id, s]));
  $('scenario-list').replaceChildren(...state.scenarios.map((s) => scenarioCard(s, stats.get(s.id))));
}

function scenarioCard(s, st) {
  const sw = el('button', {
    type: 'button',
    role: 'switch',
    'aria-checked': String(s.is_active),
    'aria-label': `${s.name} active`,
    class: `relative inline-flex h-7 w-12 shrink-0 items-center rounded-full transition ${s.is_active ? 'bg-go' : 'bg-line'}`,
    onclick: (e) => toggleScenario(s, e.currentTarget),
  }, el('span', { class: `inline-block h-5 w-5 rounded-full bg-panel shadow transition ${s.is_active ? 'translate-x-6' : 'translate-x-1'}` }));

  const li = el('li', { class: `card p-4 ${s.is_active ? '' : 'opacity-75'}` },
    el('div', { class: 'flex items-start justify-between gap-3' },
      el('div', { class: 'min-w-0' },
        el('div', { class: 'flex flex-wrap items-center gap-2' },
          el('b', {}, s.name),
          el('span', { class: 'pill border border-line text-ink-soft' }, s.code)),
        el('p', { class: 'text-sm text-ink-soft' },
          s.is_active ? 'Active on scanners' : 'Off: hidden from scanners',
          ' · ', s.allow_duplicates ? 'Repeat scans allowed' : 'One claim per delegate'),
        s.description ? el('p', { class: 'mt-1 text-sm' }, s.description) : null),
      sw),
    st ? el('p', { class: 'mt-2 text-xs text-ink-soft tabular-nums' },
      `${fmtInt(st.unique_delegates)} delegates · ${fmtInt(st.valid_scans)} scans · ` +
      `${fmtInt(st.rejected_attempts)} duplicates refused · ${fmtInt(st.manual_overrides)} manual`) : null);

  const editBtn = el('button', { type: 'button', class: 'btn-ghost mt-3 min-h-[32px] px-3 text-xs' }, 'Edit');
  editBtn.addEventListener('click', () => { editBtn.replaceWith(scenarioEditForm(s)); });
  li.append(editBtn);
  return li;
}

function scenarioEditForm(s) {
  const name = el('input', { class: 'field', value: s.name, maxlength: '100', 'aria-label': 'Name' });
  const code = el('input', { class: 'field uppercase', value: s.code, maxlength: '50', 'aria-label': 'Code' });
  const desc = el('input', { class: 'field', value: s.description || '', maxlength: '300', 'aria-label': 'Description' });
  const dup = el('input', { type: 'checkbox', class: 'h-4 w-4', checked: s.allow_duplicates });
  const form = el('form', { class: 'mt-3 space-y-2 border-t border-line pt-3' },
    el('label', { class: 'label' }, 'Name'), name,
    el('label', { class: 'label' }, 'Code'), code,
    el('p', { class: 'text-xs text-hold' }, 'Phones match passes by scenario ID, so renaming is safe; changing the code only affects reports.'),
    el('label', { class: 'label' }, 'Description'), desc,
    el('label', { class: 'flex items-center gap-2 text-sm' }, dup, 'Allow repeat scans'),
    el('p', { class: 'text-xs text-ink-soft' }, 'Changing repeat scans applies to scans recorded from now on. Existing claims keep their rule.'),
    el('div', { class: 'flex gap-2' },
      el('button', { type: 'submit', class: 'btn-primary min-h-[34px] px-3 text-xs' }, 'Save'),
      el('button', { type: 'button', class: 'btn-ghost min-h-[34px] px-3 text-xs', onclick: renderDrawer }, 'Cancel')));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    drawerError('');
    try {
      await api(`/api/v1/scenarios/${encodeURIComponent(s.id)}`, {
        method: 'PATCH',
        body: { name: name.value, code: code.value, description: desc.value, allow_duplicates: dup.checked },
      });
      toast(`Saved ${name.value.trim()}.`);
      await Promise.all([loadScenarios(), loadStats()]);
    } catch (err) {
      if (err.status !== 401) drawerError(err.message);
    }
  });
  return form;
}

async function toggleScenario(s, btn) {
  const next = !s.is_active;
  if (!next && !confirm(`Switch off “${s.name}”? Scanners stop offering it on their next refresh. Scans already taken still upload.`)) return;
  btn.disabled = true;
  drawerError('');
  try {
    await api('/api/v1/admin/scenarios/toggle', { method: 'POST', body: { scenario_id: s.id, is_active: next } });
    toast(`${s.name} is now ${next ? 'active' : 'off'}.`, next ? 'ok' : 'warn');
    await Promise.all([loadScenarios(), loadStats()]);
    loadTable();
  } catch (err) {
    if (err.status !== 401) drawerError(err.message);
    btn.disabled = false;
  }
}

function bindDrawer() {
  const dlg = $('scenario-drawer');
  $('open-scenarios').addEventListener('click', openDrawer);
  dlg.querySelectorAll('[data-close]').forEach((b) => b.addEventListener('click', () => dlg.close()));
  // Click on the backdrop (outside the panel) closes the drawer.
  dlg.addEventListener('click', (e) => { if (e.target === dlg) dlg.close(); });

  $('scenario-create').addEventListener('submit', async (e) => {
    e.preventDefault();
    drawerError('');
    const body = {
      name: $('ns-name').value,
      code: $('ns-code').value.trim().toUpperCase(),
      description: $('ns-desc').value,
      allow_duplicates: $('ns-dup').checked,
      is_active: $('ns-active').checked,
    };
    try {
      const s = await api('/api/v1/scenarios', { method: 'POST', body });
      toast(`Created ${s.name}.`);
      e.target.reset();
      $('ns-active').checked = true;
      $('new-scenario').open = false;
      await Promise.all([loadScenarios(), loadStats()]);
    } catch (err) {
      if (err.status !== 401) drawerError(err.status === 409 ? `Code ${body.code} is already used by another checkpoint.` : err.message);
    }
  });
}

// -----------------------------------------------------------------------------
// Polling lifecycle
// -----------------------------------------------------------------------------
// Everything pauses while the tab is hidden: an organizer with five forgotten
// dashboard tabs shouldn't cost five live streams and pollers.

function startLive() {
  stopLive();
  loadStats();
  loadDevices();
  startStream();
  state.timers.push(
    setInterval(loadStats, STATS_INTERVAL_MS),
    setInterval(loadDevices, DEVICES_INTERVAL_MS),
    setInterval(() => loadScenarios().catch(() => {}), SCENARIOS_INTERVAL_MS),
  );
}

function stopLive() {
  state.timers.forEach(clearInterval);
  state.timers = [];
  stopStream();
}

// -----------------------------------------------------------------------------
// Sign-in / sign-out
// -----------------------------------------------------------------------------

function showLogin(message) {
  stopLive();
  pendingChange = null;
  document.querySelectorAll('dialog[open]').forEach((d) => d.close());
  $('app-view').hidden = true;
  $('login-view').hidden = false;
  $('pwchange-form').hidden = true;
  $('login-form').hidden = false;
  $('login-error').textContent = message || '';
  $('login-error').hidden = !message;
  $('login-email').focus();
}

let authExpiredShown = false;
function onAuthExpired(message = 'Your session ended. Sign in again.') {
  if (authExpiredShown) return;
  authExpiredShown = true;
  clearSession();
  showLogin(message);
}

async function showApp() {
  authExpiredShown = false;
  const session = getSession();
  $('login-view').hidden = true;
  $('app-view').hidden = false;
  $('signed-in-as').textContent = session.full_name;

  try {
    await Promise.all([loadScenarios(), loadVenues(), loadFilterFields()]);
    await loadStats(); // picks the default scenario before the first table load
  } catch (err) {
    if (err.status === 403) {
      clearSession();
      return showLogin('This account can’t use the control panel. Ask an event lead for an admin login.');
    }
    if (err.status !== 401) toast(`Could not load event data: ${err.message}`, 'error');
  }
  loadTable();
  startLive();
}

// Temporary-password sign-in waiting for a new password. Kept in memory only:
// the token works for nothing but /auth/change-password.
let pendingChange = null;

function assertManager(body) {
  // The server enforces roles on every admin endpoint; this just gives a
  // scanner account a clear message instead of a dashboard full of errors.
  if (!MANAGER_ROLES.includes(body.role)) {
    throw new Error('This is a scanner account. Use the scanner app at /scanner.html; the control panel needs an admin login.');
  }
}

async function onLogin(e) {
  e.preventDefault();
  const btn = $('login-submit');
  btn.disabled = true;
  $('login-error').hidden = true;
  try {
    const password = $('login-password').value;
    const body = await signIn($('login-email').value, password);
    assertManager(body);
    $('login-password').value = '';
    if (body.force_password_change) {
      // First sign-in with emailed credentials: choose a password first.
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

async function onPasswordChange(e) {
  e.preventDefault();
  const err = $('pw-error');
  err.hidden = true;
  const next = $('pw-new').value;
  const problem = passwordProblem(next, $('pw-confirm').value);
  if (problem) {
    err.textContent = problem;
    err.hidden = false;
    return;
  }
  const btn = $('pw-submit');
  btn.disabled = true;
  try {
    const body = await changePassword(pendingChange.token, pendingChange.current, next);
    pendingChange = null;
    $('pw-new').value = '';
    $('pw-confirm').value = '';
    saveSession(body);
    toast('Password set. Welcome!');
    await showApp();
  } catch (ex) {
    if (ex.status === 401) return showLogin('That sign-in expired. Sign in again with the temporary password.');
    err.textContent = ex.message.replace(/^validation failed: /, '');
    err.hidden = false;
  } finally {
    btn.disabled = false;
  }
}

function onLogout() {
  clearSession();
  feed.items = [];
  feed.ids.clear();
  renderStream();
  showLogin();
}

// -----------------------------------------------------------------------------
// Boot
// -----------------------------------------------------------------------------

function boot() {
  $('login-form').addEventListener('submit', onLogin);
  $('pwchange-form').addEventListener('submit', onPasswordChange);
  $('pw-cancel').addEventListener('click', () => showLogin());
  window.addEventListener(AUTH_EVENT, (e) => onAuthExpired(e.detail?.message));
  $('logout').addEventListener('click', onLogout);
  bindFilters();
  bindOverride();
  bindDrawer();
  bindStream();

  document.addEventListener('visibilitychange', () => {
    if (!getSession() || $('app-view').hidden) return;
    if (document.visibilityState === 'hidden') {
      stopLive();
      setFeedStatus('paused');
    } else {
      startLive();
      loadTable();
    }
  });

  const handover = sessionStorage.getItem('nsac_signin_message'); // from staff.html
  sessionStorage.removeItem('nsac_signin_message');
  if (getSession()) showApp();
  else showLogin(handover || undefined);
}

boot();
