# Database schema

Status: draft, 2026-10-22 (task A3). Pending maintainer review.
DDL: `internal/db/migrations/`. Requires PostgreSQL ≥ 15 (tested on 18).

## 1. Migrations

| Decision | Alternatives | Reason |
|---|---|---|
| **A small runner in `internal/db/migrate.go`** over numbered SQL files embedded in the binary | goose, golang-migrate | ~200 lines we fully control; no extra dependency tree to licence-check; the features needed are few: ordering, a lock, a transaction per migration, rollback |
| One file per version, `NNNN_name.sql`, with a `-- +up` and a `-- +down` section | Separate up/down files | Up and down are reviewed together |
| Versions 1..n without gaps | Timestamps | One linear history in one repository; gaps would only hide mistakes |
| `pg_advisory_lock` around every run | — | Several controller instances may start at once |
| SHA-256 of each applied *up* section stored in `schema_migration`; a mismatch stops the controller | — | An applied migration must never be edited. Changes go in a new migration |
| A database newer than the binary stops the controller | Run anyway | An old binary on a new schema may write data the new code doesn't expect |
| Applied at start-up by `serve` (default), or explicitly with `opendectcell-controller migrate up`; `migrate down [n]` rolls back | Explicit only | One-command bring-up (`first-tasks.md` B2); `serve --no-migrate` turns it off |

## 2. Tables

| Table | Holds | Notes |
|---|---|---|
| `app_user` | Users | `user` is a reserved word. Local users have an Argon2id `password_hash`; OIDC users have `(oidc_issuer, oidc_subject)` and are created on first login. For OIDC users, `role` is re-derived from groups at every login. Username unique case-insensitively |
| `session` | Browser sessions | Keyed by SHA-256 of the cookie token, never the token itself. Idle and absolute expiry |
| `login_failure` | Failed logins | Rate limiting in the database, so it holds across instances |
| `oidc_login` | In-flight OIDC logins | State (hashed), nonce, PKCE verifier; minutes of life |
| `data_key` | Wrapped data keys | `docs/threat-model.md` §4.2. Exactly one active |
| `setting` | Installation-wide policies | JSON values, e.g. `slot_policy` (`cell-protocol.md` §7.4), and `dect_system_identity`: the class B ARI the cells share (EIC, FPN, FPS, made up or allocated, who set it; `internal/cell/identity.go`) |
| `cell` | Cells | States as in `cell-protocol.md` §6.2. Capacities announced by the cell are separate columns (`first-tasks.md` §3.4); `max_calls` is the admin limit, default 1 |
| `cell_ca` | The private cell CA | Key encrypted. Old CAs kept during rotation (`cell-protocol.md` §5.6) |
| `cell_certificate` | Per-cell client certificates | Keyed by SHA-256 fingerprint, which is what the controller checks (`cell-protocol.md` §4.2) |
| `cell_enrollment_token` | One-time tokens | Hash only; single use; optional auto-approve |
| `handset` | Handsets | `owner_user_id` **mandatory** (0004, the maintainers' notes: a handset enrols only through its owner's registration window; a team's handset belongs to an organisation or admin account). UAK encrypted. **TPUI from an installation-wide pool, unique; its low 16 bits (the link signature) unique too** (`cell-protocol.md` §7.4). No slot index here: slots are per cell. `cc_port_id`, `cc_fixed_id` (0003): the identities an FP-originated `{CC-SETUP}` carries, as the handset put them on the air; NULL until learned |
| `handset_home_cell` | A handset's home set of cells | **Many-to-many** (see §3) |
| `cell_slot` | The controller's copy of each chip's subscription table | `(cell, slot)`; state *empty / writing / present / unknown*; a handset is in at most one slot per cell |
| `sip_account` | SIP accounts | `owner_user_id` NULL = shared. Password encrypted. The **M** parameters of `voip-requirements.md` §2 are columns; the S/L ones (session timers, User-Agent, identity headers, STUN, port overrides, digit-collection timeouts, line prefix) live in `advanced` (JSON), validated by the application |
| `handset_line` | Line 1 / 2 → SIP account | 0–2 per handset. One account can serve several handsets |
| `registration_window` (+ `_cell`) | *Unused since 2026-10-31* (ADR 0005): windows live 60 s and are held in memory; the audit log records them. To be dropped by a migration | — |
| `call_log` | Call history | `owner_user_id` is a snapshot, so a user keeps their history after deleting a handset. `missed_read_at` (0002) is the one read mark of a missed call, shared by the line's handsets. `quality` is `media.Quality` as JSON: both legs, both directions, an MOS estimate and the worse leg (`media-engine.md` §7); `{}` when no media flowed |
| `feature_setting` | Per-handset feature modules | `dect-features.md` |
| `contact` + `contact_number` (0002) | Phonebook entries for CAT-iq list access | `owner_user_id` NULL = shared. Up to 5 typed numbers, one default (`list-access.md`) |
| `audit_log` | Audit trail | **Append-only, enforced by trigger**: no UPDATE, DELETE or TRUNCATE, except DELETE inside a transaction that sets `opendectcell.audit_purge = 'on'` (retention). No foreign key on the actor, so entries outlive the user; `actor_name` is a snapshot |

`architecture.md` §3 entities: all represented. `user` → `app_user`;
`handset_line.slot` → `line` (to avoid confusion with chip slots).

## 3. Decisions a reviewer may overrule

| Decision | Alternative | Reason |
|---|---|---|
| **Handset ↔ cell is many-to-many** through `handset_home_cell` (answers `first-tasks.md` §6 question 3 with a proposal) | One home cell plus roaming as an exception | `architecture.md` §5 already lets the user pick *one or more* cells at registration, and `cell-protocol.md` §7.4 places records on the whole home set |
| UUID keys for user-visible objects; identity integers for logs | Integers everywhere | IDs appear in URLs and the API; UUIDs don't leak counts or invite enumeration |
| Enumerations as `text` + `CHECK` | PostgreSQL `ENUM` types | Adding a value is a plain migration, with no `ALTER TYPE` limits inside transactions |
| `ON DELETE RESTRICT` from handset and SIP account to their owner | `CASCADE` | Deleting a user must not silently destroy handsets (UAKs) and accounts; an admin reassigns or deletes them first |
| The line owner rule (a line's account belongs to the handset's owner, or is shared) is checked in the application | A trigger | It needs the two owners compared across tables; the service layer checks it, with a test |
| Encrypted values bound to `(table, column, primary key)` | Plain AES-GCM | A database writer cannot move one handset's UAK into another row (`threat-model.md` §4.2) |

## 4. Encrypted columns

| Column | Plaintext |
|---|---|
| `handset.uak_enc` | 16-byte UAK |
| `sip_account.password_enc` | SIP password |
| `cell_ca.key_enc` | CA private key, PKCS #8 DER |

`internal/db.EncryptedColumns` lists them for key rotation, and a test fails
if any `*_enc` column in the schema is missing from that list.
