# UI backlog

Written 2026-11-01 from the research for the interface rework (TTN console,
Keycloak/Authentik, UniFi-style consoles, auth checklists of similar projects)
and from what a privacy-minded user and an operator hosting many cells
(makerspace networks, a hosting provider; assumed from the cut-off sentence in
the request) would expect. Built items are in ADR 0006; this is what is left.

| # | Item | Why | Notes |
|---|---|---|---|
| B1 | Handsets page (list, register through a PIN window, name, lines) | The core user task; the API exists | C6 — **built 2026-11-01 (removal of a handset is not: it must deregister on every cell)** |
| B2 | Calls page for users, CSV export | Own history | uses `calllog` |
| B3 | Public map and a read-only JSON feed | Sharing switches exist; nothing consumes them | `cell.Public()`; add statistics counts only where `share_stats` — **built 2026-11-01** |
| B4 | Two-factor login: TOTP, then WebAuthn; recovery codes | Standard for an admin console | admins first — **TOTP and recovery codes built 2026-11-01; WebAuthn and enforcing it for admins are not** |
| B5 | Personal API tokens (C7 in the roadmap) | Scripts, the map feed | hashed, scoped, shown once |
| B6 | Sessions list and "log out everywhere"; new-device e-mail | Account hygiene | **sessions list and "log out everywhere else" built 2026-11-04** (Account > Where am I logged in; a session keeps a short browser label, no address); the new-device e-mail is not |
| B7 | E-mail verification for self-registration | The address is not verified today | needs SMTP; approval is the stopgap — **built 2026-11-01 (mandatory once SMTP is configured)** |
| B8 | Password reset by e-mail | Self-registered users have no other way | needs SMTP, B7 — **built 2026-11-01** |
| B9 | Data export (JSON) and account deletion with a grace period | GDPR Art. 20 and 17 | handsets must be moved or removed first |
| B10 | Retention settings: call log detail vs aggregates, audit log age | Privacy and disk | statistics already read aggregates — **built 2026-11-01 (call details and audit entries; defaults keep everything)** |
| B11 | Notification centre in the UI (cell offline, certificate ending, sign-up waiting) | Operators | the dashboard shows these now |
| B12 | Alerts by e-mail to admins: cell offline for n minutes | Operators | needs a scheduler — **built 2026-11-01 (cell offline for 10 minutes, back online; admins who opt in)** |
| B13 | Prometheus metrics endpoint | Hosting | admin token, B5 — **built 2026-11-01 (`/metrics` with a bearer token)** |
| B14 | Backup and restore page (and what to back up: the master key separately) | Hosting | docs first — **built 2026-11-01 as an explaining page** |
| B15 | Per-call log lines from the DECT core and the SIP side | The calls level has only the call log's three lines | the call path writes call-log rows now; per-call log lines are still not produced |
| B16 | German and other translations | After English | needs a string table |
| B17 | Legal pages: imprint, privacy notice, "what we store" | Public instances | text set by the admin |
| B18 | Map tiles from the controller itself (a caching proxy) | A map without telling a third party | optional; self-hosted tiles work today |
| B19 | Accessibility pass (keyboard, contrast, screen reader on the charts) | Public instances | charts have text alternatives; no audit yet |
| B20 | Welcome page when no cell, no line, no handset exists | First run | the overview has a checklist |
