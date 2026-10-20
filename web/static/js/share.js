// Share tab: the one-time link is selected on focus, ready to copy.
for (const el of document.querySelectorAll('[data-select-on-focus]')) {
  el.addEventListener('focus', () => el.select());
  el.focus();
}
