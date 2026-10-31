# Roadmap: opendectcell-controller

- **Timing:** design work (C0) can happen now. **Implementation (C1 onward) starts only after openDECTcell gate G7** (a handset rings and audio passes via the on-box test tool). See the openDECTcell ROADMAP for the gates.
- **Tracking:** each `## C…` section is a GitHub milestone, and each checkbox an issue. Items marked **[hw]** need real cells and handsets.

**Joining the project?** Start with [docs/first-tasks.md](docs/first-tasks.md):
the C0 and C1 items below, in dependency order, with what the cell side has
measured so far and what is still unknown.

## C0: Design (now)
- [x] Architecture draft, VoIP requirements, DECT feature framework (`docs/`)
- [x] Core decisions: Go, Docker + PostgreSQL, multi-cell only, roles/OIDC, 0–2 lines per handset
- [x] Cell↔controller wire protocol spec: framing, message types, enrollment, mTLS, heartbeat, SRTP key exchange (`docs/cell-protocol.md`, v1 draft; pending review)
- [x] Database schema (SQL) and migration strategy (`docs/schema.md`; pending review)
- [x] REST/JSON API specification (OpenAPI) for web UI and admin (`docs/api/openapi.yaml`; pending review)
- [x] Threat model and secret handling (key management for UAK/DCK and SIP passwords) (`docs/threat-model.md`; pending review)
- [x] UI mock-ups: user (handsets, lines, registration), admin (cells, stats, logs, policies) (`docs/ui/`; pending review)
- [x] Web frontend technology decision: Go templates + htmx (`docs/adr/0001-web-frontend.md`; pending review)
- [x] Audio format on the cell link — a per-direction, per-cell parameter. Downlink narrowband is L16 at 8 kHz (measured 2026-10-23); the uplink is not yet measured; wideband is unproven on current cells (`docs/cell-protocol.md` §8.2)
- [x] `libopendectcell` integration design (cgo boundary, threading, timers, transport injection) — `docs/libopendectcell-integration.md`, proof of concept in `internal/dect/cdect`; pending review
- [ ] Subscription-slot allocation across cells (six records per chip; a handset is only usable on a cell holding its record) — proposal in `docs/cell-protocol.md` §7.4, policy pending decision

## C1: Skeleton (G7 passed)
- [x] Go module layout, linting, unit-test setup, CI (GitHub Actions)
- [x] Docker image and docker-compose with PostgreSQL
- [x] Database migrations and configuration handling (env/file, secret key)
- [x] Authentication: local accounts plus OIDC (Authentik), group→role mapping, users created on first login
- [x] Audit log

## C2: Cell manager
- [x] Enrollment: one-time tokens, CSR, certificate issuing, admin approval
- [x] mTLS control channel, heartbeat, reconnect, cell status (CA rotation and expiry warnings still open)
- [ ] Mail tunnel to/from `dect-stubd` (cell mode in openDECTcell M9)
- [ ] **Diagnostics transport**: carry the cell's `dect_diag` (per handset: identity, subscription slot and whether the chip really holds it, in range / connected, model, field strength, signal quality, slot type, time slot, carrier, codec, encryption; per base: RFPI, radio state, dummy-bearer slot, carrier, power). `docs/cell-protocol.md` has no diagnostics message today and needs one. See `../opendectcell/docs/diagnostics.md` for the field list and which parts are blocked upstream.
- [ ] **[hw]** One cell connected and reporting status

## C3: DECT core
- [ ] `libopendectcell` via cgo: per-handset sessions, timers, dispatch
- [ ] Registration window with PIN; handset record (IPUI, keys encrypted, capabilities, model identifier)
- [ ] Locate/idle tracking (last cell seen)
- [ ] **[hw]** A handset registers through the controller

## C4: SIP user agents
- [x] One UA per handset line (0–2), registration incl. SRV/NAPTR, outbound proxy, NAT keep-alive (per account; binding to handset lines waits for C3)
- [ ] Calls in/out, early media, hold/resume, digit collection, DTMF modes (done except digit collection and in-band DTMF; Asterisk and FreeSWITCH interop in CI)
- [x] SIP over TLS; SRTP (SDES); per-account encryption policy (TLS calls not yet interop-tested)
- [ ] Dual-ring policy (call waiting vs busy) and outgoing line selection (CAT-iq line id or prefix)

## C5: Media engine
- [x] Cell audio ↔ RTP bridge, jitter buffer, PLC (`internal/media`, `docs/media-engine.md`; not yet driven by C3)
- [x] G.711 and G.722 transcoding; SRTP on the cell link; wideband as a capability the cell reports
- [x] Call quality statistics (RTCP), per leg, in `call_log` and on `/admin/calls`
- [ ] **Join the DECT-side quality to the RTP-side quality.** RTCP covers controller↔PBX; it says nothing about the air. A call that sounded bad because the handset was at the edge of range looks identical in RTCP to one that sounded bad because of packet loss, and the two have opposite fixes. Needs the C2 diagnostics transport.
- [x] Regression corpus in CI
- [ ] **[hw]** End-to-end call: handset ↔ cell ↔ controller ↔ PBX (needs C3)

## C6: Web UI (user)
- [ ] Handset list and registration flow
- [ ] SIP account management and line assignment (0–2), dual-ring policy, default line
- [ ] Shared handsets (admin-owned)

## C7: Admin UI
- [ ] Cells: status, enrollment, policy "who may add cells"
- [ ] Statistics, call log, logs
- [ ] User and role management
- [ ] **DECT diagnostics across cells.** The single-cell view is the LuCI page in openDECTcell M9; this is what one cell cannot see:
  - **which cell a handset is on**, and its field strength there — the input to any future roaming decision (C9)
  - **history**: per-call quality in `call_log`, so "it was bad last Tuesday" is answerable rather than a shrug
  - **alerting**: a handset whose subscription audit fails on its cell, or that has not been seen for *n* hours, is a support call waiting to happen. Both are things the cell knows and nobody currently asks it
  - rationale and field list: `../opendectcell/docs/diagnostics.md`

## C8: Release 1.0
- [ ] Interop matrix: Asterisk, FreeSWITCH, 3CX, FRITZ!Box registrar, one public provider
- [ ] Documentation (install, operations, security)
- [ ] Load test at the design target (10 cells / 50 handsets)

## C9: Multi-cell
- [ ] Idle roaming across cells with a shared registration database. Investigation, preconditions and work list: `docs/roaming.md` (2026-11-01): handsets choose their cell themselves; the controller's job is shared identity, records on every cell, tracking and routing
- [ ] System identity (ARI/RPN) assignment, based on the openDECTcell M11 research
- [ ] Research: seamless handover (depends on inter-cell sync)

## C10: Feature modules (later; see `docs/dect-features.md`)
- [ ] Feature-module framework and model-quirks table
- [ ] Date/time module
- [ ] Missed-call notification
- [ ] Message waiting (from SIP MWI)
