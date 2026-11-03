# First tasks: C0 design and C1 skeleton

Status: written 2026-10-19, for a second developer joining the controller while
the cell side (openDECTcell) finishes its G7 gate.

This is a starting brief, not a specification. It says what to build first, what
you can already rely on, what is genuinely still unknown, and what to ask rather
than guess. Everything listed here is **hardware-free** — none of it needs a
FRITZ!Box, a handset or a DECT radio.

## 1. Orientation: read these first, in this order

| # | File | Why |
|---|---|---|
| 1 | `README.md` | What the controller is, and the hard "not a PBX" boundary |
| 2 | `AGENTS.md` | The rules. Licence, clean-room, commit discipline, security. Binding |
| 3 | `docs/architecture.md` | Components, data model, cell protocol sketch, and §7 decisions already made |
| 4 | `ROADMAP.md` | Milestones C0..C10; this brief covers C0 and C1 |
| 5 | `docs/voip-requirements.md` | SIP/RTP parameters to support |
| 6 | `docs/dect-features.md` | The per-model feature framework (later, but shapes the data model) |

`docs/architecture.md` §7 lists decisions that are **settled**: Go, cgo to
`libopendectcell`, Docker + PostgreSQL (no SQLite), local accounts + OIDC with
just-in-time user creation, 0–2 SIP lines per handset, media always relayed
through the controller, English-only UI, design target 10 cells / 50 handsets.
Do not relitigate these. If one looks wrong, raise it as a question (§6) rather
than quietly designing around it.

## 2. Constraints that bind every task

- **Licence: AGPL-3.0-or-later.** Every source file gets
  `SPDX-License-Identifier: AGPL-3.0-or-later`. Dependencies must be
  AGPL-compatible: GPL-2.0-**or-later**, GPL-3.0, LGPL, MIT, BSD, Apache-2.0.
  **GPL-2.0-only cannot be linked in** — check this before adding anything.
  `libopendectcell` is GPL-2.0-or-later, so it is fine.
- **Clean-room rules are inherited** from the openDECTcell repo and apply here in
  full. Never commit, paste or quote AVM material — binaries, firmware,
  decompiled or disassembled code. You will not need any of it for these tasks.
- **Secrets never land in the repo or the logs.** UAK and SIP passwords are
  encrypted at rest with a key from config or a secret store. Logs carry state
  transitions, never key, PIN or identity bytes.
- **Commit early, push after every commit**, imperative subject ≤72 chars, then
  the *why*. Never force-push. Record each session in the maintainers' notes — see
  §7, this is how the maintainers review the work.
- **No DECT or SIP call logic yet.** `AGENTS.md` §7 gates that on openDECTcell's G7.
  C0 design and the C1 skeleton are explicitly allowed; see §5 for the line.

## 3. What the cell side actually gives you

This is measured behaviour from working hardware, not a plan. It is the part of
the picture that was missing when `docs/architecture.md` was drafted, and it
constrains the protocol spec more than anything else in this brief.

### 3.1 The unit of exchange is a STUB mail

A cell runs `dect-stubd`, which talks to the DECT chip over a PCM/TDM control
channel. Everything — radio control, registration, signalling, audio setup —
is one message shape:

```
src:  u8    source task      (the host uses 0x22)
dst:  u8    destination task (the chip's contact task is 0x02)
prim: u16   primitive id
param: up to 249 bytes
```

To the transport this is opaque and length-prefixed, exactly as
`architecture.md` §4 says. To the DECT core it is meaningful. The primitives
that already work on hardware, and that the controller will therefore have to
drive:

| Primitive | Meaning |
|---|---|
| `0x015B` | Radio / beacon on and off |
| `0x022B` / `0x022C` | Open and close the registration window |
| `0x0247` + `0x020A` | Write a subscription record: slot index plus the handset's 16-byte key |
| `0x0200` / `0x0201` | Connection up / connection released |
| `0x0243` | Connection status (periodic) |
| `0x0203` | **Signalling segment** — carries the DECT C-plane inbound, one 5-byte Cs fragment per mail |
| `0x0244` | Append connection data — carries the C-plane outbound, a whole Lc frame per mail (not followed by `0x0202`, which drops the link) |
| `0x0230` → `0x0242` | Assign an audio channel, and the chip's confirmation |
| `0xF626`, `0x022D`, `0x0233` | Audio route, audio path on/off, release channel |
| `0x0208` | Call security material. **Not yet working** — see §5 |

### 3.2 The timing budget is tight, and it is the key design input

Inside `0x0203` rides the DECT C-plane, which runs a link-layer (LAPC) with its
own acknowledgement and retransmission. Measured on hardware:

- **LAPC retransmits an unacknowledged frame after ~1 s.**
- **The radio link is torn down after ~10 s** without progress.

So the round trip *cell → controller → cell* for one C-plane message must stay
comfortably below one second, and in practice should be tens of milliseconds.
A handset drops its call if the controller is slow, and there is no recovery
short of re-establishing the link.

Practical consequences for the protocol spec:

- One persistent, framed, bidirectional TCP connection per cell (already the
  decision) — not request/response over HTTP, and no unbounded queues.
- Control messages must not queue behind bulk transfers (firmware uploads,
  statistics dumps, log shipping). Either separate those onto their own stream
  or chunk them so a C-plane message never waits behind one.
- Head-of-line blocking matters. If you ever consider TLS record batching or
  Nagle, measure first; disable Nagle on the control connection.
- Reconnect back-off must not be so slow that a cell stays dark for minutes.

### 3.3 Bandwidth

- **Signalling:** the control channel is one 8 kHz TDM slot carrying two bytes
  per frame, so **~16 kB/s is the ceiling for all signalling on a cell**, shared
  by every handset on it. It is not a bottleneck, but it means the link protocol
  should not be chatty per-frame.
- **Voice:** one TDM slot per call, also two bytes per frame at 8000 frames/s,
  so **~16 kB/s per direction per call** leaving the chip. The encoding inside
  those two bytes is still being measured (§5), so treat the cell-link audio
  format as open.

### 3.4 Per-handset state the database must carry

From a completed registration on hardware, a handset record needs:

| Field | Notes |
|---|---|
| Portable identity (IPUI) | Variable length, as sent by the handset |
| TPUI | Assigned by us during registration |
| UAK | 16 bytes, long-term key. **Encrypted at rest** |
| Subscription slot index | See below — this is a *per-cell* resource |
| Model / capability info | From registration, drives the feature framework |

The derived cipher key (DCK) is per-call and derived fresh; never store it.

**The subscription slot is a scarce, cell-local resource.** A DECT chip holds
**six** subscription records (see `../opendectcell/docs/capacity.md` for why six, and
why it is a firmware table size rather than a proven hard limit). A handset is
only usable on a cell whose chip currently holds its record. For the multi-cell
design that has a consequence which `architecture.md` does not yet address:

> The controller must **allocate subscription slots per cell** and write a
> handset's record into whichever cell it is roaming to — and evict something
> when all six are taken.

Designing that allocation and eviction policy is part of task A1.

**Three capacity dimensions, not one** (from A-0032, 2026-10-19). Keep them in
separate fields from the start, because they are bounded by different things:

| per cell | value | status |
|---|---:|---|
| subscription records (identity + key storage) | 6 | STUB table size, not a proven silicon limit |
| concurrent radio connection contexts | 6 | reconstructed firmware; paging/registration can use some without a voice path |
| host audio endpoints configured by the stock 7520 | **4** | observed in the stock boot log (`DECT=4`); the cell protocol should let a cell *announce* this |
| validated concurrent two-way calls | **0** | nothing end-to-end yet; raise only after measured 2-, 3-, 4-call tests |

Do not put 6, 5 or 4 into the controller as a fixed fact. Model narrowband and
wideband capacity separately until measured.

## 4. The tasks

Each task names its deliverable and how you know it is done. Order within each
part is roughly dependency order; A1 and A2 are the ones everything else leans
on.

**Progress** (update this table when a task changes state; details are in
the maintainers' notes):

| Task | State | Where |
|---|---|---|
| A1 Cell protocol | **Done, awaiting review** | `docs/cell-protocol.md` (v1 draft); cell answers C1–C11 folded in 2026-10-22, remaining unknowns in its §13 |
| A2 libopendectcell integration | **Done, awaiting review** | `docs/libopendectcell-integration.md`; PoC in `internal/dect/cdect`; change requests for openDECTcell in its §11 |
| A3 Schema | **Done, awaiting review** | `docs/schema.md`, `internal/db/migrations/`. Question 3 answered with a proposal: many-to-many home set |
| A4 API spec | **Done, awaiting review** | `docs/api/openapi.yaml`, validated by `internal/api` tests |
| A5 Threat model | **Done, awaiting review** | `docs/threat-model.md` |
| A6 Frontend ADR | **Done, awaiting review** | `docs/adr/0001-web-frontend.md`: Go templates + htmx |
| A7 UI mock-ups | **Done, awaiting review** | `docs/ui/README.md` |
| B1 Module layout, lint, CI | **Done** | `.golangci.yml`, `.github/workflows/ci.yml`. The cgo package is built in CI via the `OPENDECTCELL_READ_SSH_KEY` deploy key (L7, resolved) |
| B2 Docker and compose | **Done** | `Dockerfile`, `docker-compose.yml`, `docs/deployment.md`; smoke-tested in CI |
| B3 Migrations and configuration | **Done** | `internal/db`, `internal/config`, `docs/configuration.md` |
| B4 Authentication | **Done** | `internal/auth` (local + OIDC), `internal/web` |
| B5 Audit log | **Done** | `internal/audit`; append-only by trigger |
| C5.1 Media facts | **Done** | `voip-requirements.md` §5, `cell-protocol.md` §8.2: per-direction formats; HELLO tag 16 and MEDIA_OPEN tag 13 asked in openDECTcell#82 |
| C5.2 Audio pipeline | **Done** (no hardware) | `internal/media`, `docs/media-engine.md` |
| C5.3 Call quality | **Done** (no call writes it yet: C3) | `media.Summarise`, `internal/calllog`, `/admin/calls` |
| C5.4 Wideband as a capability | **Done** | `media.Choose`, `CellCaps.Wideband` |
| C5.5 Regression corpus | **Done** | `internal/media/corpus_test.go`, its own CI step |
| C3.1 Golden-trace replay | **Reviewed 2026-10-31** (the maintainers' notes: `host.c` stays as is; its steps move into libopendectcell, S7) | `internal/dect/cdect/host.c` (one cell's `dect_base`), `trace_test.go`: all 15 openDECTcell traces replay; one known one-pass difference (worklog 2026-10-31, C3.1) |
| C3.2 One stack per cell | **Reviewed 2026-10-31**; the ownerless handset removed (`0c6715d`). ADRs 0003-0005 from the review await review (no hardware) | `internal/dect/core`, `internal/dect/stack`, `cell.CellWatcher`, migration 0003; on with `-tags libopendectcell` / `LIBOPENDECTCELL=1` (worklog 2026-10-31, C3.2) |

Per `AGENTS.md` §10, the next task is chosen by the maintainers.
*2026-10-22:* they did, in opendectcell-controller#46: C0 leftovers, C1, C4, C6/C7,
and C2 except the mail tunnel; the libopendectcell integration is frozen until
openDECTcell#67–#70 land.
*2026-10-23:* C5, the media engine (the maintainers' notes), done as above; the next
choice is the maintainers' again.

### Part A — C0 design

**A1. Cell↔controller wire protocol spec** → `docs/cell-protocol.md`

The single most valuable piece of work on this list. Specify:

- Framing: length-prefixed messages, a version/capability handshake, message
  type registry, maximum sizes, and how an unknown type is handled.
- Message types: tunnelled STUB mails (both directions), cell management
  (hello, version, config, heartbeat, status, statistics), media session setup
  including SRTP key exchange, and errors.
- Enrollment: one-time token plus CSR, certificate issuance, admin approval,
  renewal and revocation.
- mTLS: TLS 1.3, per-cell client certificates, which CA, how pinning works.
- Heartbeat and liveness: interval, timeout, and what happens to active calls
  when the control link drops (`architecture.md` §4 says a short grace period —
  put a number on it, justified against the ~10 s radio timeout in §3.2).
- Subscription slot allocation and eviction across cells (§3.4).
- Back-pressure and prioritisation, given §3.2.

*Done when:* someone could implement either side from the document alone, every
message has a defined size bound and error behaviour, and the latency budget is
stated explicitly with its justification.

**A2. `libopendectcell` integration design** → `docs/libopendectcell-integration.md`

`libopendectcell` is a C library of pure protocol state machines. Its shape is
unusually friendly to cgo, and the design should lean on that:

- **No I/O, no syscalls, no threads, no global state.** Each layer is a struct
  plus functions over it.
- The pattern throughout is *feed bytes in, pull queued messages out*:
  `dect_lc_rx_fragment()`, `dect_lapc_rx()`, `dect_mm_rx()`, `dect_cc_rx()` to
  feed; `dect_mm_poll()`, `dect_cc_poll()`, `dect_lapc_poll_tx()` to drain.
- **The caller supplies randomness and time.** For example `dect_mm_rx()` takes
  the random challenge values as parameters rather than calling a RNG itself.
  That keeps it testable and keeps policy on the Go side.
- Largest NWK message is 256 bytes; largest mail parameter block 249 bytes.

So the natural mapping is one goroutine per handset session owning one set of C
structs, with no C callbacks into Go and no C threads at all. Specify:

- Ownership and locking: one session's structs touched by one goroutine.
- How Go and C memory are kept apart (never hand C a Go pointer it retains;
  copy at the boundary).
- Where timers live (Go side) and how they drive the C state machines.
- How randomness is injected, and how tests inject deterministic values.
- Build: how the openDECTcell source is vendored or submoduled, and how CI builds it.
- Error and panic strategy across the boundary.

*Done when:* the boundary is specified tightly enough that C3 is mechanical, and
a trivial proof-of-concept compiles and calls one libopendectcell function from Go.

**A3. Database schema and migrations** → `docs/schema.md` plus SQL migrations

Turn `architecture.md` §3 into real PostgreSQL DDL. Include the subscription
slot allocation from §3.4, which the draft data model does not yet have. Decide
and document the migration tool. Note which columns are encrypted and how
(envelope encryption, key rotation).

*Done when:* migrations apply cleanly to an empty database and roll back, and
every entity in `architecture.md` §3 is represented or explicitly deferred.

**A4. REST/JSON API specification** → `docs/api/openapi.yaml`

Cover what the user UI and admin UI need: handsets, SIP accounts, line
assignment, registration windows, cells, enrollment tokens, call log, stats,
users and roles. Every endpoint states its required role and ownership check.

*Done when:* the spec validates, and each endpoint names its authorisation rule.

**A5. Threat model and secret handling** → `docs/threat-model.md`

Assets, actors, trust boundaries (handset↔cell radio, cell↔controller link,
controller↔PBX, browser↔controller). Key management for the UAK and SIP
passwords: where the master key comes from, rotation, what an attacker with a
database dump gets. Include the DECT-specific note that a cell on an untrusted
network sees the C-plane in the clear unless the link is TLS.

*Done when:* each boundary has a stated attacker capability and a mitigation.

**A6. Web frontend technology decision** → short ADR in `docs/`

Go templates + htmx versus an SPA. `architecture.md` §8 leaves it open and §7
says decide at implementation start — that is now. Decide, with reasons.

**A7. UI mock-ups** → `docs/ui/`

User flows: handset list, registration with the PIN window, line assignment,
dual-ring policy. Admin: cells, enrollment, stats, logs, policies. Wireframes
are enough; they exist to settle the API, not to be pretty.

### Part B — C1 skeleton

None of this touches DECT or SIP logic.

**B1. Go module layout, linting, tests, CI** — module path, package structure
matching the components in `architecture.md` §2, `golangci-lint`, `go test`,
GitHub Actions running build + lint + test on every push.

**B2. Docker image and compose** — multi-stage build producing a small image,
`docker-compose.yml` with PostgreSQL, healthchecks, and a documented one-command
local bring-up.

**B3. Migrations and configuration** — migrations wired into start-up or an
explicit command; configuration from environment and file, including the secret
key, with validation and clear failure messages on missing config.

**B4. Authentication** — local accounts plus OIDC, group→role mapping, users
created just-in-time on first OIDC login, sessions, CSRF, and server-side
authorisation helpers for role and ownership checks (`AGENTS.md` §5).

**B5. Audit log** — actor, action, object, timestamp; written for every
state-changing action; never contains secrets.

*Part B is done when:* `docker compose up` gives a running service with a
database, a working login against a local account and against a test OIDC
provider, an audit trail, and green CI.

## 5. What is blocked, and why

| Item | Blocked on | Note |
|---|---|---|
| **Audio format on the cell link** (`ROADMAP.md` C0, `architecture.md` §8) | A measurement in progress on the cell side | Two bytes per frame at 8000 frames/s is equally consistent with 16-bit linear at 8 kHz, A-law at 16 kHz, or A-law in one byte. Specify the cell-link media negotiation so the format is a *parameter*, and leave the default open. *Update 2026-10-19:* narrowband is 8-bit A-law in the low byte on bench loopback; a live call still passes no decoded audio (openDECTcell `docs/journals/call-g7.md`, v17). A1 makes the format negotiated (`cell-protocol.md` §8.2). *Update 2026-10-22, answered* (openDECTcell#64): wideband is the coded G.722 bearer at 8 000 octets/s, not linear; narrowband is A-law. Defaults set in §8.2. Host → handset audio is still defective on the cell side. *Update 2026-10-23* (openDECTcell notes): corrected. The narrowband downlink is L16 8 kHz, not A-law; the uplink is unmeasured; wideband is unproven. The format is now per direction (`cell-protocol.md` §8.2, openDECTcell#82) |
| **Mail tunnel semantics** (C2) | openDECTcell's cell mode, milestone M9 | `dect-stubd` currently runs standalone. The protocol spec (A1) can and should still define the tunnel; the implementation waits |
| **Call ciphering** | Unresolved on the cell side | Does not affect C0/C1. *Update 2026-10-19:* the handset drops unencrypted calls only when its "Abhörsicher" (secure) setting is on; with it off, they hold. The DCK derivation is verified on hardware, and the chip-side cipher enable (`0x0208`) is still unproven (openDECTcell `docs/journals/call-g7.md`) |
| **DECT system identity (ARI/RPN) for multi-cell** | openDECTcell research item M11 | Affects C9 only |

The sensible move for all four: design so the unknown is a configurable
parameter rather than a baked-in assumption.

## 6. Questions worth asking rather than guessing

Send these back rather than designing around them:

1. Is the ~10 s radio timeout in §3.2 acceptable as the hard upper bound for the
   control-link grace period, or should calls drop sooner and more cleanly?
2. Subscription slot eviction (§3.4): should a roaming handset evict the
   least-recently-seen record automatically, or should six-per-cell be a hard
   limit the admin manages explicitly?
3. Is the `handset` ↔ `cell` relationship many-to-many in the schema, or is a
   handset homed to one cell with roaming as an exception?
4. Does the cell link need to carry anything the current message list omits —
   for example log shipping, remote firmware update, or a debug channel?

*Status 2026-10-19:* still open for the maintainers. A1 contains proposals for
1 (a 7 s grace period, `cell-protocol.md` §6.4) and 2 (automatic eviction
with pins, plus a manual mode, §7.4), and reserves message types for 4 (§12).
Question 3 is untouched and is needed for A3.
*Update 2026-10-22:* the cell side's answers change the basis of 1 and 2 but
not the questions. For 1, the cell now releases calls at 7 s and parks the
radio at 60 s instead of resetting the chip. For 2, slot affinity is gone: the
TPUI is no longer tied to a slot (`cell-protocol.md` §7.4), so eviction no
longer has to keep a handset's index, and on-demand placement stays off until
openDECTcell#58 is tested.

## 7. How we work together

The maintainers review this work and decide what gets built next. They are not
watching your commits as they happen, so the written trail is the whole of what
they see. `AGENTS.md` §9 has the binding version; in short:

**Commit and push frequently.** Small, reviewable commits, pushed as you go.
Nothing of value should sit only on your machine. Imperative subject ≤72 chars,
then the *why*; never force-push.

**Keep the maintainers' notes current.** One entry per working session, newest
first, using the template at the top of that file. It is the review surface:
what changed and with which commits, what you decided and why, what is open,
and what did not work.

Three things reviewers need from every entry:

- **Decisions stated explicitly**, with the alternative you rejected. Anything
  a reviewer might reasonably overrule — a library, a schema shape, an
  interpretation of a design doc — belongs in the decisions list. A choice
  mentioned only in a commit message has not been surfaced.
- **Honest status.** Say when something is half-finished, skipped or did not
  work. An entry listing only successes cannot be reviewed.
- **Questions, not assumptions.** Put open questions under their own heading
  and mark whether they block you. If one does, work on something else
  meanwhile rather than guessing.

**Finish the given tasks, then stop and report.** Completing a milestone is the
point at which you hand back for review, not the point at which you choose the
next one.

**Ask early about the cell side.** It moves fast and its findings keep changing
the interface — this brief alone corrects two things the architecture draft
assumed. Keep A1 (`docs/cell-protocol.md`) as the living contract between the
two repos, and raise a question the moment a task depends on cell behaviour not
written down here. Guessing at the DECT side has already cost the cell project
several days, twice.
