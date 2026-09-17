/*
 * Build inputs for internal/api/webui/tailwind.css, which replaces the
 * cdn.tailwindcss.com script the page used to load.
 *
 * Regenerate from this directory with the standalone CLI:
 *   tailwindcss -c tailwind.config.js -i tailwind.input.css -o tailwind.css --minify
 * (https://github.com/tailwindlabs/tailwindcss/releases, v3.4.17, MIT)
 *
 * The committed output is what the Go binary embeds; the CLI is a build-time
 * tool only and is not required to build or run the server.
 */
module.exports = {
  content: ['./index.html', './app.js', './hardening.js'],
  theme: { extend: {} },
  plugins: [],
};
