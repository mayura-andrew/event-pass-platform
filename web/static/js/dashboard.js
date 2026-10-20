// Live event dashboard (M4): numbers come from /stats; the /stream events say
// when to fetch them again.
const root = document.getElementById('dashboard');
const api = root.dataset.api;
let registered = Number(root.querySelector('.tiles').dataset.registered) || 0;

function bars() {
  for (const el of root.querySelectorAll('.bar-fill')) {
    const n = Number(el.dataset.n) || 0;
    el.style.width = `${registered ? Math.min(100, (n / registered) * 100) : 0}%`;
  }
}

function spark(buckets) {
  const box = document.getElementById('per-ten');
  box.replaceChildren();
  const now = Date.now();
  const slots = 72; // 12 hours of 10 minutes
  const counts = new Array(slots).fill(0);
  for (const b of buckets) {
    const i = slots - 1 - Math.floor((now - new Date(b.start).getTime()) / 600000);
    if (i >= 0 && i < slots) counts[i] += b.count;
  }
  const top = Math.max(1, ...counts);
  for (const c of counts) {
    const s = document.createElement('span');
    s.style.height = `${(c / top) * 100}%`;
    s.title = `${c}`;
    box.append(s);
  }
}

function text(sel, v) { const el = root.querySelector(sel); if (el) el.textContent = v; }

async function refresh() {
  try {
    const res = await fetch(api + '/stats', { headers: { Accept: 'application/json' } });
    if (!res.ok) return;
    const st = await res.json();
    registered = st.participants.registered || 0;
    text('[data-stat="registered"]', registered);
    text('[data-stat="checked_in"]', st.checked_in);
    text('[data-stat="pending"]', st.participants.pending || 0);
    text('[data-stat="waitlisted"]', st.participants.waitlisted || 0);
    const list = document.getElementById('checkpoint-bars');
    list.replaceChildren();
    for (const c of st.checkpoints) {
      const li = document.createElement('li');
      li.innerHTML = '<span class="bar-label"></span><span class="bar"><span class="bar-fill"></span></span><span class="bar-n"></span>';
      li.querySelector('.bar-label').textContent = c.name;
      li.querySelector('.bar-fill').dataset.n = c.recorded;
      li.querySelector('.bar-n').textContent = c.recorded;
      list.append(li);
    }
    bars();
    spark(st.per_ten_minutes);
    const rows = document.getElementById('workshop-rows');
    if (rows) {
      rows.replaceChildren();
      for (const w of st.workshops) {
        const tr = document.createElement('tr');
        for (const v of [w.title, w.capacity ? `${w.signed_up} / ${w.capacity}` : w.signed_up, w.waitlisted, w.attended]) {
          const td = document.createElement('td'); td.textContent = v; tr.append(td);
        }
        rows.append(tr);
      }
    }
    const rec = await fetch(api + '/checkins/recent');
    if (rec.ok) {
      const ul = document.getElementById('recent');
      ul.replaceChildren();
      for (const r of (await rec.json()).slice(0, 20)) {
        const li = document.createElement('li');
        if (r.undone_at) li.className = 'undone';
        const b = document.createElement('b'); b.textContent = r.full_name;
        const s = document.createElement('span'); s.className = 'muted';
        s.textContent = ` ${r.checkpoint} · ${r.staff_name} · ${new Date(r.created_at).toLocaleTimeString()}${r.undone_at ? ' · undone' : ''}`;
        li.append(b, s);
        ul.append(li);
      }
    }
    document.getElementById('updated').textContent = new Date().toLocaleTimeString();
  } catch { /* try again on the next change */ }
}

bars();
refresh();
const es = new EventSource(api + '/stream');
let timer = null;
es.addEventListener('change', () => { clearTimeout(timer); timer = setTimeout(refresh, 300); });
es.addEventListener('open', () => { document.getElementById('live-state').textContent = 'Live'; });
es.addEventListener('error', () => { document.getElementById('live-state').textContent = 'Reconnecting…'; });
