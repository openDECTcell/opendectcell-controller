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
- [x] Audio format on the cell link — settled by openDECTcell G7: coded G.722 (wideband) or A-law (narrowband), not linear (`docs/cell-protocol.md` §8.2; pending review)
- [x] `libopendectcell` integration design (cgo boundary, threading, timers, transport injection) — `docs/libopendectcell-integration.md`, proof of concept in `internal/dect/cdect`; pending review
- [ ] Subscription-slot allocation across cells (six records per chip; a handset is only usable on a cell holding its record) — proposal in `docs/cell-protocol.md` §7.4, policy pending decision

## C1: Skeleton (G7 passed)
- [x] Go module layout, linting, unit-test setup, CI (GitHub Actions)
- [x] Docker image and docker-compose with PostgreSQL
- [x] Database migrations and configuration handling (env/file, secret key)
- [x] Authentication: local accounts plus OIDC (Authentik), group→role mapping, users created on first login
- [x] Audit log

## C2: Cell manager
- [ ] Enrollment: one-time tokens, CSR, certificate issuing, admin approval
- [ ] mTLS control channel, heartbeat, reconnect, cell status
- [ ] Mail tunnel to/from `dect-stubd` (cell mode in openDECTcell M9)
- [ ] **[hw]** One cell connected and reporting status

## C3: DECT core
- [ ] `libopendectcell` via cgo: per-handset sessions, timers, dispatch
- [ ] Registration window with PIN; handset record (IPUI, keys encrypted, capabilities, model identifier)
- [ ] Locate/idle tracking (last cell seen)
- [ ] **[hw]** A handset registers through the controller

## C4: SIP user agents
- [ ] One UA per handset line (0–2), registration incl. SRV/NAPTR, outbound proxy, NAT keep-alive
- [ ] Calls in/out, early media, hold/resume, digit collection, DTMF modes
- [ ] SIP over TLS; SRTP (SDES); per-account encryption policy
- [ ] Dual-ring policy (call waiting vs busy) and outgoing line selection (CAT-iq line id or prefix)

## C5: Media engine
- [ ] Cell audio ↔ RTP bridge, jitter buffer, PLC
- [ ] G.711 and G.722 transcoding; SRTP on the cell link
- [ ] Call quality statistics (RTCP)
- [ ] **[hw]** End-to-end call: handset ↔ cell ↔ controller ↔ PBX

## C6: Web UI (user)
- [ ] Handset list and registration flow
- [ ] SIP account management and line assignment (0–2), dual-ring policy, default line
- [ ] Shared handsets (admin-owned)

## C7: Admin UI
- [ ] Cells: status, enrollment, policy "who may add cells"
- [ ] Statistics, call log, logs
- [ ] User and role management

## C8: Release 1.0
- [ ] Interop matrix: Asterisk, FreeSWITCH, 3CX, FRITZ!Box registrar, one public provider
- [ ] Documentation (install, operations, security)
- [ ] Load test at the design target (10 cells / 50 handsets)

## C9: Multi-cell
- [ ] Idle roaming across cells with a shared registration database
- [ ] System identity (ARI/RPN) assignment, based on the openDECTcell M11 research
- [ ] Research: seamless handover (depends on inter-cell sync)

## C10: Feature modules (later; see `docs/dect-features.md`)
- [ ] Feature-module framework and model-quirks table
- [ ] Date/time module
- [ ] Missed-call notification
- [ ] Message waiting (from SIP MWI)
