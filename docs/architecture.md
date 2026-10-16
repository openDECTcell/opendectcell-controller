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
| `user` | id, name, email, auth source (local/OIDC subject), role, `may_add_cells` |
| `handset` | id, owner (user), IPUI, TPUI, auth key (UAK, **encrypted**), model info (manufacturer/EMC, model, profile/capabilities), display name, default line, last seen cell, registration time |
| `sip_account` | id, owner (user), display name, user, domain, auth user, password (**encrypted**), registrar, outbound proxy, transport, encryption policy, codec list, DTMF mode, NAT options, … (`voip-requirements.md` §2) |
| `handset_line` | handset, slot (1 or 2), sip_account. **Up to 2 lines per handset**; one account may serve several handsets |
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

## 7. Open design decisions

| Topic | Options | Notes |
|---|---|---|
| Controller service language | Go / Rust / Python (calling `libopendectcell` via FFI) | To be decided before implementation. Needs a solid SIP/SRTP library |
| SIP/media library | PJSIP (C) / sofia-sip / language-native stacks | PJSIP covers TLS, SRTP, G.722 and jitter buffer; licence check needed (GPL-2.0 compatible) |
| Database | PostgreSQL / SQLite | SQLite for small installs, PostgreSQL optional |
| Audio format on the cell link | linear PCM vs G.722/G.711 | LAN bandwidth is not an issue; linear keeps cells simple |
