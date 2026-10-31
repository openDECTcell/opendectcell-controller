# ADR 0006: Admin UI framework and menu structure

Status: decided 2026-11-01 (maintainer: "Tabler looks good", map and English-first accepted). Pending review.

## Decision

- **Tabler 1.6.1** (MIT, Bootstrap 5) for layout and components, vendored in
  `internal/web/static/vendor/tabler/` with its licence; **Tabler icons** (MIT)
  inlined as SVG by a template function; **Leaflet 1.9.4** (BSD-2) for the
  position picker. Our own CSS and JS are `app.css`, `app.js`, `theme.js`.
- Everything stays server-rendered (ADR 0001). No CDN, no web font, no
  analytics: the strict Content-Security-Policy (`default-src 'self'`, no
  inline script or style) is unchanged, with one admin-chosen exception: the
  map tile host, named in `img-src` only while an admin has configured one.
- **English first.** Strings are in the templates; translation (German, others)
  is a later task that needs a string table first.
- **Dark and light** from the system setting, switchable per browser
  (`localStorage`, no cookie, nothing sent to the server).

## Alternatives

| | Why not |
|---|---|
| AdminLTE (MIT, Bootstrap 5) | Older look, jQuery in the default bundle |
| CoreUI free | The free tier lacks parts we need; pro features are paid |
| A Node/SPA admin (React-admin, Refine) | Contradicts ADR 0001: a toolchain, hundreds of transitive licences, client-side auth state |
| Hand-written CSS (what we had, 2.4 KB) | Looked unfinished; every component would be ours to build and keep accessible |

## Menu structure

Modelled on what TTN's console, UniFi, Home Assistant and Keycloak/Authentik
admin consoles do: a sidebar grouped by task, role-dependent, with the
administration group visibly separate, and planned pages shown (marked "soon")
so the whole structure is reviewable before every page exists.

| Group | Entries (✓ built, · stub) |
|---|---|
| Phones | Overview ✓ · Handsets · · Phone lines ✓ · Calls · · Contacts ✓ |
| Base stations | My base stations ✓ (profile, map position, sharing) · Enroll ✓ · Public map · |
| Insight | Statistics ✓ (own equipment) · Logging ✓ (read-only, e-mail opt-in) |
| Administration (admins) | Dashboard ✓ · Cells ✓ · All handsets · · Users ✓ · All calls ✓ · Statistics ✓ · Logging ✓ (control) · Audit log ✓ · Settings ✓ · System ✓ |
| Account | Account ✓ |

Not yet in the menu, on the list in `docs/ui-backlog.md`: two-factor login,
personal API tokens, sessions list, data export and account deletion,
notifications centre, backup page.

## Privacy rules the UI follows

1. Nothing is public unless its owner switched it on, per field (name,
   position, specs, statistics), all off by default; public views are built by
   one function (`cell.Cell.Public`) that cannot emit an unshared field.
2. The shared position is rounded to the precision the owner chose.
3. No request leaves for a third party unless an admin configured a tile
   server, and the settings page says what that costs (visitors' addresses).
4. Audit entries record switches, never coordinates.
5. A logo may be PNG, JPEG or WebP only (never SVG), is sniffed on upload and
   served with `nosniff` and a sandboxing CSP.
