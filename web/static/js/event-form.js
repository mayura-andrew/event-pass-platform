// New event: start in the browser's time zone when the list has it.
const tz = document.getElementById('ev-tz');
if (tz && location.pathname.endsWith('/events/new') && tz.value === 'UTC') {
  try {
    const mine = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if ([...tz.options].some((o) => o.value === mine)) tz.value = mine;
  } catch { /* keep UTC */ }
}
