# AGENTS.md: rules for AI agents (and humans) working on opendectcell-controller

Read this completely before changing anything. For the current project state, see the maintainers' notes. If a rule is unclear or wrong, propose a change to this file in its own commit; don't silently deviate. The sister repository **openDECTcell** (cell side: firmware loader, kernel module, `libopendectcell`) has its own `AGENTS.md`. Its clean-room rules apply here too.

## 1. What this project is, and what it is not

- **It is** a DECT↔SIP translator. Each handset is represented by up to **two SIP user agents** (lines). The controller also provides the web UI, the admin UI and cell management.
- **It is not a PBX:**
  - no dial plan, call routing or hunt groups
  - no voicemail storage, IVR or conferencing server
  - no SIP registrar or proxy for third parties

  Feature requests that belong in a PBX are declined, or solved by passing data through: for example, showing the PBX's message-waiting state on the handset.
- Handset-to-handset calls go through the SIP server like any other call.

## 2. Clean-room and blob rules (inherited from openDECTcell)

1. Never commit AVM material (binaries, firmware, decompiled code, calibration data).
2. Never paste decompiled or disassembled code anywhere. Use facts in your own words, with the source named.
3. The DECT protocol logic comes from `libopendectcell` (C) in the openDECTcell repo. Don't fork it into this repo; use it as a dependency.

## 3. Licence

- **AGPL-3.0-or-later.** Every source file carries `SPDX-License-Identifier: AGPL-3.0-or-later`.
- Dependencies must be AGPL-3.0 compatible (GPL-2.0-**or-later**, GPL-3.0, LGPL, MIT, BSD, Apache-2.0). GPL-2.0-**only** code cannot be linked in. This also applies to `libopendectcell` from the openDECTcell repo, which therefore must be GPL-2.0-or-later or more permissive.

## 4. Commit and push discipline

- **Commit early and often:** one logical change per commit (a doc section, a model plus its migration, a handler plus its tests).
- **Push after every commit** (or every few small ones), so changes stay traceable and easy to revert.
- **Never force-push** or rewrite published history; use `git revert` or follow-up commits.
- **Message format:** an imperative summary of 72 characters or less, a blank line, then the *why*. Reference issues (`Refs #n`, `Fixes #n`), plus the attribution line your tool requires.
- **Check `git diff --staged` before committing.** Never commit secrets, `.env` files, keys, certificates or databases.
- **Author:** `ctandi <ctandi@users.noreply.github.com>` (repo-local).

## 5. Security rules

- **Secrets encrypted at rest:**
  - DECT authentication keys (UAK/DCK) and SIP credentials are encrypted in the database. The key comes from config or a secret store, never from the repo.
  - Logs never contain secrets, PINs or keys.
- **Cell enrollment:** cells enroll with a one-time token and then authenticate with **mutual TLS** (per-cell certificate). There are no shared static passwords.
- **Access control:** every web or API action is checked server-side against the user's role (admin / user) and ownership (a user only sees their own handsets and accounts).

## 6. Feature framework rule

DECT features beyond basic calls (see `docs/dect-features.md`) are implemented as **feature modules**:
- Each module declares the handset capabilities or models it needs.
- It is enabled per handset, based on the capabilities the handset signalled and a model-quirks table.
- The core call path must never depend on an optional feature module.

## 7. Testing

- Unit tests for every module. SIP interop is tested against at least Asterisk and FreeSWITCH, ideally also a FRITZ!Box as registrar.
- DECT-side tests use openDECTcell's simulator and golden traces until real cells are available.
- **No implementation before openDECTcell gate G7.** Design documents, data model and UI mock-ups are fine before then.

## 8. Documentation

- Keep `docs/` current. Every design decision gets a short note: the decision, the alternatives and the reason.
- Write in English, concisely, preferring tables.
