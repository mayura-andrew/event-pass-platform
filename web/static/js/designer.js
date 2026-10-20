// Pass designer (SaaS, milestone M5). The design is JSON; the server renders
// it (the same code that renders the passes people get) and this script draws
// selection boxes, handles and guides over that image.
const root = document.getElementById('designer');
const api = root.dataset.api;
const csrf = root.dataset.csrf;
const editable = root.dataset.editable === 'true';
const $ = (id) => document.getElementById(id);
let design = JSON.parse(root.dataset.design);
let selected = null;
const undoStack = [];
const redoStack = [];
let dirty = false;

const stage = $('stage');
const img = $('preview');
const overlay = $('overlay');

// ---- History ------------------------------------------------------------------
function snapshot() { return JSON.stringify(design); }
function change(fn) {
  if (!editable) return;
  const before = snapshot();
  fn();
  // A form control can report the same edit twice (change, then blur as the
  // panel redraws); only real changes go in the history.
  if (snapshot() === before) return;
  undoStack.push(before);
  if (undoStack.length > 100) undoStack.shift();
  redoStack.length = 0;
  dirty = true;
  refresh();
}
function undo() { if (undoStack.length) { redoStack.push(snapshot()); design = JSON.parse(undoStack.pop()); dirty = true; refresh(); } }
function redo() { if (redoStack.length) { undoStack.push(snapshot()); design = JSON.parse(redoStack.pop()); dirty = true; refresh(); } }
$('undo').addEventListener('click', undo);
$('redo').addEventListener('click', redo);

// ---- Geometry -------------------------------------------------------------------
const el = (id) => design.elements.find((e) => e.id === id);
const scale = () => img.clientWidth / design.size.width;
function box(e) {
  if (e.type === 'qr') return { x: e.x, y: e.y, w: e.size, h: e.size };
  return { x: e.x, y: e.y, w: e.w, h: Math.max(e.h || 0, 4) };
}
const SNAP = 8;
function snap(v, size, total) {
  // Snap to the centre line, then to the grid.
  const centre = (total - size) / 2;
  if (Math.abs(v - centre) < 12) return Math.round(centre);
  return Math.round(v / SNAP) * SNAP;
}
function clamp(e) {
  const b = box(e);
  e.x = Math.max(0, Math.min(e.x, design.size.width - b.w));
  e.y = Math.max(0, Math.min(e.y, design.size.height - b.h));
}

// ---- Overlay ---------------------------------------------------------------------
let problems = [];
function drawOverlay() {
  overlay.replaceChildren();
  const s = scale();
  if (!s) return;
  for (const e of design.elements) {
    const b = box(e);
    const d = document.createElement('div');
    d.className = 'sel' + (e.id === selected ? ' on' : '') + (problems.some((p) => p.element === e.id) ? ' bad' : '');
    d.style.left = `${b.x * s}px`;
    d.style.top = `${b.y * s}px`;
    d.style.width = `${b.w * s}px`;
    d.style.height = `${b.h * s}px`;
    d.dataset.id = e.id;
    d.title = `${e.id} (${e.type})`;
    if (e.id === selected && editable) {
      const h = document.createElement('span');
      h.className = 'handle';
      h.dataset.handle = '1';
      d.append(h);
    }
    overlay.append(d);
  }
  const g = document.createElement('div'); // centre guide
  g.className = 'guide';
  g.style.left = `${(design.size.width / 2) * s}px`;
  overlay.append(g);
}

// Drag to move; the corner handle resizes.
let drag = null;
overlay.addEventListener('pointerdown', (ev) => {
  const target = ev.target.closest('.sel');
  if (!target) { selected = null; refreshLocal(); return; }
  selected = target.dataset.id;
  refreshLocal();
  if (!editable) return;
  const e = el(selected);
  drag = { id: selected, resize: !!ev.target.dataset.handle, sx: ev.clientX, sy: ev.clientY, start: { ...e }, before: snapshot(), moved: false };
  overlay.setPointerCapture(ev.pointerId);
  ev.preventDefault();
});
overlay.addEventListener('pointermove', (ev) => {
  if (!drag) return;
  const s = scale();
  const dx = (ev.clientX - drag.sx) / s;
  const dy = (ev.clientY - drag.sy) / s;
  const e = el(drag.id);
  if (drag.resize) {
    if (e.type === 'qr') e.size = Math.max(100, Math.round((drag.start.size + Math.max(dx, dy)) / SNAP) * SNAP);
    else {
      e.w = Math.max(20, Math.round((drag.start.w + dx) / SNAP) * SNAP);
      e.h = Math.max(e.type === 'line' ? 1 : 20, Math.round((drag.start.h + dy) / SNAP) * SNAP);
    }
  } else {
    const b = box(drag.start);
    e.x = snap(drag.start.x + dx, b.w, design.size.width);
    e.y = Math.round((drag.start.y + dy) / SNAP) * SNAP;
  }
  clamp(e);
  drag.moved = true;
  drawOverlay();
  renderProps();
});
overlay.addEventListener('pointerup', () => {
  if (drag && drag.moved) {
    undoStack.push(drag.before);
    redoStack.length = 0;
    dirty = true;
    refresh();
  }
  drag = null;
});

// Arrow keys nudge; Delete removes; Ctrl+Z / Ctrl+Shift+Z.
document.addEventListener('keydown', (ev) => {
  const typing = ev.target.closest('input, textarea, select');
  if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === 'z' && !typing) {
    ev.preventDefault();
    if (ev.shiftKey) redo(); else undo();
    return;
  }
  if (typing || !selected || !editable) return;
  const step = ev.shiftKey ? 10 : 1;
  const moves = { ArrowLeft: [-step, 0], ArrowRight: [step, 0], ArrowUp: [0, -step], ArrowDown: [0, step] };
  if (moves[ev.key]) {
    ev.preventDefault();
    change(() => { const e = el(selected); e.x += moves[ev.key][0]; e.y += moves[ev.key][1]; clamp(e); });
  } else if ((ev.key === 'Delete' || ev.key === 'Backspace') && el(selected).type !== 'qr') {
    ev.preventDefault();
    change(() => { design.elements = design.elements.filter((e) => e.id !== selected); selected = null; });
  }
});

// ---- Layers -----------------------------------------------------------------------
function renderLayers() {
  const ol = $('layers');
  ol.replaceChildren();
  design.elements.forEach((e, i) => {
    const li = document.createElement('li');
    li.className = e.id === selected ? 'on' : '';
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'layer';
    b.textContent = `${e.id}`;
    const t = document.createElement('span');
    t.className = 'muted';
    t.textContent = ` ${e.type}`;
    b.append(t);
    b.addEventListener('click', () => { selected = e.id; refreshLocal(); });
    li.append(b);
    if (editable) {
      for (const [label, delta] of [['↑', -1], ['↓', 1]]) {
        const m = document.createElement('button');
        m.type = 'button';
        m.className = 'btn btn-quiet mini';
        m.textContent = label;
        m.setAttribute('aria-label', `Move ${e.id} ${delta < 0 ? 'back' : 'forward'}`);
        m.disabled = i + delta < 0 || i + delta >= design.elements.length;
        m.addEventListener('click', () => change(() => {
          const [x] = design.elements.splice(i, 1);
          design.elements.splice(i + delta, 0, x);
        }));
        li.append(m);
      }
    }
    ol.append(li);
  });
}

for (const b of root.querySelectorAll('[data-add]')) {
  b.addEventListener('click', () => change(() => {
    const type = b.dataset.add;
    let n = 1;
    while (el(`${type}${n}`)) n++;
    const id = `${type}${n}`;
    const w = design.size.width;
    const base = { id, type, x: 72, y: 200, w: w - 144, h: 60, font: { size: 40, weight: 400 }, color: 'token:ink', align: 'center' };
    if (type === 'text') Object.assign(base, { text: 'New text' });
    if (type === 'answers') Object.assign(base, { h: 150, color: 'token:muted', font: { size: 36, weight: 400 } });
    if (type === 'rect') Object.assign(base, { color: 'token:line', h: 120 });
    if (type === 'line') Object.assign(base, { color: 'token:line', h: 2 });
    if (type === 'statusBand') Object.assign(base, { x: 0, y: 0, w, h: 160, font: { size: 60, weight: 700 } });
    design.elements.push(base);
    selected = id;
  }));
}

// ---- Properties --------------------------------------------------------------------
const fieldsFor = {
  statusBand: ['x', 'y', 'w', 'h', 'fontSize', 'visibleFor'],
  text: ['text', 'x', 'y', 'w', 'h', 'fontSize', 'weight', 'color', 'align', 'maxLines', 'shrinkMin', 'transform', 'hideIfEmpty', 'visibleFor'],
  answers: ['x', 'y', 'w', 'h', 'fontSize', 'weight', 'color', 'align', 'visibleFor'],
  qr: ['x', 'y', 'size', 'fg', 'bg', 'quietZone', 'ecc'],
  rect: ['x', 'y', 'w', 'h', 'color', 'visibleFor'],
  line: ['x', 'y', 'w', 'h', 'color', 'visibleFor'],
};
const labels = { text: 'Text', x: 'X', y: 'Y', w: 'Width', h: 'Height', size: 'Size', fontSize: 'Font size', weight: 'Weight',
  color: 'Color', align: 'Align', maxLines: 'Max lines', shrinkMin: 'Shrink to fit, down to', transform: 'Capitals',
  hideIfEmpty: 'Hide when empty', visibleFor: 'Show for', fg: 'Dark color', bg: 'Light color', quietZone: 'Quiet zone (modules)', ecc: 'Error correction' };
const colorChoices = ['token:ink', 'token:muted', 'token:primary', 'token:surface', 'token:line', 'token:success', 'token:warning', 'token:danger'];
const statuses = ['registered', 'pending', 'waitlisted', 'cancelled'];

function get(e, k) {
  switch (k) {
    case 'fontSize': return (e.font && e.font.size) || 40;
    case 'weight': return (e.font && e.font.weight) || 400;
    case 'shrinkMin': return (e.shrinkToFit && e.shrinkToFit.min) || '';
    case 'quietZone': return e.quietZone || 4;
    default: return e[k] === undefined ? '' : e[k];
  }
}
function set(e, k, v) {
  const num = (x) => (x === '' ? 0 : Number(x));
  switch (k) {
    case 'fontSize': e.font = { ...(e.font || {}), size: num(v) }; break;
    case 'weight': e.font = { ...(e.font || {}), weight: num(v) }; break;
    case 'shrinkMin': if (v === '') delete e.shrinkToFit; else e.shrinkToFit = { min: num(v) }; break;
    case 'x': case 'y': case 'w': case 'h': case 'size': case 'maxLines': case 'quietZone': e[k] = num(v); break;
    case 'transform': case 'hideIfEmpty': case 'ecc': case 'align':
      if (v === '') delete e[k]; else e[k] = v; break;
    default: e[k] = v;
  }
}

function control(e, k) {
  const id = `p-${k}`;
  const wrap = document.createElement('div');
  wrap.className = 'field';
  const label = document.createElement('label');
  label.htmlFor = id;
  label.textContent = labels[k];
  wrap.append(label);
  let input;
  const options = (list, labelOf = (x) => x) => {
    input = document.createElement('select');
    for (const o of list) {
      const op = document.createElement('option');
      op.value = o;
      op.textContent = labelOf(o);
      input.append(op);
    }
  };
  if (k === 'text') { input = document.createElement('textarea'); input.rows = 2; }
  else if (k === 'weight') options(['400', '700'], (x) => (x === '700' ? 'Bold' : 'Regular'));
  else if (k === 'align') options(['center', 'left', 'right']);
  else if (k === 'transform') options(['', 'upper'], (x) => (x ? 'ALL CAPITALS' : 'As typed'));
  else if (k === 'ecc') options(['M', 'L', 'Q', 'H'], (x) => ({ L: 'Low (7%)', M: 'Medium (15%)', Q: 'Quartile (25%)', H: 'High (30%)' }[x]));
  else if (k === 'color') {
    input = document.createElement('input');
    input.setAttribute('list', 'colors');
  } else if (k === 'hideIfEmpty') {
    options(['', ...JSON.parse(root.dataset.variables)], (x) => x || 'Never hide');
  } else if (k === 'visibleFor') {
    const fs = document.createElement('fieldset');
    fs.className = 'group';
    const lg = document.createElement('legend');
    lg.textContent = 'Show for (none ticked: everyone)';
    fs.append(lg);
    for (const st of statuses) {
      const l = document.createElement('label');
      l.className = 'check';
      const c = document.createElement('input');
      c.type = 'checkbox';
      c.checked = (e.visibleFor || []).includes(st);
      c.disabled = !editable;
      c.addEventListener('change', () => change(() => {
        const cur = new Set(e.visibleFor || []);
        if (c.checked) cur.add(st); else cur.delete(st);
        e.visibleFor = [...cur];
        if (!e.visibleFor.length) delete e.visibleFor;
      }));
      l.append(c, ` ${st}`);
      fs.append(l);
    }
    return fs;
  } else {
    input = document.createElement('input');
    input.type = ['text', 'fg', 'bg'].includes(k) ? 'text' : 'number';
  }
  input.id = id;
  input.value = get(e, k);
  input.disabled = !editable;
  input.addEventListener('change', () => change(() => { set(el(selected), k, input.value); clamp(el(selected)); }));
  wrap.append(input);
  return wrap;
}

function renderProps() {
  const form = $('props');
  form.replaceChildren();
  const e = selected && el(selected);
  if (!e) {
    const p = document.createElement('p');
    p.className = 'muted';
    p.textContent = 'Select an element on the pass or in the list.';
    form.append(p);
    return;
  }
  const h = document.createElement('p');
  h.innerHTML = '<b></b> <span class="muted"></span>';
  h.querySelector('b').textContent = e.id;
  h.querySelector('span').textContent = e.type;
  form.append(h);
  for (const k of fieldsFor[e.type] || []) form.append(control(e, k));
  if (!document.getElementById('colors')) {
    const dl = document.createElement('datalist');
    dl.id = 'colors';
    for (const c of colorChoices) { const o = document.createElement('option'); o.value = c; dl.append(o); }
    document.body.append(dl);
  }
}

// ---- Preview and checks (server) -------------------------------------------------
let timer = null;
let lastURL = null;
let generation = 0;
async function renderPreview() {
  const sample = $('sample').value;
  const gen = ++generation; // answers to older requests are ignored
  const sent = JSON.stringify({ design });
  // The checks answer quickly; the image takes a little longer.
  fetch(api + '/check', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: sent })
    .then((r) => (r.ok ? r.json() : { problems: [] }))
    .then((data) => { if (gen === generation) { problems = data.problems; showProblems(); drawOverlay(); } })
    .catch(() => { if (gen === generation) { problems = [{ message: 'Checks unavailable: check your connection.' }]; showProblems(); } });
  try {
    const pr = await fetch(api + `/preview?sample=${sample}`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: sent });
    if (pr.ok && gen === generation) {
      const url = URL.createObjectURL(await pr.blob());
      img.src = url;
      if (lastURL) URL.revokeObjectURL(lastURL);
      lastURL = url;
    }
  } catch { /* the checks report connection problems */ }
}
function showProblems() {
  const ul = $('problems');
  ul.replaceChildren();
  for (const p of problems) {
    const li = document.createElement('li');
    li.textContent = p.message;
    ul.append(li);
  }
  const save = $('save');
  if (save) save.disabled = problems.length > 0 || !dirty;
}
function refreshLocal() { drawOverlay(); renderLayers(); renderProps(); }
function refresh() {
  refreshLocal();
  if (dirty) $('status').textContent = 'Unsaved changes.';
  clearTimeout(timer);
  timer = setTimeout(renderPreview, 250); // debounced (spec: 250 ms)
}
$('sample').addEventListener('change', renderPreview);
img.addEventListener('load', drawOverlay);
window.addEventListener('resize', drawOverlay);
stage.style.aspectRatio = `${design.size.width} / ${design.size.height}`;

// ---- Save ------------------------------------------------------------------------------
const saveBtn = $('save');
if (saveBtn) {
  saveBtn.addEventListener('click', async () => {
    saveBtn.disabled = true;
    const res = await fetch(api, { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({ name: $('tpl-name').value, design }) });
    const data = await res.json().catch(() => ({}));
    if (res.ok) {
      dirty = false;
      $('status').textContent = `Saved as version ${data.version}. Events move to it from their Pass tab.`;
      saveBtn.textContent = `Save version ${data.version + 1}`;
      const li = document.createElement('li');
      li.innerHTML = '<b></b> <span class="muted">just now</span>';
      li.querySelector('b').textContent = `Version ${data.version}`;
      document.querySelector('#versions-title + .recent').prepend(li);
    } else {
      problems = data.problems || [{ message: data.error || `Not saved (HTTP ${res.status})` }];
      showProblems();
      $('status').textContent = 'Not saved.';
      saveBtn.disabled = false;
    }
  });
  $('tpl-name').addEventListener('input', () => { dirty = true; showProblems(); });
}
window.addEventListener('beforeunload', (ev) => { if (dirty) { ev.preventDefault(); ev.returnValue = ''; } });

refreshLocal();
renderPreview();
window.__design = () => JSON.parse(JSON.stringify(design)); // test hook: the design as edited
