# Vendored front-end files

Nothing is loaded from a CDN at runtime (`docs/adr/0001-web-frontend.md`,
`docs/adr/0006-admin-ui-framework.md`). These files are copied, unchanged, from
the npm packages named here; each directory keeps its licence file.

| Path | Package | Version | Licence |
|---|---|---|---|
| `vendor/tabler/tabler.min.css`, `tabler.min.js` | `@tabler/core` | 1.6.1 | MIT |
| `vendor/tabler/icons/*.svg` | `@tabler/icons` (outline set) | 3.35.0 | MIT |
| `vendor/leaflet/` | `leaflet` | 1.9.4 | BSD-2-Clause |

Our own files: `app.css`, `app.js`, `theme.js`. To update a vendored file, take
it from the same package at the new version, change the table, and run the web
tests (they check that a page references nothing outside its own origin).
