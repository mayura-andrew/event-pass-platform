// Suggests the URL slug from the organization name until the slug is edited.
const name = document.getElementById('org-name');
const slug = document.getElementById('org-slug');
if (name && slug) {
  let touched = slug.value !== '';
  slug.addEventListener('input', () => { touched = true; });
  name.addEventListener('input', () => {
    if (touched) return;
    slug.value = name.value.toLowerCase().normalize('NFKD').replace(/[̀-ͯ]/g, '')
      .replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40).replace(/-+$/, '');
  });
}
