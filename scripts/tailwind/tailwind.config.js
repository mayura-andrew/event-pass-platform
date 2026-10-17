// Tailwind build for the admin dashboard. The compiled CSS is committed
// (public/admin/dashboard.css) so the Go build and Docker image need no Node.
// Rebuild after changing classes in public/admin/*.{html,js}:
//
//   npx tailwindcss@3.4.17 -c scripts/tailwind/tailwind.config.js \
//     -i scripts/tailwind/input.css -o public/admin/dashboard.css --minify
//
// Colors are CSS variables (see input.css) so dark mode is one token swap,
// matching the scanner's palette.
/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ['./public/admin/**/*.{html,js}'],
  darkMode: 'media',
  theme: {
    extend: {
      colors: {
        ink: 'rgb(var(--ink) / <alpha-value>)',
        'ink-soft': 'rgb(var(--ink-soft) / <alpha-value>)',
        paper: 'rgb(var(--paper) / <alpha-value>)',
        panel: 'rgb(var(--panel) / <alpha-value>)',
        line: 'rgb(var(--line) / <alpha-value>)',
        action: 'rgb(var(--action) / <alpha-value>)',
        'action-ink': 'rgb(var(--action-ink) / <alpha-value>)',
        go: 'rgb(var(--go) / <alpha-value>)',
        hold: 'rgb(var(--hold) / <alpha-value>)',
        stop: 'rgb(var(--stop) / <alpha-value>)',
        bar: 'rgb(var(--bar) / <alpha-value>)',
      },
      fontFamily: {
        sans: ['"Atkinson Hyperlegible"', 'system-ui', '-apple-system', '"Segoe UI"', 'Roboto', 'sans-serif'],
      },
    },
  },
  plugins: [],
};
