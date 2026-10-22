# VoIP requirements

Goal: the **necessary basics, but enough to work with most PBXes and SIP providers** (Asterisk/FreePBX, FreeSWITCH/FusionPBX, 3CX, Kamailio-based services, FRITZ!Box as registrar, common German providers such as Telekom, sipgate and easybell). The controller acts as a **SIP user agent per handset line**. It is not a proxy, registrar or PBX.

Priority: **M** = must have for 1.0, **S** = should have, **L** = later or optional.

## 1. Accounts and lines

| Item | Prio | Notes |
|---|---|---|
| Up to **2 SIP accounts per handset** ("line 1", "line 2") | M | Accounts are owned by a user and assigned to that user's handsets. The same account may be assigned to several handsets, in which case all of them ring |
| Incoming on either line | M | The handset rings. It shows the line name if the handset supports multi-line (CAT-iq 2.0); otherwise the line is indicated in the caller display, e.g. as a prefix |
| Outgoing line selection | M | Default line per handset. Override via (a) the CAT-iq line selection if the handset supports it, or (b) a configurable **line prefix** on GAP handsets (e.g. `*1#`/`*2#`, stripped before dialling) |
| Per-line enable/disable, display name | M | |
| Concurrent calls per line / per handset | S | Limit configurable; additional calls get busy (`486`) |

## 2. Account parameters (per SIP account)

| Parameter | Prio | Notes |
|---|---|---|
| Display name, user (AOR user part), domain | M | |
| Authentication username (if different), password | M | Stored encrypted |
| Registrar (host/port), **outbound proxy** (loose routing) | M | Many providers require an outbound proxy |
| Transport: UDP / TCP / **TLS** | M | Per account |
| Registration on/off | M | "Off" for IP-authenticated trunks or peer setups |
| Registration expiry, re-register margin, retry/back-off on failure | M | |
| DNS: NAPTR → SRV → A/AAAA resolution with failover (RFC 3263) | M | Required by e.g. Telekom and many hosted PBXes |
| IPv4 / IPv6 / dual-stack | S | |
| Keep-alive: CRLF (TCP/TLS), OPTIONS ping, or registration refresh | M | Needed behind NAT |
| NAT: `rport` (RFC 3581), symmetric RTP/signalling | M | |
| STUN server, or a static public address/port override | S | For deployments behind NAT without SIP ALG |
| Local SIP port / port range, RTP port range | M | Controller-wide, with per-account override (S) |
| Custom User-Agent string | S | Some PBXes provision by UA |
| Caller-ID source for incoming calls: From / P-Asserted-Identity / Remote-Party-ID | M | Auto (prefer PAI) plus override |
| Outgoing identity: P-Preferred-Identity / P-Asserted-Identity, CLIR via `Privacy: id` (RFC 3323/3325) | S | Per account, plus per-call CLIR from the handset (S) |
| Codec list and order (see §5) | M | Per account |
| DTMF mode (see §6) | M | Per account |
| Encryption settings (see §7) | M | Per account |
| Session timers (RFC 4028) on/off, interval | S | Some PBXes require them |
| Digit collection for overlap dialling: inter-digit timeout, `#` terminator | M | Not a dial plan, just collecting digits (§4) |

## 3. Signalling features

| Feature | Prio | Notes |
|---|---|---|
| Digest authentication: MD5, **SHA-256** (RFC 8760), qop `auth` | M | 401 and 407 handling |
| Reliable provisional responses / PRACK (RFC 3262) | S | Support when requested |
| Early media (183 + SDP), ring-back from the network | M | Play network ring-back to the handset |
| Forked responses (multiple early dialogs) | S | |
| Redirect (3xx) following | S | |
| Hold / resume (re-INVITE `sendonly`/`inactive`, RFC 3264), incl. remote hold | M | The handset's hold key maps to a re-INVITE. Music on hold comes from the PBX |
| Call waiting: second incoming call while busy | S | Offered to the handset if it supports call waiting (CAT-iq); otherwise `486 Busy` (configurable) |
| Transfer: blind and attended via REFER (RFC 3515) / Replaces (RFC 3891) | S | Only if the handset can trigger it; otherwise handled on the PBX side (e.g. feature codes) |
| Alert-Info → distinctive ring | L | Mapped to the DECT alerting pattern if the handset supports it |
| Message waiting: SUBSCRIBE `message-summary` (RFC 3842) and unsolicited NOTIFY | S | Shown on the handset via the DECT feature framework (`docs/dect-features.md`) |
| Emergency numbers | M | No special handling and **never blocked**. The number is passed to the PBX/provider unchanged |

## 4. Dialling

- **En-bloc dialling** (handset sends the complete number) is the normal case for DECT handsets and is passed straight to SIP.
- **Overlap dialling** (digit by digit, "sending complete" missing): collect digits until the handset signals completion, `#` is pressed, or the inter-digit timeout expires. Then send the INVITE. No number rewriting beyond stripping a line prefix.

## 5. Media and codecs

| Item | Prio | Notes |
|---|---|---|
| **G.722** (wideband) | M | Matches DECT wideband (CAT-iq). The cell delivers the coded G.722 stream itself, so a G.722 SIP call needs no transcoding (`cell-protocol.md` §8.2) |
| **G.711 A-law / µ-law** | M | Narrowband baseline |
| Opus | L | Optional, at extra CPU cost |
| G.729 | L | Licensing is no longer an issue, but it adds little value; only if demanded |
| ptime 20 ms (configurable 10/20/30/40) | M | |
| RTP/RTCP (RFC 3550), symmetric RTP (RFC 4961) | M | |
| RTCP-mux (RFC 5761) | S | |
| Adaptive jitter buffer, packet-loss concealment | M | Controller side |
| Comfort noise (RFC 3389) | L | |
| DSCP marking: EF for RTP, CS3/AF31 for SIP | S | Configurable |
| Call statistics: MOS estimate, jitter, loss (RTCP) | S | For the admin UI |

The cell link carries G.722 for wideband calls and A-law for narrowband ones (`cell-protocol.md` §8.2, measured at openDECTcell G7). The controller passes the stream through when the SIP side negotiated the same codec, and transcodes otherwise (for example G.722 ↔ G.711 when the SIP peer offers no G.722). The cell never transcodes.

## 6. DTMF

| Mode | Prio |
|---|---|
| RFC 4733 telephone-event (default, payload type negotiated) | M |
| SIP INFO (`application/dtmf-relay`, `application/dtmf`) | S |
| In-band (generated tones) | S |

Keypad digits from the handset during a call (DECT keypad information) are converted into the selected DTMF mode.

## 7. Encryption

| Item | Prio | Notes |
|---|---|---|
| **SIP over TLS** 1.2/1.3 | M | Server certificate verification against the system CA store or a configurable CA, plus hostname check/SNI. Options: certificate pinning; client certificate (mutual TLS) for PBXes that require it |
| `sips:` URIs | S | Handle when the PBX uses them |
| **SRTP with SDES** key exchange (RFC 4568) | M | Crypto suites `AES_CM_128_HMAC_SHA1_80` and `_32` (M); `AEAD_AES_128_GCM`/`256_GCM` (RFC 7714) (S). SDES is only meaningful with TLS signalling, so the UI warns otherwise |
| **DTLS-SRTP** (RFC 5763/5764) | L | For PBXes that only offer DTLS-SRTP |
| ZRTP | — | Not planned. End-to-end verification would need handset UI |
| Per-account media-encryption policy: off / optional (offer, accept plain) / mandatory (reject plain) | M | |
| Encryption status per call in the admin UI and call log | S | Optional "secure call" indication on the handset via the feature framework (L) |

**Separate from SIP:** the **cell↔controller** link is protected independently. Control runs over TLS with mutual authentication, and audio is protected with SRTP keyed over the control channel, or DTLS. See `docs/architecture.md`.

## 8. Interoperability test matrix (target for 1.0)

| PBX / provider | Transports | Must pass |
|---|---|---|
| Asterisk (PJSIP), FreePBX | UDP, TLS + SRTP | register, in/out calls, hold, DTMF, MWI |
| FreeSWITCH / FusionPBX | UDP, TLS + SRTP | same |
| 3CX | UDP/TLS | register, calls, DTMF |
| FRITZ!Box as registrar | UDP | register, calls, G.722 |
| A public provider (e.g. sipgate or Telekom) | UDP/TLS, SRV, outbound proxy | register, calls, NAT keep-alive |
