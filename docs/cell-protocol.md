# Cell ↔ controller wire protocol (v1)

Status: **v1, complete for C3 (2026-10-31).** Nothing in the wire format is
left to decide; §15 says what the cell implements today and what the
controller does until the rest lands. History: draft, 2026-10-19 (task A1 in `docs/first-tasks.md`); cell-side
answers C1–C11 folded in 2026-10-22. **Revised 2026-10-29** by the cell
side, against openDECTcell `45b0264`: corrections to §7.4 (the chip's slot
table survives, a clear exists, read-back exists, admission on 07.56),
§7.5, §8.1 and §8.2 (wideband works both ways), the 06.88 mail shape in
§7.1, and a new §14 on how openDECTcell implements the cell. The split is
unchanged and confirmed by the maintainer: the controller runs the DECT
stack, the cell tunnels mails (keys stay off cells, and roaming and
handover stay possible). This is the
**living contract between openDECTcell (cell side, `dect-stubd`) and the
controller**. Change it by commit in this repo, and tell the cell side.

Items that depend on cell behaviour that hasn't been established yet are marked
**[cell?]** and collected in §13. Where a value is a policy choice rather than
a fact, the reason is given next to it. Cell-side facts are checked against
openDECTcell at commit `88d61af` (2026-10-19), or come from its answers on the
issues named (openDECTcell#n), and cite its docs or clean-room answers (`[CR …]`).

## 1. Overview

| Plane | Transport | Security | Carries |
|---|---|---|---|
| Control | One persistent TCP connection per cell, cell → controller | TLS 1.3, mutual authentication (§4) | Tunnelled STUB mails, cell management, media setup incl. SRTP keys |
| Enrollment | Same TCP port, before the cell has a certificate | TLS 1.3, server authentication only, CA pinned from the enrollment string (§5) | Token + CSR → certificate |
| Media | UDP, one flow per active call, cell → controller | SRTP keyed over the control link; plain RTP only by explicit admin choice (§8) | RTP voice |

The cell always initiates every connection and flow, so a cell behind NAT needs
no inbound ports.

Defaults (all configurable on the controller):

| Item | Default |
|---|---|
| Control/enrollment port | TCP 7300 (not IANA-registered) |
| Media port range on the controller | UDP 40000–40999 |

## 2. Latency budget

This section is the main design input. It comes from measured hardware
behaviour (`first-tasks.md` §3.2):

- LAPC on the air retransmits an unacknowledged C-plane frame after **~1 s**.
- The radio link is torn down after **~10 s** without progress.

The controller runs LAPC (via `libopendectcell`), so **every C-plane frame a handset
sends needs a controller round trip before its acknowledgement can reach the
air**. The budget for one round trip *chip → cell → controller → cell → chip*:

| Segment | Target (p99) | Notes |
|---|---:|---|
| Chip → `dect-stubd` → socket | 5 ms | Cell-side processing |
| Network, cell → controller | 50 ms | One way |
| Controller: frame in → DECT core → frame out | 10 ms | Including the session goroutine hop (`libopendectcell-integration.md`) |
| Network, controller → cell | 50 ms | One way |
| Socket → `dect-stubd` → chip | 20 ms | Includes TDM serialisation: the largest mail is 262 wire octets (§9.3), ~16 ms at 16 000 octets/s |
| **Total target** | **≤ 135 ms** | |
| **Hard ceiling** | **≤ 500 ms** | Half the LAPC retransmit timer. That leaves room for one lost TCP segment (Linux minimum RTO 200 ms) and still answers before the handset retransmits |

The air interface adds its own time outside this table. The Cs channel carries
one 5-octet fragment per MAC A-field tail (`dect_lc.h`), and each fragment
arrives as its own `0x0203` mail, so a maximum-size LAPC frame (70 octets) is
14 mails. The tail is multiplexed between logical channels, and Cs (tail type
`C_T`) is eligible only in the **8 odd frames** of the 16-frame multiframe, with
MAC control (`M_T`) taking priority. Cs therefore gets **at most one fragment
per 20 ms**, a ceiling of 250 octets/s per direction (openDECTcell#66, citing
EN 300 175-3 §6.2.2.1.1, tables 6.17/6.18):

| Lc frame | Fragments | Air time, best case |
|---|---:|---:|
| 5-octet RR (our acknowledgement) | 1 | ≤ 20 ms |
| Registration messages seen so far, 22–49 octets | 5–10 | 100–200 ms |
| Maximum, 70 octets | 14 | ≥ 280 ms |

Under MAC activity (bearer setup, handover) these get longer. The
acknowledgement we send back is a single fragment, so the controller's part of
the round trip is the table above plus ≤ 20 ms. If the handset starts its
retransmit timer at its first fragment, a maximum-size frame leaves
1000 − 280 − 20 = 700 ms for the round trip, which the 500 ms ceiling still
fits. For reference, the stock base acknowledges a CC-SETUP in about 150 ms.

`libopendectcell`'s LAPC retransmit default is 1000 ms (`dect_lapc.c`), which
matches the ~1 s measured on the handset. EN 300 175-4 §A.1 specifies
`<DL.04>` as **2 s**; openDECTcell records the 1 s as non-conformant
(`docs/spec-conformance.md`, Gap 3) and plans to move to 2 s, which would
double the margin. This spec keeps 1 s as the design basis until that lands.

Consequences, all normative:

1. One persistent, framed, bidirectional connection. No request/response per
   message and no HTTP.
2. `TCP_NODELAY` on both ends (no Nagle). Go's default already sets it; the
   cell must set it explicitly.
3. Each frame, or a batch of frames that are ready together, is written with
   a single TLS write and flushed immediately. Nothing waits to fill a record.
4. Strict priority scheduling on send, with a small kernel send buffer (§9),
   so a C-plane mail never queues behind bulk data.
5. The controller measures RTT continuously (§6.3). A cell whose p95 RTT
   exceeds **150 ms** over one minute is marked *degraded* in the admin UI.
   Above **400 ms** it is marked *unfit for calls*, and new calls on it are
   refused with SIP 503. This is policy, chosen to keep a margin below the
   500 ms ceiling.

## 3. Framing

All integers are **unsigned big-endian** unless stated. Strings are UTF-8, not
NUL-terminated.

### 3.1 Frame

```
offset  size  field
0       2     length   bytes after this field = 4 + body length (4..4100)
2       2     type     message type (§3.3)
4       2     corr     correlation id: 0 = unsolicited; a response echoes the request's corr
6       n     body     0..4096 bytes, layout per type
```

- **MAX_BODY = 4096** bytes, fixed in v1 (no negotiation). Each type has a
  tighter bound of its own (§3.3).
- `corr` is chosen by the requester, unique among its own outstanding
  requests, and never 0 for a request that expects a response.

### 3.2 Body encodings

Two encodings, chosen per type:

- **Fixed:** a fixed layout, given in the message's section. Used for the hot
  path (mails, heartbeats), where parsing must be trivial.
- **TLV:** a sequence of `tag u8 | len u16 | value[len]`. Tags are per message
  type. **Unknown tags are ignored**, which is how optional fields are added
  without a version bump. A required tag that is missing → `ERROR MALFORMED`. A
  tag repeated where the spec doesn't allow repetition → the last value wins.

### 3.3 Message type registry

Class is the send priority (§9): **P0** urgent, **P1** management, **P2** bulk.

| Type | Name | Direction | Class | Body | Max body | § |
|---|---|---|---|---|---:|---|
| **0x00xx link** | | | | | | |
| 0x0001 | HELLO | cell → ctl | P1 | TLV | 1024 | 6.1 |
| 0x0002 | HELLO_ACK | ctl → cell | P1 | TLV | 512 | 6.1 |
| 0x0003 | HEARTBEAT | both | P0 | fixed | 4 | 6.3 |
| 0x0004 | HEARTBEAT_ECHO | both | P0 | fixed | 4 | 6.3 |
| 0x0005 | GOODBYE | both | P1 | TLV | 256 | 11 |
| 0x0006 | ERROR | both | P1 | TLV | 256 | 11 |
| 0x0007 | FLOW | cell → ctl | P0 | fixed | 3 | 9.3 |
| **0x001x enrollment** (unauthenticated connections only) | | | | | | |
| 0x0010 | ENROLL_REQUEST | cell → ctl | — | TLV | 2048 | 5 |
| 0x0011 | ENROLL_RESPONSE | ctl → cell | — | TLV | 4096 | 5 |
| **0x01xx STUB tunnel** | | | | | | |
| 0x0100 | MAIL_UP | cell → ctl | P0 | fixed | 258 | 7 |
| 0x0101 | MAIL_DOWN | ctl → cell | P0 | fixed | 254 | 7 |
| 0x0102 | MAIL_REJECT | cell → ctl | P0 | fixed | 3 | 7 |
| **0x02xx cell management** | | | | | | |
| 0x0200 | CELL_STATE | ctl → cell | P1 | TLV | 256 | 6.2 |
| 0x0201 | CONFIG_SET | ctl → cell | P1 | TLV | 512 | 10 |
| 0x0202 | CONFIG_ACK | cell → ctl | P1 | TLV | 64 | 10 |
| 0x0203 | STATUS | cell → ctl | P1 | TLV | 512 | 10 |
| 0x0204 | STATUS_REQUEST | ctl → cell | P1 | empty | 0 | 10 |
| 0x0205 | STATS_REQUEST | ctl → cell | P1 | empty | 0 | 10 |
| 0x0206 | STATS | cell → ctl | P2 | TLV | 2048 | 10 |
| 0x0207 | STATE_SNAPSHOT | cell → ctl | P1 | TLV | 1024 | 6.4 |
| 0x0210 | RENEW_REQUEST | cell → ctl | P1 | TLV | 2048 | 5.4 |
| 0x0211 | RENEW_RESPONSE | ctl → cell | P1 | TLV | 4096 | 5.4 |
| 0x0212 | TRUST_UPDATE | ctl → cell | P1 | TLV | 4096 | 5.6 |
| 0x0213 | TRUST_ACK | cell → ctl | P1 | TLV | 64 | 5.6 |
| **0x03xx media** | | | | | | |
| 0x0300 | MEDIA_OPEN | ctl → cell | P0 | TLV | 512 | 8 |
| 0x0301 | MEDIA_OPEN_ACK | cell → ctl | P0 | TLV | 64 | 8 |
| 0x0302 | MEDIA_CLOSE | ctl → cell | P0 | TLV | 64 | 8 |
| 0x0303 | MEDIA_CLOSED | cell → ctl | P0 | TLV | 64 | 8 |
| **Reserved** | | | | | | |
| 0x0400–0x04FF | log shipping | — | P2 | | | 12 |
| 0x0500–0x05FF | software / firmware update | — | P2 | | | 12 |
| 0x0F00–0x0FFF | debug / experimental; only when both sides set the DEBUG capability | — | — | | | 12 |
| 0x8000–0xFFFF | reserved for a future major version | — | — | | | |

### 3.4 Handling invalid input

| Condition | Action |
|---|---|
| `length` < 4 or > 4100 | Framing is lost: send `GOODBYE PROTOCOL` (best effort) and close |
| Known type, body larger than its bound, or a fixed layout that doesn't match | `ERROR MALFORMED` or `ERROR TOO_LARGE`, discard the frame, keep the link |
| Unknown type, link established | `ERROR UNKNOWN_TYPE` with `ref_type`/`ref_corr`, discard, **keep the link** |
| Any type other than HELLO / ENROLL_REQUEST before the handshake completes | Close without reply |
| A type whose capability was not negotiated (§6.1) | `ERROR UNSUPPORTED`, discard |
| A type not allowed in the current cell state (e.g. MAIL_DOWN while *pending*) | `ERROR NOT_ALLOWED`, discard |

Senders send only types that are in the peer's announced capabilities, so
`UNKNOWN_TYPE` indicates a bug or a version skew, and is logged as a warning.

## 4. Transport security (mTLS)

- **TLS 1.3 only.** No TLS 1.2 fallback, no 0-RTT (replay risk). Session
  resumption (PSK) is allowed to speed up reconnects.
- Cipher suites: the TLS 1.3 defaults (AES-128-GCM, AES-256-GCM,
  ChaCha20-Poly1305).
- **Keys: ECDSA P-256** on both sides. Ed25519 is left for later: mbedTLS's
  Ed25519 support is weaker than its P-256 support.
- **Cell TLS library: mbedTLS** (3.6.6 on the lab FRITZ!Box 7520, OpenWrt
  25.12.5). It is already installed as OpenWrt's default stack (pulled in by
  `ustream-ssl` and `wpad`), and no OpenSSL or wolfSSL is present, so any other
  choice ships a second TLS library on a flash-limited device (openDECTcell#62).
  TLS 1.3 with an ECDSA P-256 **client** certificate is compiled into the
  target build. That is shown by binary evidence, not yet by a completed
  handshake: the 1.3 key-schedule labels, the `TLS 1.3, client
  CertificateVerify` context, `secp256r1` and the ECDSA and cert-config
  functions are all present (openDECTcell#75). A real mutual-auth handshake is on
  offer from openDECTcell and should be run before C2 implements the link. The cell
  pins the group (`mbedtls_ssl_conf_groups`) and signature algorithm
  (`mbedtls_ssl_conf_sig_algs`) explicitly rather than relying on defaults.
- The cell keeps TLS work off the process that services the chip's TDM
  control channel, so a handshake cannot starve the mail channel: TLS runs
  in its own process, `dect-cell` (§14). The 7412 runs mbedTLS 3.6.7
  (OpenWrt 25.12.6).
- **Mandatory on every control link, including the trusted LAN.** This narrows
  `architecture.md` §4, which made TLS configurable per cell; the reason is in
  the work log. Briefly: `AGENTS.md` §5 requires mTLS for cell authentication,
  and the link carries handset keys in clear inside subscription writes (§7.3).

### 4.1 The cell CA

- The controller runs its own private **cell CA**, used only for this link.
  It is not a public CA, and it is separate from the web UI's certificate.
- The CA private key is stored encrypted with the controller's master key
  (`docs/threat-model.md`, A5).
- The CA issues:

| Certificate | SAN | EKU | Validity |
|---|---|---|---|
| Controller control-link server cert | URI `urn:opendectcell:controller:<installation-uuid>`, plus DNS names as configured | serverAuth | 1 year, auto-renewed by the controller |
| Per-cell client cert | URI `urn:opendectcell:cell:<cell-uuid>` | clientAuth | 90 days, renewed over the link (§5.4) |

### 4.2 Verification

**Cell verifies the controller:** the chain must end in a CA whose SPKI
SHA-256 matches one of the cell's pinned CA hashes (from enrollment, or added
by TRUST_UPDATE), and the leaf must carry the controller URI SAN. Hostnames are
not checked; the pin replaces them, so cells can reach the controller by IP or
through port forwarding. **The pin check must also run on resumed TLS
sessions**: a verification callback that only runs on full handshakes is
bypassed by resumption. In mbedTLS, either disable session tickets on the cell
or keep the peer certificate (`MBEDTLS_SSL_KEEP_PEER_CERTIFICATE`) and check
it after every handshake. The reference cell in `internal/cell/celltest` uses
Go's `VerifyConnection`, which runs on both.

**Controller verifies a cell**, in this order:

1. The chain ends in the cell CA.
2. The leaf's SAN URI names a cell UUID that exists in the database.
3. The leaf's SHA-256 fingerprint is in that cell's list of current
   certificates (old and new are both current during a renewal).
4. The cell's state is not *revoked*.

Steps 3 and 4 make revocation immediate without CRLs or OCSP: the controller is
the only relying party.

**Clocks:** OpenWrt devices often have no RTC. A cell **must** sync time (NTP)
before connecting. If its clock is still earlier than its own build time, it
may skip the validity-period check on the controller certificate, but never the
CA pin. HELLO_ACK carries the controller's time so the cell can detect skew.

## 5. Enrollment

### 5.1 Token and enrollment string

1. An admin, or a user with `may_add_cells`, creates a token in the web UI:
   20 random bytes, single use, expiring after 24 h by default (maximum 7
   days). Optional settings: pre-assigned cell name/location, and
   *auto-approve*.
2. The database stores only the token's SHA-256 (`cell_enrollment_token`).
3. The UI shows the token **once**, as an enrollment string:

```
opendectcell-enroll:v1?h=<host>:<port>&t=<token, base32>&ca=<SHA-256 of the cell CA SPKI, base64url>
```

The admin pastes it into the cell (LuCI page or CLI). The `ca` hash is what
lets the cell authenticate the controller on first contact.

### 5.2 Exchange

1. The cell generates a P-256 key pair locally (the private key never leaves
   the cell) and a CSR. The CSR subject is ignored.
2. It connects to `h` **without a client certificate**, verifies the
   controller against the `ca` pin (§4.2), and sends `ENROLL_REQUEST`.
3. The controller checks the token: its hash exists, it is unused and unexpired.
   It then marks the token used **in the same transaction** that creates the
   `cell` row (state *pending*, or *active* with auto-approve). It issues the
   certificate and replies `ENROLL_RESPONSE`.
4. Both sides close. The cell reconnects with its certificate (§6).

A connection without a client certificate may send only `ENROLL_REQUEST`.
Anything else, or a second request, closes it. After 5 rejected tokens from one
source address within 10 minutes, the controller delays further responses from
that address by 30 s. Every attempt, successful or not, goes in the audit log
with the source address, never the token.

**ENROLL_REQUEST** (TLV)

| Tag | Name | Type | Req | Notes |
|---:|---|---|---|---|
| 1 | token | 20 bytes | yes | Raw, decoded from base32 |
| 2 | csr | DER, ≤ 1024 | yes | P-256 key, signature must verify |
| 3 | hw_model | string ≤ 64 | no | e.g. `FRITZ!Box 7520` |
| 4 | sw_version | string ≤ 64 | yes | openDECTcell / `dect-stubd` version |
| 5 | name_hint | string ≤ 64 | no | Hostname; used if the token has no name |

**ENROLL_RESPONSE** (TLV)

| Tag | Name | Type | Req | Notes |
|---:|---|---|---|---|
| 1 | status | u16 | yes | 0 ok, 1 token rejected, 2 bad CSR, 3 rate limited, 4 internal error |
| 2 | cell_id | 16 bytes | if ok | UUID |
| 3 | cert | DER | if ok | Cell client certificate |
| 4 | ca_cert | DER | if ok | Cell CA certificate, for the cell's trust store (its hash must equal the pin) |
| 5 | cell_state | u8 | if ok | §6.2 |

"Token rejected" covers unknown, used and expired tokens on purpose, so a
prober learns nothing about which tokens exist.

### 5.3 Approval

A *pending* cell may connect, send HELLO, heartbeats and STATUS, and receive
CELL_STATE. Everything that touches the radio (MAIL_*, MEDIA_*) is refused
with `NOT_ALLOWED`. When an admin approves it, the controller sends
`CELL_STATE active` and starts configuring the chip by tunnelled mails.

### 5.4 Renewal

- At two-thirds of its certificate's lifetime (day 60 of 90), the cell
  generates a **new** key and sends `RENEW_REQUEST` over its authenticated link.
- The controller issues a certificate and adds its fingerprint to the cell's
  current set. It answers `RENEW_RESPONSE`, then drops the old fingerprint when
  the old certificate expires, or as soon as the cell has connected with the
  new one.
- The cell retries every hour until it succeeds. The controller warns admins
  14 days before a cell's certificate expires.
- **An expired certificate cannot be renewed.** The cell must be re-enrolled
  with a new token. This is deliberate: it keeps a stolen and forgotten cell
  from coming back by itself.

RENEW_REQUEST: tag 1 `csr` (DER ≤ 1024, required). RENEW_RESPONSE: tag 1
`status` u16 (0 ok, 2 bad CSR, 4 internal), tag 2 `cert` DER.

### 5.5 Revocation

An admin revokes a cell in the UI. The controller sets the state to *revoked*,
clears its certificate set, sends `GOODBYE REVOKED` and closes the connection.
Further handshakes fail at step 3 of §4.2. The cell's subscription slots are
freed in the database (§7.4). The cell erases its key and enrollment data when
it receives `GOODBYE REVOKED`, but the controller does not rely on it.

### 5.6 CA rotation

The controller generates a new cell CA and sends `TRUST_UPDATE` (tag 1:
`ca_cert` DER, repeatable) to every connected cell; each cell adds the new pin
and answers `TRUST_ACK` (tag 1: status u16). Once all active cells have
acknowledged, or after a configurable overlap period, the controller switches
its server certificate to the new CA and reissues cell certificates through the
normal renewal path. Cells that were offline throughout must be re-enrolled.

## 6. Session lifecycle

### 6.1 Handshake

```
cell                                  controller
 | TCP connect, TLS 1.3 + client cert  |
 |------------------------------------>|  §4.2 checks
 | HELLO                               |  must arrive within 5 s, or close
 |------------------------------------>|
 |                          HELLO_ACK  |
 |<------------------------------------|
 | STATE_SNAPSHOT  (only if resumed)   |
 |------------------------------------>|
 |                 CELL_STATE, mails…  |
 |<------------------------------------|
```

**HELLO** (TLV)

| Tag | Name | Type | Req | Notes |
|---:|---|---|---|---|
| 1 | proto_version | u8 major, u8 minor | yes | This document is 1.0 |
| 2 | sw_version | string ≤ 64 | yes | |
| 3 | stub_version | string ≤ 64 | yes | Chip firmware build, e.g. `07.56`, `06.88`. The controller needs it: the builds differ in the `0x0203` shape (§7.1), in admission (§7.4) and in memory addresses |
| 4 | hw_model | string ≤ 64 | no | |
| 5 | boot_id | 16 bytes | yes | Random, new on every host boot |
| 6 | chip_epoch | u32 | yes | Incremented each time the chip is (re)started since boot. With `boot_id` it tells the controller whether the chip's state survived (§7.4) |
| 7 | capabilities | u32 bitmask | yes | Table below |
| 8 | cap_sub_records | u8 | yes | Subscription records the chip holds (6 on current firmware) |
| 9 | cap_radio_contexts | u8 | yes | Concurrent radio connection contexts (6) |
| 10 | cap_audio_nb | u8 | yes | Host audio endpoints usable for narrowband (4 on the stock 7520 profile) |
| 11 | cap_audio_wb | u8 | yes | Same, for wideband; may differ from narrowband |
| 12 | audio_formats | list of u8 (≤ 16) | if MEDIA | Formats the cell can accept (downlink), preferred first, §8.2 |
| 13 | resume_token | 16 bytes | no | From the previous HELLO_ACK, §6.4 |
| 14 | radio_state | u8 | yes | 0 off, 1 on, 2 fault |
| 15 | cell_time | u64 | no | Unix ms, informative |
| 16 | audio_formats_up | list of u8 (≤ 16) | no | Formats the cell can send; absent = same as tag 12. §8.2 **[proposed]** |

The cell **announces** its capacity rather than the controller assuming it
(`first-tasks.md` §3.4): the independent OpenWrt driver may expose different
numbers from the stock firmware.

**Capability bits**

| Bit | Name | Meaning |
|---:|---|---|
| 0 | MAIL | STUB mail tunnel (required in v1) |
| 1 | MEDIA | Media sessions |
| 2 | SRTP | SRTP on media |
| 3 | PLAIN_RTP | Unencrypted RTP, if the admin allows it |
| 4 | STATS | STATS messages |
| 5 | FLOW | FLOW messages |
| 6 | RESUME | Resume and STATE_SNAPSHOT |
| 7 | LOG | Reserved (§12) |
| 8 | UPDATE | Reserved (§12) |
| 9 | DEBUG | Reserved (§12) |

**HELLO_ACK** (TLV)

| Tag | Name | Type | Req | Notes |
|---:|---|---|---|---|
| 1 | proto_version | u8, u8 | yes | Version the controller will speak |
| 2 | cell_id | 16 bytes | yes | |
| 3 | cell_state | u8 | yes | §6.2 |
| 4 | session_token | 16 bytes | yes | Random; the cell presents it as `resume_token` on its next connect |
| 5 | resumed | u8 | yes | 1 if this connection resumed the previous session |
| 6 | heartbeat_ms | u16 | yes | Default 1000 |
| 7 | dead_ms | u16 | yes | Default 3000 |
| 8 | grace_ms | u16 | yes | Default 7000 |
| 9 | orphan_s | u16 | yes | Default 60 |
| 10 | capabilities | u32 | yes | The subset of the cell's bits the controller will use |
| 11 | controller_time | u64 | yes | Unix ms |

**Versioning:** a different `major` → the controller sends `GOODBYE VERSION`
and closes. A different `minor` is fine; new minors only add types, tags and
capability bits, each of which is negotiated or ignorable.

**Duplicate connections:** if a cell connects while an older connection from
it is still open (for example, half-open after a NAT reset), the new one wins.
The controller sends `GOODBYE SUPERSEDED` on the old one, closes it, and treats
the new one as a resume attempt.

### 6.2 Cell states

| Value | State | Radio traffic allowed | Set by |
|---:|---|---|---|
| 0 | pending | no | enrollment without auto-approve |
| 1 | active | yes | admin approval, or auto-approve |
| 2 | disabled | no; the controller turns the beacon off | admin |
| 3 | revoked | connection refused | admin (§5.5) |

CELL_STATE (TLV): tag 1 `state` u8 (required), tag 2 `reason` string ≤ 128.

### 6.3 Heartbeat and liveness

- **Any received frame counts as proof of life.** A side sends HEARTBEAT only
  if it has sent nothing for `heartbeat_ms`.
- HEARTBEAT body: `t u32`, the sender's monotonic clock in ms. The receiver
  answers immediately with HEARTBEAT_ECHO carrying the same `t`. The sender
  computes the RTT. Both sides do this, so both measure RTT; the cell reports
  its figures in STATS.
- **Link dead:** nothing received for `dead_ms` (3 s). The side that notices
  closes the TCP connection.

Bandwidth at defaults: ~14 bytes of frame plus TLS overhead per second per
direction, which is negligible next to the ~16 kB/s signalling ceiling.

### 6.4 Link loss, grace period and resume

Timeline, measured from the **last frame received** from the peer:

| t | Controller | Cell |
|---:|---|---|
| 0 | | |
| 3 s (`dead_ms`) | Closes the link; keeps sessions and calls | Closes the link; reconnects immediately (back-off below); keeps radio connections and audio |
| ≤ 7 s (`grace_ms`) | If the cell resumes: reconcile (below), carry on | Same |
| 7 s | Ends the cell's calls (SIP BYE, or 503 for calls still being set up) and discards its sessions | Releases every radio connection and its audio locally: openDECTcell sends `0x0233 {link, 01}` (audio disconnect) then `0x0202 {link}` [CR A-0127], the release dect-stubd uses on every link since 2026-10-28 (this row named `0x017A {handle, reason}` before 2026-10-29) |
| 60 s (`orphan_s`) | Marks the cell *offline* in the UI and the subscription table *unknown* (§7.4) | **Parks the radio**: `0x022C {01}` (close registration), then `0x015B {00}` (radio and beacon off), so handsets look for another cell rather than camping on one that can't make calls |

**The cell parks; it does not reset the chip** (openDECTcell#61, a recommendation
rather than a measurement). Parking is two mails, is reversed by two more
(`0x015B {01}`, then `0x022B {41}` if registration should open), and is the
sequence openDECTcell runs after every test. A reset would also force reloading the
STUB over the UART. Either way the chip's subscription table is lost, so after
the controller returns and turns the radio back on it rewrites every slot
(§7.4). The cell releases calls and audio **before** parking: parking with live
calls has never been tested. If a release fails, the cell parks anyway: a
parked radio with a stale chip-side call is recoverable, a beaconing cell
without a controller is not. **The cell never unparks on its own**; only the
controller turns the radio back on, after it has rewritten the slots (§7.4).
openDECTcell has committed to this behaviour for M9 (openDECTcell#80). A cell may park sooner than `orphan_s` if handsets
should roam sooner; the two thresholds differ only for user experience.

**Why 7 s:** the radio link drops after ~10 s without progress, and that clock
may have started up to one RTT before our last received frame. 7 s leaves
about 3 s for TCP + TLS reconnect, STATE_SNAPSHOT, and one LAPC
retransmission round to complete before the air link gives up. A longer grace
period would only keep calls that the radio has already lost. This answers
question 1 in `first-tasks.md` §6 with a proposal: **7 s, not 10 s**. The
maintainers should confirm it.

**Reconnect back-off (cell):** first attempt immediately, then 250 ms, 500 ms,
1 s, 2 s, 4 s, …, capped at **30 s**, each with ±20 % jitter. The attempt
counter resets after a connection has been up for 60 s. With the cap at 30 s,
a cell is back within half a minute of the controller returning.

**Resume:** the cell sends its `resume_token`. The controller accepts it if it
matches the cell's latest `session_token` and the grace period hasn't expired.
It answers `resumed = 1`, and the cell immediately sends **STATE_SNAPSHOT**.
Mails in flight during the outage are lost. That is acceptable for C-plane
signalling (`0x0203`), because LAPC retransmits. For other state, the snapshot
is reconciled:

STATE_SNAPSHOT (TLV)

| Tag | Name | Type | Notes |
|---:|---|---|---|
| 1 | chip_epoch | u32 | If changed, the chip restarted: the resume is treated as a fresh start |
| 2 | radio_state | u8 | |
| 3 | radio_conn | 4 bytes, repeatable | Each active radio connection: `handle u8` (terminal index 0..6) then the 3-byte handset `tag`, as the chip reports them in `0x0203` (`dect_cs.h`) |
| 4 | media_id | u32, repeatable | Each media session still open |

The controller releases everything it knows that is not in the snapshot, and
tells the cell (by mail or MEDIA_CLOSE) to release everything in the snapshot
that it doesn't know.

## 7. STUB mail tunnel

The cell has no DECT logic. It passes STUB mails between the chip and the
controller **verbatim**, without filtering or reordering.

### 7.1 Layout

**MAIL_UP** (cell → controller), fixed:

```
0   4   rx_time  u32, cell monotonic clock in µs (wraps), when the mail left the chip
4   1   src      STUB source task
5   1   dst      STUB destination task
6   2   prim     primitive id, big-endian on this link (the STUB itself is little-endian; the cell converts)
8   1   plen     0..249
9   n   param    plen bytes
```

**MAIL_DOWN** (controller → cell), fixed: the same without `rx_time`
(`src`, `dst`, `prim`, `plen`, `param`).

**The `0x0203` shape depends on the chip firmware build** (openDECTcell
`d917c9b`, 2026-10-29): 07.56 and 08.25 send 13 octets `{handle, 01,
seg[5], 00, 00, tag[3], handle}`, 06.88 (the FRITZ!Box 7412) sends 7
octets `{handle, 01, seg[5]}` with no handset tag. The tunnel carries
either verbatim; `libopendectcell`'s `dect_cs` accepts both
(`DECT_CS_MIN_PARAM`, `DECT_CS_SHORT_PARAM`). On 06.88 the handset tag in
STATE_SNAPSHOT tag 3 is all zero.

The body length must equal `9 + plen` (UP) or `5 + plen` (DOWN), and
`plen ≤ 249` (`DECT_MAIL_PARAM_MAX`); anything else is `MALFORMED`. `rx_time`
lets the controller measure cell-side delay and jitter. It is only meaningful
as a difference between mails from the same cell. The STUB's own framing
(start marker, sequence number, checksum, idle fill) ends at the cell and is
not carried; `dect-stubd` decodes it with `dect_rx_*` and encodes with
`dect_mail_encode()`.

**The C-plane is asymmetric** on the STUB interface. Inbound, each `0x0203`
mail carries one 5-octet Cs fragment. Outbound, the controller sends the whole
Lc frame in one `0x0244` APPEND_CONNECTION_DATA `{handle, n, data[n]}` (at most
205 octets buffered per connection), and the STUB fragments it. It does **not**
follow with `0x0202` TRIGGER_SIGNALLING: on hardware that drops the connection
within ~40 ms (`dect-stubd.c`, `send_cplane()`).

### 7.2 Delivery

- Order is preserved in both directions (one TCP stream, FIFO per class).
- Delivery is **not** guaranteed across a link loss (§6.4).
- If the cell cannot hand a MAIL_DOWN to the chip (chip not running, or its
  queue full), it sends **MAIL_REJECT** (fixed: `ref_corr u16`, `reason u8`:
  1 chip down, 2 queue full, 3 chip refused). The controller must set a nonzero
  `corr` on any MAIL_DOWN whose failure it needs to hear about; for `corr = 0`
  the cell drops the mail silently and counts it in STATS.

### 7.3 Confidentiality and logging

Some mails carry secrets or identities:

| Prim | Content |
|---|---|
| `0x0247` + `0x020A` | Subscription record including the handset's 16-byte key |
| `0x0208` | Call security material |
| `0x0203`, `0x0244` | C-plane in and out: handset identities, authentication challenges and responses |

Neither side **ever** logs the `param` of these primitives. Default logging
of any mail is limited to direction, `prim` and `plen`. Full captures are
possible only through an explicit, time-limited debug mode that an admin
enables, which is audit-logged and redacts the key-bearing primitives anyway.

### 7.4 Subscription-slot allocation across cells

A chip holds `cap_sub_records` subscription records (6 today), and a handset
works only on a cell whose chip holds its record (`first-tasks.md` §3.4). The
controller owns the allocation. This section is the A1 proposal; question 2 of
`first-tasks.md` §6 is still open for the maintainers.

**State, per (cell, slot):** `handset_id` (nullable), `state` ∈ {*empty*,
*writing*, *present*, *unknown*}, `written_at`, `last_seen_at` (the handset
was last active on this cell), `pinned` (admin flag). This becomes a
`cell_slot` table in A3.

**The controller is the source of truth.** The chip's table is treated as a
write-only cache:

- **The chip's table is not trustworthy in either direction** (corrected
  2026-10-29). It **survives** the host: records written in an earlier
  session stayed in the chip across daemon restarts and parks, and a
  handset nobody had a record of kept connecting until its slot was
  cleared (openDECTcell runs `vF426`/`vF427`, 2026-10-23, `clear_foreign_slots()`
  in `dect-stubd.c`). AVM's host reloads its records at start-up
  [CR A-0026], so whether a park ever empties the table is not established.
  So every time the controller turns a cell's radio on (`0x015B 01`), and on
  HELLO with a new `boot_id` or `chip_epoch`, all of that cell's slots
  become *unknown*, and the controller **rewrites every slot it wants and
  clears every other** (op 0, below), in the order openDECTcell runs and has proven on hardware: the radio start-up mails first, then the slot writes, the cell open to handsets throughout (a handset's first message waits on the host's identity gate, not on the chip). Writing the slots before the radio is on has not been tried.
- **The controller is the only writer.** In cell mode `dect-stubd` keeps no
  host-side subscription store and does no reload at start-up (the default in
  cell mode, not a flag), never writes slots on its own, and never sends
  `0x015B {01}` on its own (openDECTcell#80, committed for M9). The UAKs exist only
  in the controller's database, encrypted, never on the cell.
- **A clear is one mail:** `0x0247 {slot, op = 0, 0x00}` [CR A-0065];
  dect-stubd clears every slot it does not own at start this way.
  **On 08.25 a clear leaves the slot's 17 key bytes readable** (measured
  2026-10-31, openDECTcell `docs/more-than-six.md` §4b; Q-0073 asks what it resets), so
  the host's audit may call a cleared slot "held". To give a slot to another
  handset, write the new key over it.
- A write is two MAIL_DOWN, in this order: `0x0247 {slot, op = 1, status}` then
  `0x020A {slot, UAK[16], extra}`, to task `0x02` [CR A-0026]. The `status` and
  `extra` bytes have no known enumeration and are sent as `0x00`, as
  `dect-stubd` does. The exact bytes are `0x0247` len 3 `{slot, 0x01, 0x00}`
  and `0x020A` len 18 `{slot, UAK[0..15], 0x00}` (openDECTcell#59).
- **The chip does not confirm a write**; no answering primitive has ever been
  observed, and writes evidently succeed silently. The controller sends both
  mails with a nonzero `corr` (§7.2), so a cell that cannot deliver them
  answers MAIL_REJECT. The slot is *writing* until the cell has taken both
  mails without a reject, then *present*; on a reject it goes back to
  *unknown* and is retried after 2 s. A handset attaching on that cell is the
  only end-to-end proof that the record works.
- **Read-back exists** (corrected 2026-10-29): the key table is readable
  with a chip memory read at an address that is **specific to the chip
  firmware build** (on 06.88 the table is at `0x067F`; dect-stubd's slot
  audit compares each slot's 16 key octets after a write and logs "UAK
  read-back ... MATCHES"). Because the address is per build, the read-back
  is the cell's job, not the controller's: §13.1 P2 proposes a STATUS tag
  for the audit result. Until then *unknown* is resolved by rewriting.
- **Admission is the host's on 07.56** (openDECTcell `docs/multicell-experiments.md`,
  M1 and M2', 2026-10-25): on chip firmware 07.56 a handset whose key the
  chip does not hold attaches, locates, is paged and rings; the slot table
  does not decide who may open a link, the host's identity gates do. On
  08.25 clearing a slot stopped a foreign handset (`vF427`), and 06.88 is
  untested, so the placement and eviction rules below stay in force for
  every build except 07.56, where the controller may skip them (it knows
  the build from HELLO tag 3). Whether the chip needs the slot's key for
  ciphering is still open; dect-stubd sends the call key with `0x0208` per
  call.

**The TPUI is not derived from the slot.** A TPUI is paired with one IPUI
**within a location area**, and the FT must keep TPUI assignments unique there
(EN 300 175-5, cited in openDECTcell#65). All cells of an installation are one
location area, so that handsets can roam between them. A slot-derived TPUI
(`{0x0, 0x11, slot}`, AVM's single-base scheme [CR A-0023, A-0031] and what
`dect-stubd` does) would therefore collide: handset X in slot 2 on cell A and
handset Y in slot 2 on cell B would share one TPUI, and pages would reach the
wrong handset. Assigning a per-cell TPUI instead makes the handset change
identity at every move. So:

- **The controller assigns each handset one TPUI from an installation-wide
  pool**, unique within the location area, at its first location
  registration, and keeps it on every cell. Its low 16 bits are the link
  signature that signs every Lc frame, so the pool keeps those unique too.
- **The chip slot is a private detail of each cell.** The controller keeps the
  (cell, slot) → handset mapping; a handset may occupy different slot indices
  on different cells. The number of handsets per installation is bounded by
  the TPUI pool, and only the number *resident on one cell* is bounded by its
  6 slots.
- The coupling of TPUI and slot is `dect-stubd`'s choice, not the chip's
  (openDECTcell#65). `libopendectcell`'s MM layer currently builds the TPUI from a
  hard-coded slot 1 (`DECT_MM_REG_SLOT`), so the controller needs the TPUI as
  a parameter; that is change request L10 (openDECTcell#68,
  `libopendectcell-integration.md` §11).
- **What else keys off the TPUI.** The page `0x0300 {page info, lo, hi}`
  carries the TPUI's low 16 bits, so it follows the TPUI, not the slot. The
  link signature is the same 16 bits [CR A-0023, A-0031]. Whether ciphering
  or anything else silently relies on those bits equalling `0x11<slot>` is
  **not established**: no call has ever run with a non-slot-derived TPUI
  (openDECTcell#76). That is the main risk of this design.
- Not yet shown on hardware: that a handset accepts a pool TPUI, and (only
  relevant when an installation migrates from the old scheme) that it accepts
  a *changed* TPUI on re-locate **[cell?]** (openDECTcell#76). The first test, on
  one cell, is one handset with a unique non-slot-derived TPUI; reassignment
  needs two cells.

**Placement: which cells get a handset's record.**

*Changed 2026-10-31 (maintainer, the maintainers' notes point 4; proposed
details in `adr/0004-shared-network.md`):* the controller is a **shared
network**. Any handset may attach to any cell that accepts foreign
handsets, so the home set is no longer the access rule but a **placement
hint** (prefetch), and on-demand placement (rule 3) becomes the main
rule wherever the chip admits a handset it holds no key for. That is
known for 07.56 only; the bench day of 2026-11-01 tests 08.25 and 06.88
(openDECTcell T-B). **Until it has, rules 1, 2 and 4 stay the working
assumption** (step 3). Where the chip does not admit such a handset, a
cell serves only handsets placed there beforehand.

1. **Home set** (now: the prefetch set). The cell the handset registered
   at (`architecture.md` §5, one cell per registration window), plus the
   owner's own cells and cells the handset was last seen on. The record
   goes to every cell in that set.
2. **No-eviction fast path.** If a cell's home-set handsets fit into its
   slots, nothing is ever evicted. Installations with ≤ 6 handsets just put
   every record on every cell.
3. **On demand.** If a handset tries to access a cell that doesn't hold its
   record, and the chip reports that to the host early enough **[cell?]**,
   the controller writes the record there, evicting if necessary. The handset's retry then
   succeeds. The candidate event is `0xF604` HANDSET_ACCESS_EVENT; whether it
   arrives before the chip refuses the connection is the open question in
   openDECTcell's `docs/capacity.md` (option 3, "host-side slot virtualisation").
   It has **never appeared in any openDECTcell capture**, because every handset
   seen so far already had its record on the chip; the access observed is
   connection-oriented (`0x0200` + `0x0203`), a different path (openDECTcell#58).
   Until a bench run with an empty slot table settles it, rule 3 is off and
   placement is home set only (rules 1, 2 and 4). On 07.56 the first
   contact is the host's identity gate (above), which sees the IPUI
   before anything else; the record must then go into the running stack,
   which libopendectcell cannot do today (records go in before start; a
   question to the cell side once T-B has an answer).
4. **Neighbour prefetch** (C9, later): when a handset is seen on cell A, make
   sure its record is on the cells an admin has marked as A's neighbours.

**Eviction, when a cell is full:**

1. Never evict a slot that is *pinned*, or whose handset has an active radio
   connection on that cell.
2. Prefer slots not written in the last 5 minutes (hysteresis, so two cells
   don't ping-pong one handset).
3. Among those, evict a foreign handset (not the cell operator's) before
   one of the operator's own, so a busy public cell never locks its
   operator out (*proposed*, ADR 0004); then the least recent
   `last_seen_at`. On a tie, the oldest `written_at`.
4. No candidate → refuse the placement, log an alarm ("cell X: all slots
   pinned or busy"), and show it in the admin UI.

Eviction overwrites the slot with the new record. A pure delete (handset
deregistered or deleted, cell revoked) clears the slot with op 0 (above) and
records it as *empty*. (Before 2026-10-29 this section overwrote the slot
with a random key instead, because no clear was known.)

**Policy switch:** `slot_policy = auto | manual`, per installation. *Manual*
means placement only by admin assignment and never evicts; a full cell refuses.
This is the alternative raised in question 2.

**Rate:** a write is two small mails, 16 + 30 = 46 wire octets (§9.3). The controller limits slot
writes to 4 per second per cell, so a full repopulate after a radio-on takes
under 2 s and never competes noticeably with call signalling.

### 7.5 Call capacity

The controller admits at most `min(cap_audio_nb or cap_audio_wb, max_calls)`
concurrent calls on a cell, and answers further calls with SIP 486/503.
The chip has **three voice channels**, narrowband or wideband [CR A-0124];
two concurrent calls ran on a 7520 on 2026-10-28, three are built but
untested. `max_calls` is an admin setting per cell, **default 2** (it was 1
while no concurrent call had been validated), raised to 3 when a three-call
test passes. Cells report `cap_audio_nb = cap_audio_wb = 3`.

## 8. Media

### 8.1 Session setup

The controller assigns the audio channel on the chip by tunnelled mails. The
sequence `dect-stubd` uses follows AVM's bind order [CR A-0029 §1]: `0x0251`
SET_CHANNEL_PARAM, then `0x0230` ASSIGN_AUDIO_CHANNEL in its full form, answered
by `0x0242`, then the audio path (`0x022D`). Release is `0x0233` plus `0x022D`
disconnect.

**Confirmed by the maintainer, 2026-10-30:** the bind stays with the
controller (through `dect_base`'s `call_up()`/`call_down()` callbacks), not
the cell, unless it proves too time-critical over the tunnel. Nothing
measured says it is: the bind has no air timer and costs one round trip at
the start of a call. If a bench shows otherwise with the RTT inside §2's
budget, the bind moves to the cell, and MEDIA_OPEN then has to carry the
connection handle and the bind flags.

**Reading the assigned channel** (openDECTcell#60, observed on every call): use the
**long, 4-octet form** of `0x0242` `{handle, chan_flags, mask0, mask1}`, which
answers `0x0230`; the short form reports later state changes and is not used
for this. Accept it only if `handle` is the connection's handle, because the
chip also reports on other handles. The channel is the **low nibble of
`chan_flags`**, 1..9; `0x0` and `0xF` mean no channel, and the call fails.
**The number says nothing about the codec** (openDECTcell#81): the ranges 0..3 /
4..6 / 7..9 once read as ADPCM / PCM / wideband are an allocation pool, and
this host-facing link channel is a different namespace from the chip's
internal channel. Every wideband call measured so far reports channel 1. The
controller logs the channel and never branches on it.

**Both directions work on narrowband calls** (openDECTcell
`notes-for-the-controller.md`, 2026-10-23, which withdraws the earlier warning
and opendectcell-controller#29's reason). Base-originated downlink audio needed a
second `0x0230` with flag bit 6 clear after the audio connect (the chip's
narrowband mode) and signed 16-bit linear PCM on the lane. Both are
cell-internal; on the cell link they show only as the formats of §8.2. An
inbound SIP call can therefore carry audio. Paging a handset that is holding a
link can add about a second before it rings; that is expected, not a fault.

**Testing a downlink:** a tone proves nothing, because pitch survives almost
every misread format and rate. If the controller ever gets an audio path to a
handset, check it by recording the earpiece and correlating its loudness
envelope against the known source; that separates "wrong format" from "not
our data" where listening and the cell's logs cannot.

**The audio bind is not part of openDECTcell#67.** The stack glue that moves into
`libopendectcell` covers Cs, LAPC and NWK only. The bind sequence above, the
channel assignment and everything keyed on `0x0242` stay host-side and remain
the controller's to drive. The controller must accept a `0x0242` that
arrives before its own bind completes.

**What works on the cell side since 2026-10-25** (openDECTcell
`docs/two-calls.md`, `docs/call-g7.md`), for the controller to copy:

- Channel k (1, 2, 3) binds `0x0251 {h, 02, k}`, then `0x0230` with the
  flags nibble as a **bit** (k = 1, 2, 3 → 0x1, 0x2, 0x4) and mask
  `1 << k`, then `0x022D {h, 00}`; stock takes the lowest free channel
  [CR A-0124].
- **Wideband** is stock's bind with flag bit 6 set (`0x41` for channel 1),
  codec byte 0, no re-assign: the lane then carries the coded G.722
  octets (§8.2). **Narrowband** needs the second `0x0230` with bit 6 clear
  after the audio connect (above).
- dect-stubd binds when the call is ACTIVE and the handset has
  authenticated; it does not bind list-access calls. That trigger works on
  every Gigaset tried (A540, C430A, AS690H) on 07.56 and on 06.88.
- The channel → lane mapping is the cell's and is now proven for all three
  channels: on the 7520 channel k is raw TDM word k XOR 1; on the 7412 the
  speech lanes sit next to the control lane.

**Only voice calls get media.** A `{CC-SETUP}`…`{CC-CONNECT}` is also how a
handset runs CAT-iq list access (phonebook, call list), and a Gigaset A540 does
it several times per real call (openDECTcell `docs/notes-for-the-controller.md`,
2026-10-22). The controller assigns an audio channel and dials SIP only for
`<<BASIC-SERVICE>>` call class `0x8` (normal) and `0xA` (emergency); everything
else, list access included, gets no audio (`internal/dect/callclass`).

The controller then sends **MEDIA_OPEN** to tie that chip channel to a UDP
flow:

MEDIA_OPEN (TLV)

| Tag | Name | Type | Req | Notes |
|---:|---|---|---|---|
| 1 | media_id | u32 | yes | Controller-chosen, unique on this link |
| 2 | audio_channel | u8 | yes | The chip channel from `0x0242`, 1..9 (above). The cell maps it to its TDM lane |
| 3 | format | u8 | yes | Downlink format, and uplink unless tag 13 is present. §8.2 |
| 4 | ptime_ms | u8 | no | 10 or 20; default 20 |
| 5 | payload_type | u8 | yes | RTP payload type for both directions |
| 6 | ctl_addr | u8 family (4/6), 4 or 16 bytes address, u16 port | yes | Controller media endpoint |
| 7 | ssrc_up | u32 | yes | SSRC the cell uses |
| 8 | ssrc_down | u32 | yes | SSRC the controller uses |
| 9 | srtp_profile | u8 | yes | 0 none, 1 AES_CM_128_HMAC_SHA1_80, 2 AEAD_AES_128_GCM |
| 10 | key_up | master key ‖ salt (30 or 28 bytes) | if profile ≠ 0 | Cell → controller |
| 11 | key_down | same | if profile ≠ 0 | Controller → cell |
| 12 | dscp | u8 | no | Default 46 (EF) |
| 13 | format_up | u8 | no | Uplink format, if different from tag 3. §8.2 **[proposed]** |

- Keys are random per media session and direction, generated by the controller,
  and never reused with the same SSRC. They exist only in memory on both sides
  and are never logged.
- Profile 0 is accepted only if the cell has `allow_plain_rtp` set by an admin
  and both sides negotiated PLAIN_RTP. The controller refuses it otherwise.
- **The TDM lane is the cell's business.** Which lane, and which byte half of
  a 32-bit DMA word, carries a channel is chip-specific: the logical lane and
  the raw DMA word differ (`raw = logical ^ 1`), and two pairs of lanes share
  one word, one in each half [CR A-0035, openDECTcell#60]. The controller sends
  only the chip's channel number; openDECTcell agreed (openDECTcell#78). The channel →
  lane mapping is proven only for channel 1, and every call so far was given
  channel 1, so other channels may never occur. A cell that has no mapping for
  a channel answers status 1.
  *openDECTcell, 2026-10-30:* the cell finds the lane from the long `0x0242` that
  answered the controller's own bind on a live handle: `audio_channel` must
  equal its `chan_flags` low nibble, its mask bit is the channel k, its flag
  bit 6 the G.722 lane. So MEDIA_OPEN goes after that `0x0242`, as
  `call_up()` sends it; without one, status 1.
- **MEDIA_OPEN_ACK** (TLV): tag 1 `media_id` u32, tag 2 `status` u16 (0 ok,
  1 unknown channel, 2 format unsupported, 3 no capacity, 4 internal).
- **MEDIA_CLOSE** (tag 1 `media_id`) → the cell stops the flow and answers
  **MEDIA_CLOSED** (tag 1 `media_id`, tag 2 `reason` u8: 0 requested,
  1 chip released the channel, 2 local error). The cell also sends
  MEDIA_CLOSED unsolicited when a flow ends on its side.

### 8.2 Audio format: negotiated per call

The format is a **parameter**: the cell lists what it can carry in HELLO, and
the controller picks per call and per direction.

| Id | Format | RTP payload type | Bytes per 20 ms |
|---:|---|---|---:|
| 1 | L16, 8 kHz | dynamic | 320 |
| 2 | L16, 16 kHz | dynamic | 640 |
| 3 | PCMA, 8 kHz | 8 | 160 |
| 4 | PCMU, 8 kHz | 0 | 160 |
| 5 | PCMA, 16 kHz (non-standard) | dynamic | 320 |
| 6 | G.722 | 9 | 160 |
| 7 | G.726-32 (reserved, unused: the chip encodes G.726 itself) | dynamic | 80 |
| 0xF0–0xFF | experimental | dynamic | — |

**The format is per direction.** The downlink and the uplink of one call need
not carry the same format, so HELLO lists them separately and MEDIA_OPEN names
both (proposed 2026-10-23, see below):

- HELLO tag 12 `audio_formats`: what the cell can **accept** from the
  controller (downlink).
- HELLO tag 16 `audio_formats_up`, list of u8 (≤ 16), optional: what the cell
  can **send** (uplink). Absent means the same list as tag 12.
- MEDIA_OPEN tag 3 `format`: the downlink format, and also the uplink format
  unless tag 13 is present.
- MEDIA_OPEN tag 13 `format_up`, u8, optional: the uplink format.

In each list the cell puts its preferred format first. The controller picks one
format per direction from these lists and never assumes one.
**[proposed; asked in openDECTcell#82, §13 C12]**

L16 on the cell link is RFC 3551 L16: signed 16-bit samples in network byte
order. Whatever the lane's word layout, the cell presents it in that form; that
is framing, not transcoding.

What the lane carries (openDECTcell `docs/notes-for-the-controller.md`,
2026-10-23, corrected 2026-10-29 by the calls since):

| Call | Air codec | Lane, both directions | Format | Status |
|---|---|---|---|---|
| Narrowband | G.726 32 kbit/s, coded by the chip | signed 16-bit linear PCM, one sample per 8 kHz frame | 1 | Both directions on every call since 2026-10-23 (07.56, 06.88) |
| Wideband | G.722, coded by the handset and the far end | the G.722 octets themselves: one octet per 8 kHz frame pair, each 16-bit lane word held for two frames (the cell does that framing) | 6 | Both directions since 2026-10-25 on the A540, C430A and AS690H (07.56), and since 2026-10-29 on the 7412 (06.88) |

Consequences:

- **Wideband is the default, and it is a pass-through.** The chip does not
  code G.722; the octets the handset sends are the octets on the lane. A
  cell reports format 6 and format 1 in both lists (tags 12 and 16), and
  `cap_audio_wb = 3`. The controller passes G.722 through to a SIP peer
  that negotiates G.722, and transcodes only when the two ends share no
  codec (the maintainer's rule for openDECTcell's standalone bridge, which the
  controller should mirror: the phones do the coding).
- **Nothing assumes A-law on the narrowband lane.** A-law produced silence in
  one byte half and noise in the other when fed down. Format 3 stays in the
  registry, but no cell offers it.
- **The controller knows the codec before MEDIA_OPEN.** The handset and the FP
  agree on it with `<<CODEC-LIST>>` at registration or location registration,
  optionally restated per call (ETSI TS 102 527-1 §7.3.1; G.722 is codec
  identifier 3 on a long slot, G.726 identifier 2 on a full slot). That is
  C-plane, which the controller runs. A narrowband SIP caller should ring
  the handset narrowband (G.726), and a wideband handset calling a
  narrowband peer should fall back to narrowband
  (`dect_cc_prefer_narrowband()`), so nothing is resampled.
- **From the agreed air codec to the formats** (opendectcell-controller#27):

  | `<<CODEC-LIST>>` codec | Format, both directions | Status |
  |---|---|---|
  | 2, G.726 32 kbit/s | 1, L16 8 kHz | working |
  | 3, G.722 64 kbit/s | 6, G.722 | working |
  | 4, 5 (G.711) | not offered by the cell side's codec list | — |
  | 6, G.729.1 | not offered | — |

  The format is **not** derived from the `0x0242` channel number, although
  #27 once suggested it; openDECTcell#81 confirmed that the number carries no
  codec information (§8.1).
- A narrowband call shows **no HD symbol** on the handset. That is the G.726
  path working, not a defect.
- `voip-requirements.md` §5, `architecture.md` and `sip-ua.md` are corrected
  to match.

### 8.3 Packets and NAT

- Packets are RTP (RFC 3550), SRTP (RFC 3711 / 7714) when keyed. There is no
  RTCP on the cell link in v1; the controller derives loss and jitter from
  sequence numbers and timestamps.
- The cell sends a packet every `ptime_ms` for the whole session, including
  silence, so the NAT binding stays open and the controller can latch.
- *2026-10-31:* the cell does not yet send during silence on an idle lane
  (§15). The controller must not treat a quiet uplink as a dead session: it
  ends a session only on MEDIA_CLOSED, a closed link past the grace period,
  or its own call state, never on missing packets. Until the first packet
  arrives, its downlink is dropped as below; on a live call the lane carries
  words, so the first packet comes within one ptime.
- **Latching:** the controller sends only to the source address of the most
  recent packet that authenticated (SRTP) or that carried `ssrc_up` (plain RTP,
  trusted LAN only). Before the first such packet, it drops its outgoing audio.
  With SRTP this makes latching safe against spoofing; with plain RTP it
  doesn't, which is one reason plain RTP is opt-in.

## 9. Prioritisation and back-pressure

### 9.1 Send scheduling

Each side keeps one queue per class and sends with **strict priority**: P0
before P1 before P2. Bulk payloads are chunked so that no single P2 frame body
exceeds **1024 bytes**. A P0 frame therefore waits at most for one P1 frame or
one P2 chunk already being written.

To stop the kernel's send buffer from defeating that order, both sides set
`TCP_NOTSENT_LOWAT` to **8 KiB** on the control socket (Linux ≥ 3.12; the
cell's kernel is 6.12. It is uapi constant 25: define it if the build headers
lack it, rather than making it conditional) and write P2 only while the socket is writable under that mark. P2 is
also rate-limited to 32 kB/s per cell.

### 9.2 Queue bounds

| Queue | Bound | When full |
|---|---|---|
| P0 send | 64 frames, or oldest frame waiting > 500 ms | The peer is not reading: close the link (`GOODBYE OVERLOAD`) and let resume (§6.4) recover. Dropping mails silently would desynchronise state |
| P1 send | 32 frames | Close the link the same way |
| P2 send | 64 frames | Drop the oldest STATS/log frame, count it |
| Controller: one handset session's inbox | 32 mails | **Reset that session**, never block the cell's reader. One stuck handset must not stall the other handsets on the same cell |

### 9.3 Towards the chip

The control lane carries 2 octets every 8 kHz frame, **16 000 octets/s in each
direction**, and transmits continuously (idle-filled), so an overrun shows as
dropped or corrupt mails, never as back-pressure. Each mail costs
**`12 + plen` wire octets, rounded up to even** (length, sequence, header,
checksum and pad; `dect_mail.c`, openDECTcell#63):

| plen | Wire octets | Payload efficiency |
|---:|---:|---:|
| 3 (a page; `0x0247`) | 16 | 19 % |
| 7 (a memory read) | 20 | 35 % |
| 18 (`0x020A`) | 30 | 60 % |
| 39 (a read reply) | 52 | 75 % |
| 249 (maximum) | 262 | 95 % |

The controller shapes MAIL_DOWN per cell on **wire octets**, computed with that
formula, to **3 000 octets/s** sustained with a burst of 1 000 octets. This is
conservative on purpose. The highest rate openDECTcell has sustained is ~984 octets/s
down and ~2 566 octets/s up (run F1, 180 s, no errors); the lane's raw capacity
is not the limit, the chip's receive handling is, and its threshold is
unmeasured. Expected demand fits easily: Cs carries at most 250 octets/s per
connection (§2), and slot writes are 184 octets/s at their rate limit (§7.4).
openDECTcell agreed to these values (openDECTcell#79). Only MAIL_DOWN counts against
the budget; the chip's own mails share the lane in the other direction.
Raising the shaper needs a deliberate ceiling measurement on the cell side
first.

The cell keeps a bounded queue to the chip and
reports its depth with **FLOW** (fixed: `queue_bytes u16`, `state u8`: 0 ok,
1 above 2 KiB, 2 full) whenever the state changes. On state 2 the controller
defers MAIL_DOWN that its DECT core marks as non-urgent (slot writes, status
polls), and raises an alarm if the state lasts longer than 2 s.

## 10. Cell management

**CONFIG_SET** (TLV; unknown tags ignored; the cell applies what it understands
and answers **CONFIG_ACK**, tag 1 `status` u16: 0 ok, 1 partially applied):

| Tag | Name | Type | Default |
|---:|---|---|---|
| 1 | log_level | u8 (0 error … 3 debug) | 1 |
| 2 | orphan_s | u16 | 60 |
| 3 | stats_interval_s | u16 | 60 |
| 4 | media_dscp | u8 | 46 |
| 5 | allow_plain_rtp | u8 | 0 |
| 6 | rfpi | 5 octets, or empty | the box's factory RFPI |

Radio on/off is **not** cell configuration: the controller sets it directly by
mail. The DECT identity is, since 2026-11-01 (tag 6, the only tag a cell applies):
the 40-bit RFPI the chip is to broadcast, class A or B with E 0; **empty** asks
the cell to go back to its own. No mail sets the RFPI at runtime (room A-0131), so
the cell stores it in `/etc/opendectcell/cell/rfpi`, which the init script uploads the
chip with, and restarts its service once per value asked for: a chip that still
reports another RFPI after that reload is not reloaded again (CONFIG_ACK status 1).
A message holding only tag 6 is answered status 0 when it was accepted.

*2026-10-29:* no mail is known that sets the RFPI. It is patched into the chip
firmware at upload (`dect-fwload`), so a cell's RPN changes with a firmware
reload. How the system identity is chosen and why a class A system
has at most 7 cells: openDECTcell `docs/multicell-identities.md`.

**STATUS** (cell → controller, sent on every change and in reply to
STATUS_REQUEST): tag 1 `radio_state` u8, tag 2 `chip_state` u8 (0 down,
1 starting, 2 running, 3 fault), tag 3 `chip_epoch` u32, tag 4 `uptime_s` u32,
tag 5 `detail` string ≤ 128, tag 6 `rfpi` 5 octets (2026-11-01): the RFPI the
chip broadcasts, from its contact answer; HELLO carries it as tag 17.

**STATS** (cell → controller, every `stats_interval_s` and on request): tag 1,
repeatable, `counter_id u16 | value u64`.

| Id | Counter |
|---:|---|
| 1 | mails up |
| 2 | mails down |
| 3 | mails rejected or dropped |
| 4 | control bytes up |
| 5 | control bytes down |
| 6 | reconnects since boot |
| 7 | chip restarts since boot |
| 8 | RTT p50 (ms, cell-measured) |
| 9 | RTT p95 (ms) |
| 10 | RTP packets sent |
| 11 | RTP packets received |
| 12 | RTP packets lost (received side) |
| 13 | chip queue high-water mark (bytes) |
| 14 | P2 frames dropped |
| 15 | STUB link: mails dropped from the chip queue |
| 16 | STUB link: checksum errors |
| 17 | STUB link: framing errors |
| 18 | STUB link: sequence gaps |
| 19 | STUB link: resyncs |

Counters 15–19 are the ones `dect-stubd` already keeps (`queue_drops`,
`csum_errors`, `framing_errors`, `seq_gaps`, `resyncs`), and openDECTcell will
export them (openDECTcell#79). In healthy operation all five stay at zero. They mean
different things:

- **15 (queue drops) is back-pressure**: the cell could not hand a mail to the
  chip in time. If it rises between two STATS reports, the controller halves
  the cell's shaping rate (§9.3) and restores it step by step after 5 minutes
  without drops.
- **16–19 are link faults**: the wire corrupted or lost mails, which slowing
  down may not fix (a bad STUB load or a chip needing re-initialisation also
  cause them). A rise raises an admin alarm and does not touch the shaper.
  `resyncs` means mails were lost, not just delayed.

Unknown counter IDs are ignored, so new ones need no version bump.

## 11. Errors and closing

**ERROR** (TLV): tag 1 `code` u16 (required), tag 2 `ref_type` u16, tag 3
`ref_corr` u16, tag 4 `text` string ≤ 128 (never secrets, keys, PINs or
identities). An ERROR never closes the link by itself.

| Code | Name | Meaning |
|---:|---|---|
| 1 | UNKNOWN_TYPE | Type not recognised |
| 2 | MALFORMED | Layout or required tag wrong |
| 3 | TOO_LARGE | Body above the type's bound |
| 4 | NOT_ALLOWED | Not allowed in the current cell state |
| 5 | UNSUPPORTED | Capability not negotiated |
| 6 | BUSY | Temporarily cannot process; the sender may retry once after 1 s |
| 7 | MEDIA_FAILED | A media operation failed |
| 8 | INTERNAL | Bug on the reporting side |

**GOODBYE** (TLV): tag 1 `reason` u16 (required), tag 2 `text` ≤ 128. The
sender closes the connection after writing it.

| Reason | Name | Cell's next step |
|---:|---|---|
| 1 | SHUTDOWN | Reconnect with back-off |
| 2 | PROTOCOL | Reconnect with back-off; log as an error |
| 3 | VERSION | Do not reconnect until the software changes; show in LuCI |
| 4 | REVOKED | Erase credentials; stop |
| 5 | SUPERSEDED | Nothing (a newer connection exists) |
| 6 | OVERLOAD | Reconnect immediately (resume) |
| 7 | AUTH | Certificate not accepted; stop and show in LuCI |

## 12. Reserved: log shipping, updates, debug

**Decided 2026-10-31: not part of v1.** Question 4 in `first-tasks.md` §6
(does the link need log shipping, remote firmware update or a debug channel?)
is answered no for v1: a cell's log is read on the box (LuCI, `logread`), its
firmware comes from the apk feed, and the STATS and STATUS messages of §10
carry what the controller needs to see. v1 still reserves type ranges and
capability bits for all three (§3.3), so they can be added as a minor version,
as P2 traffic, under the chunking and rate rules of §9. None is specified here.

## 13. Questions for the cell side

All of C1–C11 were answered by openDECTcell on 2026-10-22, and the answers are folded
into the sections named. What is still unsettled is marked **[cell?]** above
and listed under *Still open*; none of it blocks this spec, because each has a
fallback written into it. C12 (2026-10-23) was answered on 2026-10-29, with
corrections to C1, C3, C5 and C9 (marked *2026-10-29*).

| # | Question | Answer | Still open |
|---|---|---|---|
| C1 | Does the chip's subscription table survive a reset? Can the host read it back? | *2026-10-29:* it survives daemon restarts and parks (`vF426`/`vF427`); whether a chip reset empties it is not established. Read-back by a memory read at a build-specific address. §7.4 | Whether a park ever empties the table |
| C2 ([opendectcell#58](https://github.com/openDECTcell/opendectcell/issues/58)) | Does `0xF604` reach the host, with identity, before the chip refuses a handset without a record? | Not established: never seen in any capture. Fallback (home set only) endorsed. §7.4 rule 3 | Bench run with an empty slot table, queued ([#74](https://github.com/openDECTcell/opendectcell/issues/74)) |
| C3 ([opendectcell#59](https://github.com/openDECTcell/opendectcell/issues/59)) | Write confirm? Slot clear? `status`/`extra`? | No confirm observed; writes succeed silently. *2026-10-29:* op 0 clears [CR A-0065]. `status`/`extra` unknown, `0x00` works. Byte layouts given. §7.4 | — |
| C4 | Which field identifies a radio connection? | `handle` (0..6) plus a 3-byte handset `tag` in `0x0203` (`dect_cs.h`) | — |
| C5 ([opendectcell#60](https://github.com/openDECTcell/opendectcell/issues/60), [#78](https://github.com/openDECTcell/opendectcell/issues/78)) | How does `0x0242` identify the audio channel, and which PCM slot carries it? | Low nibble of octet 1 of the long form, matched on handle; lane mapping `raw = logical ^ 1`, lanes may share a word. The number is not a codec class ([#81](https://github.com/openDECTcell/opendectcell/issues/81)). *2026-10-29:* three channels, binds and lanes in §8.1 | — |
| C6 ([opendectcell#61](https://github.com/openDECTcell/opendectcell/issues/61), [#80](https://github.com/openDECTcell/opendectcell/issues/80)) | Will `dect-stubd` release calls itself when the grace period ends? | Release calls and audio at 7 s, then park the radio; do not reset the chip. Committed for M9 (#80). §6.4 | — (also agreed in #80: the controller is the only slot writer and the only one that unparks; §7.4) |
| C7 ([opendectcell#62](https://github.com/openDECTcell/opendectcell/issues/62)) | TLS library, TLS 1.3 + P-256, `TCP_NOTSENT_LOWAT`? | mbedTLS 3.6.6, already installed; `TCP_NOTSENT_LOWAT` available (kernel 6.12). §4, §9.1 | TLS 1.3 client certificates confirmed compiled in by binary evidence ([#75](https://github.com/openDECTcell/opendectcell/issues/75)); a real handshake is offered and wanted before C2 |
| C8 ([opendectcell#63](https://github.com/openDECTcell/opendectcell/issues/63)) | Per-mail overhead on the TDM control channel? | `12 + plen`, rounded up to even; 16 000 octets/s raw. 3 000 octets/s and counters 15–19 agreed (#79). §9.3 | The chip's overrun threshold |
| C9 ([opendectcell#64](https://github.com/openDECTcell/opendectcell/issues/64)) | Cell-link audio format? | *2026-10-29:* narrowband is L16 8 kHz both ways; wideband is G.722 octets both ways and works on all handsets tried. §8.2 | [#77](https://github.com/openDECTcell/opendectcell/issues/77) pacing |
| C10 ([opendectcell#65](https://github.com/openDECTcell/opendectcell/issues/65)) | Does a handset accept a different TPUI per cell? | The premise is wrong: a TPUI is per location area, and slot-derived TPUIs collide across cells. Decouple TPUI from slot. §7.4 | Handset behaviour with a pool TPUI and on reassignment, and whether ciphering depends on the slot-shaped TPUI ([#76](https://github.com/openDECTcell/opendectcell/issues/76)); `libopendectcell` change L10 |
| C11 ([opendectcell#66](https://github.com/openDECTcell/opendectcell/issues/66)) | One 5-octet Cs fragment per 10 ms frame? | No: at most one per 20 ms (`C_T` only in odd frames), so ≥ 280 ms for a 70-octet frame. §2 | openDECTcell's move of the LAPC timer to 2 s |
| C12 ([opendectcell#82](https://github.com/openDECTcell/opendectcell/issues/82)) | Per-direction formats: HELLO tag 16 `audio_formats_up`, MEDIA_OPEN tag 13 `format_up`. OK for M9? | *2026-10-29:* yes, kept. Both directions use the same format on every call today, so a cell sends tag 16 equal to tag 12 | — |

### 13.1 Proposals from the cell side (2026-10-29)

For the controller to accept or refuse; each has the cell's fallback.

| # | Proposal | Why | Cell's fallback |
|---|---|---|---|
| P1 | HELLO tag 3 `stub_version` is required (done above) | The builds differ in mail shape, admission and memory addresses | The cell always sends it |
| P2 | STATUS tag 6 `slot_audit`, repeatable: `slot u8, state u8` (0 empty, 1 matches what was written, 2 differs, 3 not readable on this build), sent after each slot write the cell could read back | The read-back address is per build, so only the cell can do it; it turns *writing* into a verified *present* | The cell does not send it until accepted |
| P3 | STATUS tag 7 `cell_mode_since` u64 (Unix ms) | The cell's local DECT functions are switched off in cell mode (§14.1); the admin should see since when | Not sent |
| P4 | MEDIA_OPEN for format 6 means G.722 octets in RTP payload type 9 at 8000 Hz RTP clock (RFC 3551's G.722 quirk), 160 octets per 20 ms | The lane carries the octets as they are (§8.2), so the cell does no coding | As stated |

**Controller's answer, 2026-10-31: P1–P4 are accepted.** P2 and P3 become
optional STATUS tags (6 `slot_audit`, 7 `cell_mode_since`) that the cell may
send from now on; the controller ignores them when absent and shows them
when present. The controller never depends on either: P2 turns "written"
into "verified present" in the admin UI when it is there, and a slot is
treated as present once the write was sent, as §7.4 says. The cell side has
not built either yet (§15).

## 14. The cell side in openDECTcell

How openDECTcell implements the cell, so the controller knows what it talks to.
Owned by the cell side; changes are announced in openDECTcell's
`docs/notes-for-the-controller.md`.

### 14.1 Cell mode: one switch, nothing local

A box is either a **standalone base** (openDECTcell's default: its own DECT
stack, handsets registered on the box, SIP through baresip) or a **cell**.
The switch is one setting, `opendectcell.main.role` = `base` | `cell`, in uci
and in LuCI (Network > DECT), set by `/etc/init.d/opendectcell enroll` too. In cell mode:

| Part | Standalone base | Cell |
|---|---|---|
| `dect-stubd` | runs the DECT stack (`libopendectcell`): registration, calls, lists | forwards mails (§7) and lanes (§8) only; no stack, no host subscription store, no slot writes, never turns the radio on by itself |
| Handset records and keys on the box | `/etc/opendectcell-sub.*` | none; existing records are kept on flash but not loaded |
| baresip and the SIP lines | running when a handset has a line | **not started**: no line takes memory |
| `dect-cell` | not started | the controller link: TLS, enrollment, HELLO, heartbeat, resume, renewal, RTP/SRTP |
| LuCI Network > DECT | all settings | the controller settings only (enrollment string, state, a button to leave cell mode); everything else hidden |
| LuCI Status > DECT | handsets, VoIP, log | radio and chip state, the controller link (state, RTT, reconnects), mail and lane counters, the slot audit, the log |

Leaving cell mode erases the cell's key and certificate (they belong to
one controller) and starts the standalone base with the records it had.

### 14.2 Processes and the local interface

`dect-cell` (TLS, mbedTLS) and `dect-stubd` (the chip) are separate
processes, so a TLS handshake or a slow network never delays the chip's
control lane. They talk over one local `SOCK_SEQPACKET` socket,
`/var/run/opendectcell-cell.sock` (mode 0600), in **this document's frame
format** (§3.1), unencrypted, one frame per message. `dect-stubd` sends and
receives only the types that concern the chip:

| Type | Direction on the local socket |
|---|---|
| MAIL_UP, FLOW, STATUS, STATS (counters 13, 15–19), STATE_SNAPSHOT contents | `dect-stubd` → `dect-cell`, which forwards them |
| MAIL_DOWN, MEDIA_OPEN, MEDIA_CLOSE, CELL_STATE | `dect-cell` → `dect-stubd` |
| MAIL_REJECT, MEDIA_OPEN_ACK, MEDIA_CLOSED | `dect-stubd` → `dect-cell` |

`dect-cell` answers everything else itself (HELLO, heartbeat, GOODBYE,
enrollment, renewal, trust, CONFIG_SET). When the controller link is lost,
`dect-cell` tells `dect-stubd` with a CELL_STATE carrying the local reason
`link-lost`; `dect-stubd` runs the timeline of §6.4 (release at
`grace_ms`, park at `orphan_s`) on its own clock, so a crashed `dect-cell`
cannot leave the radio on.

Audio of a media session goes over the same socket openDECTcell's SIP bridge
uses (`docs/bridge-interface.md` §3 in openDECTcell): one connection per
session, 20 ms frames of 160 G.722 octets or 160 L16 samples; `dect-cell`
does the RTP and SRTP framing.

### 14.3 First version: scope (2026-10-29)

Planned for the first version, in this order; openDECTcell's
`docs/notes-for-the-controller.md` says which parts have landed:

1. the frame and TLV codec, with unit tests;
2. cell mode in `dect-stubd`: the mail tunnel on the local socket, no local
   stack, the §6.4 timeline on link loss;
3. `dect-cell`: enrollment, mutual TLS 1.3 with the pinned CA, HELLO,
   heartbeat, reconnect with back-off;
4. the cell-mode switch in the init script and LuCI (§14.1).

Later: media, resume and STATE_SNAPSHOT, renewal and trust updates,
STATS. *2026-10-30:* STATS, resume, media and renewal are built (openDECTcell
`docs/notes-for-the-controller.md`, "The cell's media is built": SRTP
profiles 1 and 2, one format both ways, plain RTP only where the admin
allows it; "The cell renews its certificate"); trust updates are not.

## 15. Conformance: the cell as built (2026-10-31)

Checked against openDECTcell `6c910b1` and later (`dect-cell`, `dect-stubd -K`);
the source of each row is in openDECTcell's `docs/notes-for-the-controller.md`.
**Built** means unit-tested and, where noted, proven on a 7412 against this
controller in Docker. The wire format of every row is final; a row marked
*not built* changes the cell, never the format.

| Part | Spec | State on the cell | What the controller does meanwhile |
|---|---|---|---|
| Frame, TLV, error handling | §3 | Built | |
| Enrollment, mTLS 1.3, pinned CA | §4, §5.1–5.3 | Built, proven on a 7412 (2026-10-30) | |
| Revocation | §5.5 | Built | |
| Renewal | §5.4 | Built (cell asks at 2/3 of the certificate's life; new pair serves from the next connection) | Keep both fingerprints until the cell connects with the new one |
| **CA rotation (TRUST_UPDATE)** | §5.6 | **Not built** | Do not rotate the cell CA. If it must change, re-enrol the cells. Implement the controller end only when the cell's lands |
| HELLO, heartbeat, STATUS, STATS (1–19), resume, STATE_SNAPSHOT | §6, §10 | Built, proven on a 7412 | |
| **CONFIG_SET** | §10 | The cell answers CONFIG_ACK status 1 (partially applied) and applies **nothing**. `stats_interval_s` is 60 s fixed; `allow_plain_rtp` is the admin's local setting (`dect-cell -P`) | Treat every CONFIG_SET as advisory. Do not build UI that implies a cell took a value |
| STATUS tags 6 and 7 (P2, P3) | §13.1 | Not built | Optional; ignore when absent |
| Mail tunnel, MAIL_REJECT, FLOW | §7, §9.3 | Built (a malformed MAIL_DOWN gets ERROR MALFORMED; a full socket MAIL_REJECT QUEUE_FULL) | |
| MAIL_DOWN, MEDIA_OPEN, MEDIA_CLOSE while the cell is not active | §5.3 | ERROR NOT_ALLOWED | |
| Media (MEDIA_OPEN/ACK/CLOSE/CLOSED) | §8 | Built: SRTP profiles 1 and 2, plain RTP only when the admin allows it, one format both ways (tag 13 equal to tag 3), `ptime` 10 or 20. **Not on air yet**: openDECTcell bench card M1 | Send MEDIA_OPEN after the long 0x0242 (§8.1) |
| Silence packets every ptime | §8.3 | Not met: a silent or idle lane sends nothing | Never end a session on a quiet uplink (§8.3) |
| Send scheduling, `TCP_NOTSENT_LOWAT`, P0 bound of 500 ms | §9.1–9.2 | **Not built** on the cell; writes block up to 5 s | The controller side applies §9 in full; a cell that stops reading is closed by the controller's own bound |
| Local socket between `dect-cell` and `dect-stubd` | §14.2 | Built | |

**Two cell-side points that change nothing on the wire:**

- The enrollment string no longer reaches `dect-cell` on a command line when LuCI enrols a cell (a root-only file, `enroll @FILE`; openDECTcell 2026-10-31); a CLI user passing the string itself still shows it in `/proc` while the enrolment runs.
- Link loss: `dect-stubd` runs §6.4's timeline on its own clock; a controller
  that sets CELL_STATE gets the state alone, never a reason (§14.2).
