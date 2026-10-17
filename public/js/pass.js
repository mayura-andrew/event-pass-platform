// =============================================================================
// Participant portal: look up a pass, render the universal QR, show the venue.
// =============================================================================
(() => {
  'use strict';

  // The last pass is kept on the device so it still opens at a gate with no
  // signal. "Use a different pass" removes it (shared computers).
  const SAVED_PASS_KEY = 'nsac_saved_pass';
  const $ = (id) => document.getElementById(id);

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

    // Advanced markers need a Map ID; the classic Marker is deprecated but is
    // still the fallback when no Map ID is configured.
    if (cfg.google_maps_map_id) {
      const { AdvancedMarkerElement } = await google.maps.importLibrary('marker');
      new AdvancedMarkerElement({ map, position: coords, title: venueName });
    } else {
      new google.maps.Marker({ map, position: coords, title: venueName });
    }
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
    $('forget-pass').addEventListener('click', () => {
      localStorage.removeItem(SAVED_PASS_KEY);
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
