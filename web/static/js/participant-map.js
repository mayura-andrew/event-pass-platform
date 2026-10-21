// Participant map (beta): live locations of participants who chose to share,
// and the list of public profiles. Polls every 10 s; while sharing, sends this
// phone's position at most every 15 s and deletes it on Stop or leaving.
const root = document.getElementById('pmap');
const api = root.dataset.api;
const csrf = root.dataset.csrf;
const $ = (id) => document.getElementById(id);
const headers = { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf };

// ---- Map ---------------------------------------------------------------------------
const venue = root.dataset.venueLat ? [Number(root.dataset.venueLat), Number(root.dataset.venueLng)] : null;
const map = L.map('map', { zoomControl: true }).setView(venue || [6.9271, 79.8612], venue ? 17 : 12);
L.tileLayer('https://tile.openstreetmap.org/{z}/{x}/{y}.png', {
  maxZoom: 19,
  attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
}).addTo(map);
if (venue) {
  L.circleMarker(venue, { radius: 8, color: '#2350C8', weight: 3, fillOpacity: 0.2 }).addTo(map)
    .bindTooltip(root.dataset.venueName || 'Venue');
}
const markers = new Map(); // participant id -> marker
let fitted = false;

function initials(name) {
  return name.split(/\s+/).filter(Boolean).slice(0, 2).map((w) => [...w][0]).join('').toUpperCase();
}
function avatar(p, cls) {
  if (p.photo_version) {
    const img = document.createElement('img');
    img.src = `${root.dataset.photo}${p.id}?v=${p.photo_version}`;
    img.alt = '';
    img.className = cls;
    img.loading = 'lazy';
    return img;
  }
  const s = document.createElement('span');
  s.className = `${cls} avatar-initials`;
  s.textContent = initials(p.name);
  return s;
}
function ago(iso) {
  const s = Math.max(0, Math.round((Date.now() - new Date(iso)) / 1000));
  if (s < 60) return 'just now';
  return `${Math.round(s / 60)} min ago`;
}
function linkList(p) {
  const ul = document.createElement('span');
  ul.className = 'person-links';
  for (const [k, label] of [['linkedin', 'LinkedIn'], ['github', 'GitHub'], ['website', 'Website']]) {
    if (!p.links || !p.links[k]) continue;
    const a = document.createElement('a');
    a.href = p.links[k];
    a.textContent = label;
    a.target = '_blank';
    a.rel = 'noopener noreferrer nofollow ugc';
    ul.append(a);
  }
  return ul;
}
function card(p) {
  const div = document.createElement('div');
  div.className = 'person';
  div.append(avatar(p, 'person-photo'));
  const text = document.createElement('div');
  const b = document.createElement('b');
  b.textContent = p.me ? `${p.name} (you)` : p.name;
  text.append(b);
  if (p.headline) {
    const h = document.createElement('span');
    h.className = 'muted';
    h.textContent = p.headline;
    text.append(h);
  }
  const where = document.createElement('span');
  where.className = 'hint';
  where.textContent = p.lat != null ? `On the map · ${ago(p.located_at)}` : 'Not sharing location';
  text.append(where, linkList(p));
  div.append(text);
  return div;
}
function markerIcon(p) {
  const el = document.createElement('div');
  el.className = `map-pin${p.me ? ' map-pin-me' : ''}`;
  el.append(avatar(p, 'map-pin-photo'));
  return L.divIcon({ html: el, className: '', iconSize: [44, 44], iconAnchor: [22, 22], popupAnchor: [0, -22] });
}

let people = [];
function render(data) {
  const all = [...(data.me && data.me.lat != null ? [data.me] : []), ...data.people];
  // Markers: add, move, remove.
  const seen = new Set();
  for (const p of all) {
    if (p.lat == null) continue;
    seen.add(p.id);
    let m = markers.get(p.id);
    if (!m) {
      m = L.marker([p.lat, p.lng], { icon: markerIcon(p), title: p.name, keyboard: true }).addTo(map);
      markers.set(p.id, m);
    } else {
      m.setLatLng([p.lat, p.lng]);
    }
    m.bindPopup(card(p));
  }
  for (const [id, m] of markers) if (!seen.has(id)) { m.remove(); markers.delete(id); }
  if (!fitted && !venue && markers.size) {
    map.fitBounds(L.featureGroup([...markers.values()]).getBounds().pad(0.3), { maxZoom: 17 });
    fitted = true;
  }
  people = data.people;
  renderList();
}
function renderList() {
  const q = $('people-search').value.trim().toLowerCase();
  const ul = $('people');
  ul.replaceChildren();
  const shown = people.filter((p) => !q || p.name.toLowerCase().includes(q) || (p.headline || '').toLowerCase().includes(q));
  for (const p of shown) {
    const li = document.createElement('li');
    li.append(card(p));
    if (p.lat != null) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'btn btn-quiet';
      btn.textContent = 'Show on map';
      btn.addEventListener('click', () => { map.setView([p.lat, p.lng], 18); markers.get(p.id)?.openPopup(); });
      li.append(btn);
    }
    ul.append(li);
  }
  $('people-count').textContent = people.length ? `(${people.length})` : '';
  $('people-empty').hidden = people.length > 0;
}
$('people-search').addEventListener('input', renderList);

async function poll() {
  try {
    const res = await fetch(`${api}/map`, { headers: { 'X-CSRF-Token': csrf } });
    if (res.status === 403) { stopSharing(); $('people-empty').textContent = 'The organizer has turned the map off.'; $('people-empty').hidden = false; return; }
    if (res.ok) render(await res.json());
  } catch { /* try again next time */ }
}
poll();
setInterval(() => { if (!document.hidden) poll(); }, 10000);
document.addEventListener('visibilitychange', () => { if (!document.hidden) poll(); });

// ---- Sharing my location -----------------------------------------------------------
let watch = null;
let keepAlive = null;
let lastSent = 0;
let lastPos = null;
const shareBtn = $('share');
function status(text) { $('share-status').textContent = text; }

async function send(pos) {
  lastPos = pos;
  if (Date.now() - lastSent < 15000) return;
  lastSent = Date.now();
  try {
    const res = await fetch(`${api}/location`, { method: 'PUT', headers,
      body: JSON.stringify({ lat: pos.coords.latitude, lng: pos.coords.longitude, accuracy: pos.coords.accuracy || 0 }) });
    if (res.ok) {
      status(`Sharing · updated ${new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })} · accurate to about ${Math.round(pos.coords.accuracy || 0)} m`);
      poll();
    } else {
      const data = await res.json().catch(() => ({}));
      status(data.error || `Couldn't share (HTTP ${res.status}).`);
      stopSharing();
    }
  } catch { status('No connection: will retry.'); lastSent = 0; }
}
function startSharing() {
  if (!navigator.geolocation) { status("This browser can't share its location."); return; }
  status('Asking for your location…');
  watch = navigator.geolocation.watchPosition(send, (err) => {
    status(err.code === err.PERMISSION_DENIED
      ? 'Location permission was refused. Allow it for this site in your browser settings to share.'
      : "Couldn't find your location. Try outdoors or near a window.");
    stopSharing(false);
  }, { enableHighAccuracy: true, maximumAge: 10000, timeout: 30000 });
  shareBtn.textContent = 'Stop sharing';
  shareBtn.setAttribute('aria-pressed', 'true');
  // A position that doesn't change still needs refreshing to stay "fresh".
  keepAlive = setInterval(() => { if (lastPos) send(lastPos); }, 60000);
}
function stopSharing(clear = true) {
  if (watch !== null) navigator.geolocation.clearWatch(watch);
  watch = null;
  clearInterval(keepAlive);
  lastSent = 0;
  if (shareBtn) {
    shareBtn.textContent = 'Share my location';
    shareBtn.setAttribute('aria-pressed', 'false');
  }
  if (clear) {
    fetch(`${api}/location`, { method: 'DELETE', headers, keepalive: true }).then(poll).catch(() => {});
    status('Not sharing. Your location was removed from the map.');
  }
}
if (shareBtn) {
  shareBtn.addEventListener('click', () => (watch === null ? startSharing() : stopSharing()));
  window.addEventListener('pagehide', () => { if (watch !== null) stopSharing(); });
}

// ---- Feedback -----------------------------------------------------------------------
let rating = 0;
const fb = $('feedback');
for (const b of fb.querySelectorAll('[data-rating]')) {
  b.addEventListener('click', () => {
    rating = Number(b.dataset.rating);
    for (const o of fb.querySelectorAll('[data-rating]')) o.setAttribute('aria-pressed', String(o === b));
    fb.querySelector('[type=submit]').disabled = false;
  });
}
fb.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const res = await fetch(`${api}/feedback`, { method: 'POST', headers,
    body: JSON.stringify({ rating, comment: $('fb-comment').value }) }).catch(() => null);
  if (res && res.ok) {
    $('fb-status').textContent = 'Thank you! The organizers will see it.';
    fb.querySelector('[type=submit]').disabled = true;
    $('fb-comment').value = '';
  } else {
    $('fb-status').textContent = "Couldn't send it. Try again in a moment.";
  }
});
