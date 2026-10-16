# Architecture (draft)

## 1. Overview

```
 handsets ))) cell (FRITZ!Box + OpenWrt + opendectcell)          controller (server)                  PBX / provider
              STUB ─PCM─ kmod ─ dect-stubd ══TCP/TLS (mails, mgmt)══ cell manager ─┐
                                          ══UDP/SRTP (audio)═══════ media engine ─┼─ SIP UAs ──SIP/TLS── registrar
                                                                    DECT core      │  (≤2 per handset) ─RTP/SRTP─ media
                                                                    (libopendectcell)  ─┘
                                                                    DB · web UI · admin UI · auth (local/OIDC)
```

- **Cell:** a thin radio unit. It runs the STUB (AVM chip firmware), the SPI-TDM kernel module and `dect-stubd`, which tunnels STUB mails and voice to the controller. A cell has no DECT protocol logic of its own (beyond the on-box test tool `dect-probe`).
- **Controller:** runs `libopendectcell` (DLC/NWK) for all handsets and cells, the SIP user agents, the media engine, the database and the web UI.

## 2. Components

| Component | Responsibility |
|---|---|
| **Cell manager** | Cell enrollment, mTLS sessions, heartbeat and status, cell configuration (RFPI/RPN, enabled), firmware/STUB version reporting |
| **DECT core** | `libopendectcell` instances: DLC links, LCE/MM/CC per handset, the registration window (PIN), authentication and ciphering, idle-roaming bookkeeping (which cell last saw the handset) |
| **Feature framework** | Optional DECT features per handset model (see `dect-features.md`) |
| **SIP stack** | One UA per handset line (≤ 2 per handset): registration, calls, MWI subscription, encryption (see `voip-requirements.md`) |
| **Media engine** | Audio bridging cell ↔ RTP: jitter buffer, transcoding (linear PCM ↔ G.722/G.711), SRTP, statistics |
| **Database** | Users, handsets, SIP accounts, cells, call log and statistics (secrets encrypted) |
| **Web/API** | Users manage their handsets and accounts; admins see stats, logs and cells and manage policies. REST/JSON API used by the web frontend |
| **Auth** | Local accounts plus OIDC (Authentik and others); roles `admin`, `user`; optional per-user permission "may add cells" |

## 3. Data model (first cut)

| Entity | Key fields |
|---|---|
| `user` | id, name, email, auth source (local/OIDC subject; created on first OIDC login), role (mapped from OIDC groups or set locally), `may_add_cells` |
| `handset` | id, owner (user, or *shared* = admin-managed), IPUI, TPUI, auth key (UAK, **encrypted**), model info (manufacturer/EMC, model, profile/capabilities), display name, default line, last seen cell, registration time |
| `sip_account` | id, owner (user), display name, user, domain, auth user, password (**encrypted**), registrar, outbound proxy, transport, encryption policy, codec list, DTMF mode, NAT options, … (`voip-requirements.md` §2) |
| `handset_line` | handset, slot (1 or 2), sip_account. **0–2 lines per handset**; one account may serve several handsets. Per handset: `dual_ring_policy` (call waiting / busy) for when both lines ring |
| `cell` | id, name, location, RFPI/RPN, certificate fingerprint, enrolled by, status, firmware/STUB version, last heartbeat |
| `cell_enrollment_token` | token hash, created by, expiry, single use |
| `call_log` | handset, line, direction, remote party, start/answer/end, cell(s), codec, encrypted yes/no, quality metrics |
| `feature_setting` | handset, feature id, enabled, parameters (per-model feature framework) |
| `audit_log` | actor, action, object, time |

## 4. Cell ↔ controller protocol

- **Control: TCP**, with TLS 1.3 **mandatory when the cell is outside the trusted LAN** (configurable per cell; default on). Mutual authentication uses per-cell client certificates issued at enrollment. Content:
  - tunnelled STUB mails (opaque, length-prefixed)
  - cell management (hello, version, config, heartbeat, status, statistics)
  - media session setup, including SRTP keys
- **Audio: UDP**, one flow per active call. RTP-like framing (sequence, timestamp), linear PCM 8/16 kHz or a compact codec. Encrypted with **SRTP** (keys exchanged over the TLS control channel) or DTLS. Plain RTP only on trusted LANs, by explicit admin choice.
- **Enrollment:**
  1. An admin (or a permitted user) creates a one-time token in the web UI.
  2. The cell sends the token together with its certificate signing request.
  3. The controller issues a certificate, and an admin approves the cell, unless auto-approve is set.
- **Resilience:** cells reconnect with back-off. Calls on a cell end if the control link is lost for longer than a short grace period. Handsets re-locate when the cell comes back.

## 5. Registration flow (user view)

1. The user selects "Register handset" and picks one or more cells (or all of their allowed cells).
2. The controller opens a registration window (e.g. 120 s) with a one-time PIN shown in the UI.
3. The user starts registration on the handset and enters the PIN.
4. The controller stores the IPUI, keys and the model/capability info. The handset belongs to the user.
5. The user assigns line 1 and optionally line 2 (SIP accounts), chooses the default outgoing line, and enables optional features.

## 6. Non-goals (not a PBX)

Not provided: dial plans, routing, hunt and ring groups, voicemail, IVR, conferencing, call parking, call recording, a SIP registrar or proxy for other devices. Internal calls between handsets go through the SIP server.

## 7. Decisions (2026-10-16)

| Topic | Decision | Reason / notes |
|---|---|---|
| Variant | **Multi-cell only:** all DECT and VoIP logic runs on the controller; cells are thin radio units | A possible later split into `opendectcell-mc` (multi-cell) and `opendectcell-sa` (standalone with VoIP on the box) is **not** on the list now |
| Media path | Always relayed through the controller (cell ↔ controller ↔ PBX) | Direct cell↔PBX media is out of scope |
| Service language | **Go** (`libopendectcell` via cgo) | Mature, permissively licensed SIP/RTP stack (sipgo for SIP; pion for RTP, SRTP and DTLS), simple and fast to compile (efficient for AI-assisted coding), small static binaries, good enough performance. Rust was the alternative (faster, safer, but less mature SIP libraries and slower iteration); Python was rejected (efficiency) |
| Deployment | **Docker container**, **PostgreSQL only** | No SQLite variant |
| Authentication | Local accounts + OIDC (e.g. Authentik); **OIDC groups map to roles**; **users are created on first login** (just-in-time) | |
| Handset ownership | Personal handsets **and shared/admin-owned handsets** (e.g. a reception handset) | `handset.owner` is a user or "shared"; shared handsets are managed by admins |
| Lines | **0, 1 or 2 SIP accounts per handset.** With 0 lines a handset still works for paging/locating | **When both lines ring at the same time, the user chooses the behaviour per handset:** call waiting, or busy for the second call |
| Internal calls | **Only via the SIP server**; no handset-to-handset routing in the controller | No call routing in the controller (not a PBX) |
| Scale | Design target **10 cells / 50 handsets** per installation, but keep scaling easy: stateless web/API, state in PostgreSQL, no hard-coded limits | Multi-site and multi-tenant are not goals for now |
| UI language | **English only** | No frontend framework preference; pick what fits Go best (decision at implementation start) |

## 8. Still open

| Topic | Options | Notes |
|---|---|---|
| Audio format on the cell link | linear PCM vs G.722/G.711 | LAN bandwidth is not an issue; linear keeps cells simple |
| Web frontend technology | server-rendered (Go templates + htmx) vs SPA | Decide at implementation start |
| DECT system identity (ARI) for multi-cell | see openDECTcell ROADMAP M11 research item | Needed before multi-cell handover; single-cell tests use the box's factory RFPI |

All dependencies must be AGPL-3.0 compatible (GPL-2.0-or-later, GPL-3.0, LGPL, MIT, BSD, Apache-2.0); GPL-2.0-only code cannot be linked in.
