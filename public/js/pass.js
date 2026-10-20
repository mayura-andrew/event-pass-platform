// =============================================================================
// Participant portal: look up a pass, render the universal QR, show the venue.
// =============================================================================
(() => {
  'use strict';

  // The last pass is kept on the device so it still opens at a gate with no
  // signal. "Use a different pass" removes it (shared computers).
  const SAVED_PASS_KEY = 'nsac_saved_pass';
  // A food preference chosen while offline waits here until it reaches the
  // server: { membershipId, value }.
  const PENDING_FOOD_KEY = 'nsac_pending_food';
  const $ = (id) => document.getElementById(id);

  // ---------------------------------------------------------------------------
  // Event schedule: hardcoded demo data. Coordinates are approximate.
  // ---------------------------------------------------------------------------

  const PLACES = {
    hall:  { pin: '1', name: 'BMICH Main Hall',           lat: 6.9013, lng: 79.8722 },
    rooms: { pin: '2', name: 'BMICH Committee Rooms',     lat: 6.9005, lng: 79.8732 },
    lawn:  { pin: '3', name: 'BMICH Lawn (food court)',   lat: 6.9026, lng: 79.8737 },
    uoc:   { pin: '4', name: 'University of Colombo, Faculty of Science', lat: 6.9009, lng: 79.8606 },
  };

  const SCHEDULE = [
    { day: 'Day 1', time: '08:00', name: 'Registration and check-in', place: 'hall' },
    { day: 'Day 1', time: '09:00', name: 'Opening ceremony',          place: 'hall' },
    { day: 'Day 1', time: '10:00', name: 'Hacking begins',            place: 'rooms' },
    { day: 'Day 1', time: '12:30', name: 'Lunch',                     place: 'lawn' },
    { day: 'Day 1', time: '15:00', name: 'Mentor sessions',           place: 'uoc' },
    { day: 'Day 1', time: '19:30', name: 'Dinner',                    place: 'lawn' },
    { day: 'Day 2', time: '08:00', name: 'Breakfast',                 place: 'lawn' },
    { day: 'Day 2', time: '12:00', name: 'Project submission closes', place: 'rooms' },
    { day: 'Day 2', time: '13:00', name: 'Lunch',                     place: 'lawn' },
    { day: 'Day 2', time: '14:00', name: 'Judging and demos',         place: 'hall' },
    { day: 'Day 2', time: '17:00', name: 'Awards and closing',        place: 'hall' },
  ];

  let currentPass = null; // data behind the pass on screen, for the card download

  // ---------------------------------------------------------------------------
  // Data
  // ---------------------------------------------------------------------------

  async function loadParticipantPass(membershipId, email) {
    const params = new URLSearchParams({ id: membershipId.trim(), email: email.trim() });
    const res = await fetch(`/api/v1/participants/pass?${params}`, { cache: 'no-store' });

    if (res.status === 404) {
      throw new Error('No confirmed pass matches that membership ID and email. Check both against your confirmation message.');
    }
    if (res.status === 429) throw new Error('Too many attempts. Wait a minute, then try again.');
    if (!res.ok) throw new Error(`The pass service returned an error (HTTP ${res.status}). Try again shortly.`);
    return res.json();
  }

  // ---------------------------------------------------------------------------
  // Rendering
  // ---------------------------------------------------------------------------

  function renderPass(data, { offline = false } = {}) {
    $('lookup-view').hidden = true;
    $('pass-view').hidden = false;
    $('offline-note').hidden = !offline;
    currentPass = data;
    $('download-error').hidden = true;
    renderFood(data);
    renderSchedule({ offline });

    $('delegate-name').textContent = data.full_name;
    $('delegate-team').textContent = data.team_name ? `Team ${data.team_name}` : 'No team listed';
    $('membership-id').textContent = data.membership_id;

    // Universal QR payload, identical to what the scanner expects.
    const qrPayload = JSON.stringify({ id: data.id, sig: data.qr_signature });
    const target = $('qrcode');
    target.replaceChildren();
    new QRCode(target, {
      text: qrPayload,
      width: 480,              // drawn at 2x and scaled by CSS for sharp screens
      height: 480,
      colorDark: '#000000',
      colorLight: '#ffffff',
      correctLevel: QRCode.CorrectLevel.M, // M survives cracked screens better than L
    });

    const venue = data.venue;
    if (!venue) {
      $('venue-details').textContent = 'Your venue has not been assigned yet. Check back closer to the event.';
      $('venue-description').hidden = true;
      $('map-container').hidden = true;
      $('directions-link').hidden = true;
      return;
    }

    $('venue-details').textContent = `${venue.name}, ${venue.hall_section}`;
    $('venue-description').textContent = venue.description || '';
    $('venue-description').hidden = !venue.description;
    $('directions-link').hidden = false;
    $('directions-link').href =
      `https://www.google.com/maps/dir/?api=1&destination=${venue.latitude},${venue.longitude}`;

    if (offline) {
      $('map-container').hidden = true; // map tiles need a connection
    } else {
      $('map-container').hidden = false;
      initMap(Number(venue.latitude), Number(venue.longitude), `${venue.name}, ${venue.hall_section}`)
        .catch((err) => {
          console.warn('Map unavailable:', err);
          $('map-container').hidden = true;
        });
    }
  }

  // ---------------------------------------------------------------------------
  // Food preference
  // ---------------------------------------------------------------------------
  // Saved as soon as it changes. Offline, the choice waits in localStorage and
  // is sent when the connection returns (or the page is next opened).

  function readPendingFood() {
    try { return JSON.parse(localStorage.getItem(PENDING_FOOD_KEY) || 'null'); } catch { return null; }
  }

  function setFoodStatus(text, { error = false } = {}) {
    $('food-status').textContent = text;
    $('food-status').classList.toggle('is-error', error);
  }

  function renderFood(data) {
    const options = data.food_options || [];
    $('food-panel').hidden = options.length === 0; // older saved copies lack the list
    if (!options.length) return;

    const pending = readPendingFood();
    const mine = pending?.membershipId === data.membership_id;
    const value = mine ? pending.value : (data.food_preference || '');

    const select = $('food-select');
    const opt = (v, label = v) => Object.assign(document.createElement('option'), { value: v, textContent: label });
    select.replaceChildren();
    if (!value) select.append(Object.assign(opt('', 'Choose your preference'), { disabled: true }));
    // A value from the registration form that isn't one of today's choices.
    if (value && !options.includes(value)) select.append(opt(value));
    options.forEach((o) => select.append(opt(o)));
    select.value = value;

    if (mine) {
      syncFood();
    } else {
      setFoodStatus(value ? '' : 'Not set yet.');
    }
  }

  function onFoodChange() {
    if (!currentPass) return;
    const value = $('food-select').value;
    localStorage.setItem(PENDING_FOOD_KEY, JSON.stringify({ membershipId: currentPass.membership_id, value }));
    currentPass.food_preference = value;
    syncFood();
  }

  let foodSyncing = false;

  async function syncFood() {
    if (foodSyncing) return;
    foodSyncing = true;
    try {
      // Loops if the delegate picks again while a save is in flight.
      for (;;) {
        const pending = readPendingFood();
        const saved = readSaved();
        if (!pending || saved?.data?.membership_id !== pending.membershipId) return;

        setFoodStatus('Saving…');
        let res;
        try {
          res = await fetch('/api/v1/participants/pass/food-preference', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: saved.membershipId, email: saved.email, food_preference: pending.value }),
          });
        } catch {
          setFoodStatus("Saved on this device. It will sync when you're back online.");
          return;
        }

        if (!res.ok) {
          // Busy or failing: keep it queued for the next retry. Rejected: drop it.
          if (res.status === 429 || res.status >= 500) {
            setFoodStatus('Saved on this device. The server is busy, so it will retry shortly.');
          } else {
            localStorage.removeItem(PENDING_FOOD_KEY);
            const body = await res.json().catch(() => ({}));
            setFoodStatus(body.error || `Your preference could not be saved (HTTP ${res.status}).`, { error: true });
          }
          return;
        }

        const { food_preference: value } = await res.json();
        if (readPendingFood()?.value !== pending.value) continue;
        localStorage.removeItem(PENDING_FOOD_KEY);
        saved.data.food_preference = value;
        localStorage.setItem(SAVED_PASS_KEY, JSON.stringify(saved));
        setFoodStatus('Saved.');
        return;
      }
    } finally {
      foodSyncing = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Schedule
  // ---------------------------------------------------------------------------

  let scheduleMap = null;   // { map, info, markers: { placeKey: marker } } once loaded
  let scheduleMapLoading = null;

  function renderSchedule({ offline }) {
    const list = $('schedule-list');
    list.replaceChildren();
    let ul = null;
    let day = null;
    SCHEDULE.forEach((slot, i) => {
      if (slot.day !== day) {
        day = slot.day;
        list.append(Object.assign(document.createElement('h3'), { className: 'day', textContent: day }));
        ul = list.appendChild(Object.assign(document.createElement('ul'), { className: 'schedule' }));
      }
      const place = PLACES[slot.place];
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.dataset.index = i;
      btn.setAttribute('aria-pressed', 'false');
      btn.append(
        Object.assign(document.createElement('span'), { className: 'slot-time', textContent: slot.time }),
        Object.assign(document.createElement('span'), { className: 'slot-name', textContent: slot.name }),
        Object.assign(document.createElement('span'), { className: 'slot-place', textContent: place.name }),
      );
      btn.lastChild.dataset.pin = place.pin;
      btn.addEventListener('click', () => selectSlot(i));
      const li = document.createElement('li');
      li.append(btn);
      ul.append(li);
    });

    if (offline) {
      $('schedule-map').hidden = true; // map tiles need a connection
      return;
    }
    $('schedule-map').hidden = false;
    scheduleMapLoading ||= initScheduleMap().catch((err) => {
      console.warn('Schedule map unavailable:', err);
      $('schedule-map').hidden = true;
      scheduleMapLoading = null;
    });
  }

  function selectSlot(index) {
    const slot = SCHEDULE[index];
    const place = PLACES[slot.place];
    document.querySelectorAll('#schedule-list button').forEach((b) => {
      b.setAttribute('aria-pressed', String(Number(b.dataset.index) === index));
    });
    const link = $('schedule-directions');
    link.hidden = false;
    link.href = `https://www.google.com/maps/dir/?api=1&destination=${place.lat},${place.lng}`;

    if (scheduleMap) {
      scheduleMap.map.panTo({ lat: place.lat, lng: place.lng });
      scheduleMap.map.setZoom(17);
      openPlaceInfo(slot.place);
      $('schedule-map').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    }
  }

  // Info window listing every session held at one place.
  function openPlaceInfo(key) {
    const { map, info, markers } = scheduleMap;
    const box = document.createElement('div');
    box.append(Object.assign(document.createElement('strong'), { textContent: PLACES[key].name }));
    SCHEDULE.filter((s) => s.place === key).forEach((s) => {
      box.append(document.createElement('br'), `${s.day} ${s.time} · ${s.name}`);
    });
    info.setContent(box);
    info.open({ map, anchor: markers[key] });
  }

  async function initScheduleMap() {
    const cfg = await loadGoogleMaps();
    const { Map, InfoWindow } = await google.maps.importLibrary('maps');
    const map = new Map($('schedule-map'), {
      mapId: cfg.google_maps_map_id || undefined,
      streetViewControl: false,
      mapTypeControl: false,
      fullscreenControl: false,
    });
    const bounds = new google.maps.LatLngBounds();
    const markers = {};
    for (const [key, place] of Object.entries(PLACES)) {
      const position = { lat: place.lat, lng: place.lng };
      markers[key] = await addMarker(map, cfg, position, place.name, place.pin);
      markers[key].addListener(cfg.google_maps_map_id ? 'gmp-click' : 'click', () => openPlaceInfo(key));
      bounds.extend(position);
    }
    map.fitBounds(bounds, 32);
    scheduleMap = { map, info: new InfoWindow(), markers };
  }

  // ---------------------------------------------------------------------------
  // Card download (official template, see pass-card.js)
  // ---------------------------------------------------------------------------

  async function downloadCard(kind, btn) {
    if (!currentPass) return;
    const label = btn.textContent;
    btn.disabled = true;
    btn.textContent = 'Preparing…';
    $('download-error').hidden = true;
    try {
      const canvas = await NSACPassCard.render(currentPass);
      const blob = kind === 'pdf' ? await NSACPassCard.toPDF(canvas) : await NSACPassCard.toPNG(canvas);
      // Membership IDs are short codes, but keep the file name safe anyway.
      const id = String(currentPass.membership_id).replace(/[^A-Za-z0-9_-]+/g, '-');
      NSACPassCard.download(blob, `space-apps-colombo-pass-${id}.${kind}`);
    } catch (err) {
      console.error('Pass download failed:', err);
      $('download-error').textContent = navigator.onLine
        ? 'The pass could not be created. Reload the page and try again.'
        : 'You are offline and the pass design is not saved on this device yet. Connect once, then try again.';
      $('download-error').hidden = false;
    } finally {
      btn.disabled = false;
      btn.textContent = label;
    }
  }

  // ---------------------------------------------------------------------------
  // Google Maps
  // ---------------------------------------------------------------------------
  // The browser key comes from the API (/api/v1/config/public) instead of being
  // pasted into the HTML. Restrict that key to this site's HTTP referrers and
  // to the Maps JavaScript API in Google Cloud Console.

  let mapsReady = null;

  function loadGoogleMaps() {
    if (mapsReady) return mapsReady;
    mapsReady = fetch('/api/v1/config/public')
      .then((r) => r.json())
      .then((cfg) => {
        if (!cfg.google_maps_api_key) throw new Error('GOOGLE_MAPS_API_KEY is not configured');
        return new Promise((resolve, reject) => {
          window.__nsacMapsReady = () => resolve(cfg);
          const s = document.createElement('script');
          s.src = 'https://maps.googleapis.com/maps/api/js'
            + `?key=${encodeURIComponent(cfg.google_maps_api_key)}`
            + '&v=weekly&loading=async&callback=__nsacMapsReady';
          s.async = true;
          s.onerror = () => reject(new Error('Google Maps script failed to load'));
          document.head.appendChild(s);
        });
      });
    return mapsReady;
  }

  async function initMap(lat, lng, venueName) {
    const cfg = await loadGoogleMaps();
    const { Map } = await google.maps.importLibrary('maps');
    const coords = { lat, lng };

    const map = new Map($('map-container'), {
      zoom: 17,
      center: coords,
      mapId: cfg.google_maps_map_id || undefined,
      streetViewControl: false,
      mapTypeControl: false,
      fullscreenControl: false,
    });
    await addMarker(map, cfg, coords, venueName);
  }

  // Advanced markers need a Map ID; the classic Marker is deprecated but is
  // still the fallback when no Map ID is configured. label is an optional
  // short glyph such as "1".
  async function addMarker(map, cfg, position, title, label) {
    if (cfg.google_maps_map_id) {
      const { AdvancedMarkerElement, PinElement } = await google.maps.importLibrary('marker');
      const content = label ? new PinElement({ glyph: label, glyphColor: '#FFFFFF' }).element : undefined;
      return new AdvancedMarkerElement({ map, position, title, content, gmpClickable: true });
    }
    return new google.maps.Marker({ map, position, title, label: label ? { text: label, color: '#FFFFFF' } : undefined });
  }

  // ---------------------------------------------------------------------------
  // Flow
  // ---------------------------------------------------------------------------

  function showLookup(message) {
    $('pass-view').hidden = true;
    $('lookup-view').hidden = false;
    $('lookup-error').textContent = message || '';
    $('lookup-error').hidden = !message;
  }

  async function fetchAndShow(membershipId, email) {
    const data = await loadParticipantPass(membershipId, email);
    localStorage.setItem(SAVED_PASS_KEY, JSON.stringify({ data, membershipId, email }));
    renderPass(data);
  }

  async function onLookup(event) {
    event.preventDefault();
    const btn = $('lookup-submit');
    btn.disabled = true;
    $('lookup-error').hidden = true;
    try {
      await fetchAndShow($('lookup-id').value, $('lookup-email').value);
    } catch (err) {
      showLookup(err.message);
    } finally {
      btn.disabled = false;
    }
  }

  function readSaved() {
    try { return JSON.parse(localStorage.getItem(SAVED_PASS_KEY) || 'null'); } catch { return null; }
  }

  async function boot() {
    $('lookup-form').addEventListener('submit', onLookup);
    $('download-png').addEventListener('click', (e) => downloadCard('png', e.currentTarget));
    $('download-pdf').addEventListener('click', (e) => downloadCard('pdf', e.currentTarget));
    $('food-select').addEventListener('change', onFoodChange);
    // Send a queued food preference as soon as there is a connection again.
    window.addEventListener('online', syncFood);
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') syncFood();
    });
    $('forget-pass').addEventListener('click', () => {
      localStorage.removeItem(SAVED_PASS_KEY);
      localStorage.removeItem(PENDING_FOOD_KEY);
      history.replaceState(null, '', location.pathname);
      showLookup();
    });

    if ('serviceWorker' in navigator) {
      navigator.serviceWorker.register('/sw.js').catch(() => {});
    }

    // Deep link from the confirmation email: /pass.html?id=NSAC-1092&email=...
    const q = new URLSearchParams(location.search);
    const saved = readSaved();
    const id = q.get('id') || saved?.membershipId;
    const email = q.get('email') || saved?.email;

    if (!id || !email) {
      showLookup();
      return;
    }
    $('lookup-id').value = id;
    $('lookup-email').value = email;

    try {
      await fetchAndShow(id, email);
      // Keep the email out of the address bar once it has been used.
      if (q.has('email')) history.replaceState(null, '', location.pathname);
    } catch (err) {
      // Offline (or server unreachable): fall back to the saved copy.
      if (saved && saved.membershipId === id && err instanceof TypeError) {
        renderPass(saved.data, { offline: true });
      } else {
        showLookup(err instanceof TypeError ? 'You appear to be offline. Connect once to load your pass.' : err.message);
      }
    }
  }

  boot();
})();
