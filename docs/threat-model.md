# Threat model and secret handling

Status: draft, 2026-10-22 (task A5); the shared network (B8, A9) added
2026-10-31. Pending maintainer review.

Scope: one installation, i.e. one controller (Docker + PostgreSQL), its cells
and handsets, one or more SIP servers, and the users' browsers. *Changed
2026-10-31 (maintainer):* the installation is a **shared network** of
everyone's cells and handsets (`adr/0004-shared-network.md`): a handset may
attach to a cell someone else runs, so the cells' operators are now
distinct parties (A9, B8).

## 1. Assets

| # | Asset | Where it lives | Impact if lost |
|---|---|---|---|
| S1 | **UAK**, a handset's 16-byte long-term DECT key | DB (encrypted); controller memory during a session; inside a subscription write on the cell link and on the cell's chip | Impersonate the handset to the FP or the FP to the handset; derive the per-call DCK and decrypt recorded air traffic of that handset |
| S2 | **SIP credentials** (auth user + password) | DB (encrypted); controller memory | Toll fraud and call impersonation at the PBX or provider |
| S3 | **Master key** (KEK) | Outside the DB: environment, file or Docker secret | Together with a DB dump: S1, S2, S4 |
| S4 | **Cell CA private key** | DB (encrypted with the KEK) | Issue certificates for rogue cells; impersonate the controller to cells |
| S5 | Per-cell private keys | Cell flash only; never leaves the cell | Impersonate that cell until revoked |
| S6 | User passwords (local accounts) | DB, Argon2id hash only | Account takeover |
| S7 | Session tokens | Browser cookie; DB stores a SHA-256 hash | Account takeover for the session's life |
| S8 | OIDC client secret | Config | Abuse of the IdP client registration |
| S9 | Enrollment tokens, registration PINs | Token: DB hash, shown once. PIN: memory and DB hash while the window is open | Enroll a rogue cell; register a rogue handset into a user's account |
| S10 | SRTP master keys (cell link and SIP side) | Memory only, per call | Decrypt that call's audio |
| S11 | Call content (audio) | Transient, never stored | Eavesdropping |
| S12 | Personal data: IPUI, call log, user names and e-mail | DB in clear | Privacy: who called whom, when, from which cell |
| S13 | Audit log | DB | Integrity of the record of admin actions |

The DCK is derived per call and never stored (`first-tasks.md` §3.4).

## 2. Actors

| Actor | Capability assumed |
|---|---|
| A1 Internet attacker | Reaches the web UI and the cell control port if exposed; no credentials |
| A2 Network attacker on a cell's path (LAN or internet) | Reads, drops, injects and replays packets between cell and controller |
| A3 Radio attacker near a cell | Receives and transmits DECT; can run a fake base station |
| A4 Malicious authenticated user | Valid `user` account; tries to reach other users' handsets, accounts and logs |
| A5 Stolen or compromised cell | Root on one cell, including its private key and anything that passes through it |
| A6 DB dump holder | A copy of the database or a backup, without the master key |
| A7 Host compromise | Root on the controller host; game over by definition, listed to bound the others |
| A8 Malicious or compromised SIP server | Sees signalling for the accounts registered to it |
| A9 Operator of a visited cell | Runs a cell other users' handsets attach to; has root on that box by design (it is theirs). Honest-but-curious or malicious. Unlike A5, needs no compromise: it is the normal case of a shared network |

## 3. Trust boundaries

| Boundary | Attacker | Mitigation | Residual risk |
|---|---|---|---|
| **B1 Handset ↔ cell (radio)** | A3 | DECT authentication with the UAK; call ciphering once proven on the chip (`0x0208`, openDECTcell `docs/call-g7.md`). Registration only inside a short window (default 120 s) opened by the user **on one cell they pick** (`adr/0005-registration-flow.md`), with a random **8-digit** PIN, single use, rate-limited, that the controller uses as the authentication code; the new handset appears in the user's list for confirmation | **Calls may be unciphered on the air** until chip-side ciphering is proven; DSC (the original DECT cipher) has published practical attacks, so a DSC-ciphered call is not strong protection either. A radio attacker who learns the PIN inside the window can register their own handset into that user's account |
| **B2 Cell ↔ controller** | A2, A5 | mTLS 1.3 on every link including the LAN (`cell-protocol.md` §4); per-cell certificates from the controller's private CA; pinned CA on the cell; no shared secrets; immediate revocation by fingerprint; SRTP on media by default | **The cell sees everything it tunnels**, because the chip needs it: the C-plane, each subscription write including the UAK in clear, and decrypted audio. A compromised cell (A5) therefore learns the UAK of every handset written to it and every call it carries. Limited by writing records only to the cells that need them (home set, `cell-protocol.md` §7.4), by the cell keeping no store (`cell-protocol.md` §7.4, openDECTcell#80), and by revocation. Note for reviewers: without TLS, a network attacker (A2) would see the C-plane and the UAKs in clear, which is why TLS is mandatory even on the LAN |
| **B3 Controller ↔ SIP server** | A2, A8 | SIP over TLS with certificate verification by default; SRTP (SDES) only together with TLS, since the keys travel in the SDP; per-account encryption policy (`voip-requirements.md`) | Accounts configured for UDP/TCP or plain RTP are readable on the path. A8 sees its own accounts' calls, as any SIP server does |
| **B4 Browser ↔ controller** | A1, A4 | TLS (terminated in front of or by the controller); Argon2id for local passwords; OIDC with state, nonce and PKCE; HttpOnly, Secure, SameSite=Lax session cookies; CSRF protection on every state-changing request (`http.CrossOriginProtection`); login rate limiting; **server-side role and ownership checks on every action** (`AGENTS.md` §5); `html/template` escaping | Phishing of IdP credentials is outside the controller's control |
| **B5 Controller ↔ database** | A6, A2 | Secrets encrypted at rest (§4); TLS to PostgreSQL when not on a private Docker network; least-privilege DB role | A6 gets S12 and S13 in clear, plus the hashes in S6, S7 and S9 |
| **B6 Controller ↔ IdP** | A2 | TLS with verification; ID token signature, issuer, audience, nonce and expiry checked | A compromised IdP can log in as anyone it can name; group→role mapping limits what it can grant only to what the mapping allows |
| **B7 Logs and support output** | Anyone who reads logs | Logs never contain S1–S10, PINs, IPUIs or mail parameters of the key- and identity-bearing primitives (`cell-protocol.md` §7.3) | — |
| **B8 Handset ↔ a visited cell's operator** (shared network) | A9 | None that DECT allows: **the base must hold the handset's key**, so whichever cell serves a handset holds its UAK. Limits, proposed (`adr/0004-shared-network.md`): records only on cells that need them; an owner-side setting "my handset may be placed on: my cells / operators I trust / any cell"; the cell keeps no store; an operator's UI shows no foreign owners | **Inherent, not a bug.** The operator of every cell a handset was placed on can: (1) **read its UAK** from the chip (root on the box; the slot write also passes through the cell process in clear, B2), and with it impersonate the handset on **any** cell of the network, so **place calls on its owner's SIP lines** (toll fraud), and decrypt recorded air traffic of that handset; (2) **hear its calls in plaintext**: audio is clear at the chip and on the TDM lane before SRTP; (3) see the C-plane: dialled digits, caller IDs, the names in its Internal Names List, call times, the IPUI. The only remedy for a leaked UAK is re-registering the handset. The Things Network comparison breaks here: LoRaWAN's gateways never hold application keys, DECT's base does. **For the maintainer to decide whether this is acceptable for the intended community** (the maintainers' notes point 4) |

## 4. Secret handling

### 4.1 Master key

- 32 random bytes, base64-encoded, supplied as `OPENDECTCELL_MASTER_KEY` or, better,
  as a file (`OPENDECTCELL_MASTER_KEY_FILE`, e.g. a Docker secret). Never in the
  repository, never in the database, never logged.
- The controller refuses to start without it, and refuses to start if it cannot
  unwrap the active data key with it (wrong key).
- **Generate:** `opendectcell-controller keygen` prints a new key.
- **Back it up separately from the database.** Losing it means every
  UAK has to be replaced, i.e. every handset re-registered, and every SIP
  password re-entered.

### 4.2 Envelope encryption

| Layer | Key | Stored |
|---|---|---|
| KEK | Master key (§4.1) | Outside the DB |
| DEK | Random 256-bit data key, one active at a time | `data_key` table, wrapped (AES-256-GCM) by the KEK, with the KEK's id |
| Secret | AES-256-GCM under a DEK, 96-bit random nonce | In the owning row, as `dek_id ‖ nonce ‖ ciphertext ‖ tag` |

- **Associated data binds each ciphertext to its place**: table, column and the
  row's primary key. A ciphertext copied into another row or column fails to
  decrypt, so a DB writer cannot swap one handset's UAK into another's row.
- Encrypted columns: `handset.uak`, `sip_account.password`, `cell_ca.key`, and
  any future secret column. The schema names them `*_enc` (`docs/schema.md`).
- Random nonces are safe here: the number of encryptions per DEK stays many
  orders of magnitude below the 2³² limit for random 96-bit nonces.

### 4.3 Rotation

| What | How | Cost |
|---|---|---|
| Master key | Start with the new key as `OPENDECTCELL_MASTER_KEY` and the old one in `OPENDECTCELL_MASTER_KEY_OLD`; `opendectcell-controller rekey` re-wraps every DEK; then remove the old key | One row per DEK, instant |
| Data key | `opendectcell-controller rotate-dek`: a new DEK becomes active for new writes, and existing secrets are re-encrypted in batches; the old DEK is deleted when nothing references it | One row per secret |
| UAK | Only by re-registering the handset | User action |
| Cell certificate | Renewed over the link before expiry (`cell-protocol.md` §5.4) | Automatic |
| Cell CA | `cell-protocol.md` §5.6 | Admin action |

### 4.4 What an attacker gets

| Attacker has | Gets |
|---|---|
| DB dump only (A6) | IPUIs, TPUIs, call log, users, audit log in clear; password, session and token hashes; **no** UAK, SIP password or CA key |
| DB dump + master key | Everything in the DB, including all UAKs and SIP passwords |
| Master key only | Nothing by itself |
| One cell (A5) | The UAKs of handsets written to that cell, the C-plane and audio of calls through it, until it is revoked; nothing about handsets that never had a record there |
| A visited cell's operator (A9) | The same as A5, for every foreign handset ever placed on their cell, **without any compromise and without revocation ending it**: a UAK read once stays valid until the handset re-registers |
| Host root (A7) | Everything, including keys in memory |

## 5. Other rules

- **Passwords:** Argon2id, memory 64 MiB, 3 passes, parallelism 2 (tunable in
  config); a dummy hash is computed for unknown users, so response times don't
  reveal which names exist.
- **Tokens and PINs** (session, enrollment, API) are random with ≥ 128 bits of
  entropy (PINs excepted: 8 digits, but valid only inside one 120 s window,
  on one cell and for one registration, with at most 10 windows per user per
  hour and attempts limited by the DECT procedure itself) and stored as
  SHA-256 hashes; the clear value is shown once.
- **Audit:** every state-changing action is recorded with actor, action, object
  and time, and never with secret values (`docs/schema.md`, `audit_log`).
- **Container:** runs as a non-root user with a read-only root filesystem; the
  only writable path is a temporary directory.
- **Dependencies:** licence-checked (`AGENTS.md` §3), pinned in `go.sum`, and
  scanned with `govulncheck` in CI.

## 6. Open points

- Call ciphering on the chip (`0x0208`) is unproven, and so B1's main
  mitigation for call content is not yet in place. Tracked on the openDECTcell side.
- PIN length: decided 2026-10-25, 8 digits by default (the maintainers' notes);
  6 or 4 remain an admin option for a handset that will not take 8.
- **The shared network's trust model (B8)**: whether it is acceptable that
  a visited cell's operator can read a handset's key and hears its calls,
  and whether placement on foreign cells is opt-in per handset. **Open for
  the maintainer.**
