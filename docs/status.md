# What works, and what doesn't yet

The state of opendectcell-controller on 2026-11-03 (one status table, as in openDECTcell's `docs/status.md`; it answers audit finding
DOC-01). "Seen" means seen working with real cells and handsets; "tests" means covered by the automated tests only.

Legend: ✅ seen working · 🟡 built and tested by the test suite, or seen only in part · ❌ not built · ➖ out of scope

## Accounts and access

| Feature | State | Notes |
|---|---|---|
| Local users, Argon2id, sessions, rate limits | ✅ | |
| Single sign-on (OIDC) with group-to-role mapping | 🟡 | Tests against a test provider; an administrator's session ends after 12 h so a withdrawn role does not linger |
| Self sign-up with e-mail check and optional approval | 🟡 | Off by default |
| Two-factor login (TOTP, recovery codes, QR code) | 🟡 | Offered to everyone, required of administrators by a setting; asked on a new device and every three weeks on a known one. Setup, new-device prompt and known-device login are driven in headless Chrome by `tools/browser-smoke/run.sh`; no authenticator app has scanned the QR code yet |
| Sessions list, end one, "log out everywhere else" | 🟡 | Account > Where am I logged in; trusted browsers stay trusted until "ask on every device again" |
| Handsets private to their owner; "Disclose to administrators" per handset and base station | 🟡 | ADR 0008 |
| Organizations and the trust setting (all / own / own plus organizations) | 🟡 | ADR 0007; every page loads without console or CSP errors in headless Chrome (`tools/browser-smoke/run.sh`); the organization forms themselves have not been clicked through |
| REST API | 🟡 | `docs/api/openapi.yaml`; a test fails when a route is not in the document |

## Base stations (cells)

| Feature | State | Notes |
|---|---|---|
| Enrolment with a one-time token, mutual TLS, certificate renewal | ✅ | A box on the same LAN and a remote box across the internet (IPv6) |
| Enrolled cells are active at once (no approval) | ✅ | Who may enroll is a setting |
| Cell on the controller's address as the cell reached it (media address per cell) | ✅ | `OPENDECTCELL_CELLS_MEDIA_IP=auto` |
| Revocation, periodic re-check, disclosure to administrators | 🟡 | |
| Public map and sharing switches | 🟡 | Off by default |
| Updating a cell's software from the controller | ❌ | |

## Handsets and calls

| Feature | State | Notes |
|---|---|---|
| Registering a handset through a window on any cell | ✅ | The handset is shown online at once |
| Handsets shared by all cells (pool), roaming between cells | 🟡 | Seen between two cells; a working set per cell |
| Calls in and out through SIP accounts, G.722, ciphering | ✅ | |
| Calls between handsets of one account | 🟡 | |
| Phonebook, call lists, message-waiting lamp | 🟡 | Built; not all seen on hardware |
| CardDAV address books (read only, several, chosen per handset), vCard and CSV import and export of contacts | 🟡 | Tests against a fake server; not tried against Nextcloud; private network addresses refused unless the operator allows them |
| Metrics: pool, per base station, per line, call quality histogram, queues, backups; Prometheus alert rules and a Grafana dashboard in deploy/monitoring | 🟡 | Format validated by tests and `promtool check metrics`, the rules by `promtool check rules`; the dashboard not imported into a Grafana |
| Fleet view: base stations side by side, software versions and which are behind, busiest and quietest, CSV | 🟡 | Tests; "behind" needs the cells to report a comparable version |
| Administration > Health: checks for database, schema, clock, mail, base stations, certificates, lines, backups, space; optional public /status | 🟡 | Tests; no check for DECT workers or the database disk |
| Account > My activity (own sign-ins, failed attempts, changes, from the audit log) | 🟡 | Tests; the sentences for rarer actions are plain |
| Webhooks: signed posts for calls, handset presence and notifications, with retries and a switch-off | 🟡 | Tests with a real HTTP receiver; not tried with Home Assistant or ntfy; the delivery needs the controller to reach the receiver |
| Personal API tokens with scopes (no administrator scope), the handsets, calls and statistics endpoints | 🟡 | Tests, including that a token with every scope cannot reach the administrators' API; not tried by a real client |
| Statistics: peaks at once (per day, per base station against capacity), weekday and hour, quality, per line and handset, line availability, against the previous period, CSV, monthly usage report | 🟡 | Tests against the database and the pages; not looked at with a month of real data; presence and set-up time are not built |
| Scheduled backups: nightly encrypted dump, restore check, SFTP copy, retention; state, download and alerts in the interface | 🟡 | Proven on throwaway containers with a real SFTP server (`tools/backup-test.sh`); not run on the Pi; S3 not built |
| Notification centre: a bell, VoIP line down, handset not heard (opt-in), base station offline, certificates ending; mail with quiet hours | 🟡 | Tests; not seen with real failures on the Pi; disk, worker restart, release and backup alerts are not built |
| VoIP line page: registration history (up, down, why, how long), share of time registered, last calls | 🟡 | Tests; not yet seen with a real provider failing |
| Export and import of handsets (with keys, encrypted), VoIP lines (with passwords) and contacts; VoIP lines from a CSV | 🟡 | Tests against the database; a handset moved with its key has not been tried on air |
| Calls page: your history with filters, a page per call (codecs, encryption, quality per leg), CSV export, missed calls marked as read | 🟡 | Tests and headless Chrome; the numbers are the user's own and shown in full |
| Transfer, conference, early media | ❌ | |

## Operation

| Feature | State | Notes |
|---|---|---|
| Docker Compose deployment, least-privilege database role, one-shot migration | ✅ | `docs/deployment.md` |
| Test ground on a Raspberry Pi with Let's Encrypt, DynDNS and a host firewall | ✅ | `docs/test-ground.md` |
| DECT stacks in worker processes | 🟡 | The 72 h soak runs on the Pi from 2026-11-04 (SEC-03): crash injection and samples, no calls yet |
| Backup page, retention, audit log, metrics | 🟡 | `tools/restore-test.sh` proves a dump and the master key restore a working controller (BUILD-06); backups are not scheduled |
| Export and erasure of one user's data | 🟡 | Account page: a JSON download (no secrets) and "Delete my account and data" (handsets, VoIP lines, base stations revoked, contacts, calls; the audit log keeps the entries but forgets who) |
| An administrator releases a handset registered under another user's name | 🟡 | Administration > Release a handset; audited (SEC-12) |
| High availability | ➖ | One controller, one database |

More in [wiki/Limits.md](wiki/Limits.md) (for users), [next-steps-2026-11-02.md](next-steps-2026-11-02.md) and the open
audit items in [security-review-2026-11-02.md](security-review-2026-11-02.md).
