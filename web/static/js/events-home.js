// Events home: "/" jumps to search (spec), and the sort menu applies at once.
const search = document.getElementById('event-search');
document.addEventListener('keydown', (e) => {
  const t = e.target;
  const typing = t instanceof HTMLElement && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
  if (e.key === '/' && !typing && !e.ctrlKey && !e.metaKey && !e.altKey && search) {
    e.preventDefault();
    search.focus();
    search.select();
  }
});
for (const el of document.querySelectorAll('select[data-autosubmit]')) {
  el.addEventListener('change', () => el.form.requestSubmit());
}
