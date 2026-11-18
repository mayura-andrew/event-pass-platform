// Sign-in links carry their one-time token in the URL fragment, which the
// browser never sends to the server. Move it into the form and wipe it from
// the address bar and history, so it isn't left behind on this device.
(function () {
  var input = document.getElementById("verify-token");
  var missing = document.getElementById("verify-missing");
  var form = document.getElementById("verify-form");
  if (!input || !form) return;
  var m = /(?:^#|&)token=([^&]+)/.exec(window.location.hash || "");
  if (m) input.value = decodeURIComponent(m[1]);
  if (window.location.hash || window.location.search) {
    try { window.history.replaceState(null, "", window.location.pathname); } catch (e) {}
  }
  if (!input.value) {
    form.hidden = true;
    if (missing) missing.hidden = false;
  }
})();
