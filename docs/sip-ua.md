# SIP user agents (C4)

Status: draft, 2026-10-22 (ROADMAP C4, directed by opendectcell-controller#46).
Pending maintainer review. Requirements: `docs/voip-requirements.md`.

## 1. Shape

```
 DECT core (C3) ──Endpoint──┐                       ┌── PBX / provider
                             │   internal/sip         │
 media engine (C5) ──Media──┤  Manager               │
                             │   ├─ Line (per account, per handset line)
 web/API (C6) ──Accounts────┘   ├─ Registration ──REGISTER──▶ registrar
                                 ├─ Call (dialog) ──INVITE/BYE──▶
                                 └─ resolve (RFC 3263), SDP, RTP/SRTP
```

| Part | Does |
|---|---|
| `internal/sip/resolve` | RFC 3263: NAPTR → SRV → A/AAAA, ordered targets for failover |
| `internal/sip/sdp` | Offer/answer: codec choice, telephone-event, SRTP SDES (RFC 4568), hold direction |
| `internal/sip/rtp` | One RTP/SRTP endpoint per call (pion); the media engine (C5) reads and writes frames |
| `internal/sip` | Manager: transports, accounts' registrations, calls in and out, DTMF, keep-alive |
| `internal/sipaccount` | Accounts in the database (password sealed), authorisation, audit; API and UI use it |

The DECT core is not there yet (C3 is frozen, #46). The SIP side talks to it
through an `Endpoint` interface: "ring handset X on line N with caller Y",
"the handset answered / hung up / dialled / sent DTMF". Tests and, until C3,
a loopback implementation stand in for it.

## 2. Decisions

| Decision | Alternatives | Reason |
|---|---|---|
| **sipgo** for SIP messages, transactions and dialogs | Own stack; diago (on top of sipgo) | `architecture.md` §7 chose it. diago bundles its own media and call model, which would fight the cell-side media path |
| **Own RFC 3263 resolution**, then send to an explicit destination | sipgo's built-in SRV lookup | sipgo has no NAPTR and no failover across targets. Both are required (`voip-requirements.md` §2, e.g. Telekom) |
| **One shared transport set** (UDP, TCP, TLS listeners on the configured ports) for all accounts | One socket per account | "One UA per line" is a logical UA. A socket per account multiplies ports and NAT bindings for no gain. Each account gets its **own Contact user part** (`dw-<id>`), which is how incoming requests find their account |
| Contact address learned from the registrar's `received`/`rport` (symmetric signalling) | STUN only | Works behind NAT without extra servers. STUN and a static public address remain overrides (`advanced`) |
| Media: always relayed by the controller (`architecture.md` §7), one RTP port pair per call from the configured range | — | Settled |
| **G.722 passes through untranscoded** when both sides agree on it (`cell-protocol.md` §8.2) | Always transcode | The cell delivers coded G.722. Transcoding is only for a SIP peer without G.722 (C5) |
| SRTP: SDES only with TLS signalling; policy *off / optional / mandatory* per account | DTLS-SRTP | `voip-requirements.md` §7: SDES is M, DTLS-SRTP is L |
| Digest: MD5 and SHA-256, qop `auth`, 401 and 407 | — | `voip-requirements.md` §3 |
| **TLS verified against the SIP domain (or the SRV target name)** by our own check, with system roots; per account it can be turned off | sipgo's default check | sipgo shares one TLS configuration and would check the certificate against the IP address we dial. We dial TLS servers by name (so SNI is sent), and registrars given as an IP literal share one expectation: any of their domains is accepted, and verification is on if any account asks for it |

## 3. Registration

- One registration per enabled account with `register = true`; accounts used
  by several handsets register once.
- Refresh at `expires × 0.8` (or 30 s before, whichever is earlier) of what
  the registrar granted. On failure, back off 30 s, 60 s, 120 s … capped at
  10 min, with ±20 % jitter, and move to the next resolved target on a
  transport error or 5xx/503. A 401/407 that persists after one retry with
  credentials is `failed` (wrong password); it is retried on the slow
  schedule only.
- Keep-alive: `auto` = CRLF every 30 s on TCP/TLS, OPTIONS every 30 s on UDP
  when the registrar's address is not a private address.
- States for the API (`SipAccountStatus`): `disabled`, `registering`,
  `registered`, `failed`, `backoff`, with the last error text (status line
  only, never credentials).

## 4. Calls

| Event | SIP |
|---|---|
| Handset dials on line N | INVITE with SDP offer to the account's AOR domain via the resolved outbound proxy or registrar; 401/407 answered with credentials; 180/183 → ringback or early media to the handset; 200 → connected |
| Incoming INVITE to `dw-<id>` | Find the account; ring every handset with that account on a line, following each handset's dual-ring policy; first to answer gets 200, the others are cancelled; nobody → 480 after 60 s or on CANCEL |
| Hold / resume | re-INVITE with `a=sendonly` / `sendrecv`; a remote hold is passed to the handset as held |
| DTMF | RFC 4733 events on the RTP stream, SIP INFO, or in-band (C5), per account |
| Busy | `486` when the handset policy says busy, or the cell has no capacity (`cell-protocol.md` §7.5: 503) |

Emergency numbers are dialled like any other number, never blocked
(`voip-requirements.md` §3).

## 5. Testing

- Unit: resolver against an in-process DNS server; SDP offer/answer tables;
  digest with both algorithms; registration timers with a fake clock.
- Integration: two controllers' SIP stacks against an in-process registrar
  and UAS built on sipgo.
- Interop (`AGENTS.md` §7): CI installs Asterisk (Ubuntu package) and runs
  register, call in, call out, hold and DTMF against it. FreeSWITCH has no
  Ubuntu package; it follows as a container job.
