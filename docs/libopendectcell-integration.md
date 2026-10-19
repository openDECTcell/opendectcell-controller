# `libopendectcell` integration design (cgo boundary)

Status: draft, 2026-10-19 (task A2 in `docs/first-tasks.md`). Checked against
the `libopendectcell` headers at openDECTcell commit `88d61af`, the commit pinned in
`third_party/opendectcell`. A proof of concept builds and passes (§10). The library
API is still changing (the maintainers' notes, 2026-10-19), so the design depends on its
*shape*, and every exact signature stays inside one package.

## 1. What the library gives us

All of `src/libopendectcell/` is GPL-2.0-or-later, C11, with no I/O, threads,
global state, `assert` or `abort`, and no internal allocation. Each layer is a
flat struct plus functions over it.

| Layer | Header | Functions the controller uses | Notes |
|---|---|---|---|
| Cs ↔ STUB mails | `dect_cs.h` | `dect_cs_decode_segment`, `dect_cs_append_mail` | Inbound: one 5-octet fragment per `0x0203`, with `handle` and 3-byte `tag`. Outbound: one `0x0244`. Do **not** use `dect_cs_trigger_mail` (`0x0202`): on hardware it drops the link (`cell-protocol.md` §7.2) |
| Lc (DLC lower) | `dect_lc.h` | `dect_lc_rx_init`, `dect_lc_rx_fragment`, `dect_lc_build` | Reassembly + checksum; learns the link signature (LSIG) from the first frame |
| LAPC (DLC class A) | `dect_lapc.h` | `dect_lapc_init`, `_parse`, `_rx`, `_send`, `_tick`, `_poll_tx` | Window 1, modulus 2; segments NWK messages up to 256 octets; retransmit after `retry_ms` (default 1000) |
| NWK codec | `dect_nwk.h` | `dect_nwk_parse` | Routes by protocol discriminator (MM vs CC) |
| MM | `dect_mm.h` | `dect_mm_init`, `_rx`, `_start_cipher`, `_cipher_dck`, `_poll` | Registration (access rights, key allocation, DSAA), locate, call-time authentication → DCK |
| CC | `dect_cc.h` | `dect_cc_init`, `_rx`, `_release`, `_poll` | Minimal FP side of an outgoing call (G7 milestone A) |
| DSAA | `dect_dsaa.h` | (used inside MM) | A11/A12/A21/A22 |
| Mail framing, STUB session | `dect_mail.h`, `dect_session.h` | **not used** by the controller | These end at the cell (`dect-stubd`); the controller sees decoded mails (`cell-protocol.md` §7.1) |

**Time:** every function that needs it takes `uint64_t now_ms`, a monotonic
millisecond clock chosen by the caller. Only LAPC has a timer
(`dect_lapc_tick`). MM and CC have **no timers**, so procedure supervision
(registration timeout, call-setup timeout) is the controller's job (§6).

**Randomness:** `dect_mm_rx(mm, msg, len, rand_f[8], rs[16])` and
`dect_mm_start_cipher(mm, uak, rand_f, rs)` take the challenge values as
parameters. They are used only when the call actually produces a challenge.

**Sizes:** a NWK message is ≤ 256 octets (`DECT_LAPC_MSG_MAX`), a LAPC frame ≤ 66
(`DECT_LC_LAPC_MAX`), an Lc frame ≤ 70 (`DECT_LC_FRAME_MAX`), and a mail's
parameter block ≤ 249 (`DECT_MAIL_PARAM_MAX`). Every buffer at the boundary is
fixed-size.

### 1.1 The layers are not glued together

The library provides the layers but **not the stack**. `dect-stubd` wires them
together itself, for a single connection (`reg_segment()`, `drain_nwk()`,
`pump_lapc()` in `src/stubd/dect-stubd.c`):

```
0x0203 mail → cs_decode_segment → lc_rx_fragment → lapc_parse → lapc_rx
   → (LAPC acks: lapc_poll_tx → lc_build → cs_append_mail → 0x0244)
   → NWK message → nwk_parse: PD = CC → cc_rx, else → mm_rx(rand_f, rs)
   → while no I-frame is outstanding: mm_poll, then cc_poll → lapc_send → pump
```

That glue embodies protocol behaviour: the order of MM before CC on drain, one
NWK message per LAPC window, LSIG learning, and when to write the subscription.
The controller needs exactly the same glue. Two copies that drift apart would
be a fork in all but name (`AGENTS.md` §2.3), so the plan is:

- **Preferred:** move the glue into `libopendectcell` as a `dect_link` (Lc + LAPC)
  and, later, a per-connection stack, so `dect-stubd` and the controller share
  one implementation. This is a change request to openDECTcell (§11, L9).
- **Until then:** the controller keeps its glue in **C**, in `cdect.c`, written
  from the same docs and kept close to `dect-stubd`'s version. C rather than Go
  for a concrete reason: `struct dect_lapc_frame` holds a pointer into the
  receive buffer, so `lapc_parse` and `lapc_rx` must run in the same C call
  anyway. That makes one cgo call per inbound mail.

## 2. Packages

| Package | Role | Imports `"C"` |
|---|---|---|
| `third_party/opendectcell` | git submodule of openDECTcell (relative URL `../opendectcell.git`), pinned to a commit | — |
| `internal/dect/cdect` | The **only** cgo package: owns C memory, the glue of §1.1, copies bytes in and out, maps return codes to Go errors | yes |
| `internal/dect/session` | One goroutine per radio connection: owns a `cdect` handle, timers, entropy; turns mails into events | no |
| `internal/dect/core` | Session registry, dispatch from cells to sessions, registration window, subscription slots, link to the database and the SIP side | no |

No other package may import `"C"`. That keeps the boundary in one place for
review, for `cgocheck2`, and for API churn in `libopendectcell`. `cdect` exists
now (the proof of concept); the other two are names for C3.

## 3. Sessions and goroutines

### 3.1 What a session is

A **session is one radio connection of one handset on one cell**, identified
by the cell and the chip's connection `handle` (0..6) plus the 3-byte `tag` from
`0x0203`. It is created at connection-up (`0x0200`) and ends at connection
release (`0x0201`), on a protocol error, or when the cell's grace period
expires (`cell-protocol.md` §6.4). It owns one Lc, one LAPC, one MM and one CC
struct.

The brief says "one goroutine per handset session". A per-connection session
amounts to the same thing, since a handset has at most one radio connection at
a time (handover is C9 research), but it starts *before* the handset has
identified itself: the first NWK message names the IPUI. Once it does, the
session binds to the handset's database record. If the handset already has a
live session on another cell, the **newer one wins** and the old one is closed.

Persistent handset state lives in the database, not in C structs: IPUI, TPUI,
UAK, capabilities, last cell. A session loads what it needs when it binds (for
example the UAK for `dect_mm_start_cipher`). It hands results back to `core`:
a new UAK after registration, the handset's identity, the DCK for `0x0208`.
This replaces what `dect-stubd` does with its local subscription file.

### 3.2 Goroutines

| Goroutine | Count | Does |
|---|---|---|
| Cell reader | 1 per cell | Decodes frames. Routes `0x0203` MAIL_UP to a session inbox by `(handle, tag)`, `0x0200`/`0x0201` to session creation and teardown, everything else to the cell manager (radio, registration window, audio confirms, status) |
| Cell writer | 1 per cell | The priority scheduler of `cell-protocol.md` §9 |
| Session | 1 per radio connection | Owns all C state of the session; the only goroutine that ever touches it |

The cell reader never blocks on a session. Each session inbox is a bounded
channel (32 mails); a full inbox resets that session (`cell-protocol.md`
§9.2) instead of stalling the other handsets on the cell. At the design target
(10 cells, 50 handsets, at most 7 connections per chip) this is ≤ ~90
goroutines.

### 3.3 The session loop

```go
// Illustrative; cdect method names are the planned API, not final.
func (s *Session) run(ctx context.Context) {
	defer s.close() // zeroes and frees C memory, releases the radio link
	defer s.recoverPanic()
	for {
		select {
		case m := <-s.inbox: // 0x0203 for this connection
			s.stack.Segment(m.Param, s.clock.NowMs(), s.entropy)
		case <-s.timer.C:
			s.stack.Tick(s.clock.NowMs())
		case cmd := <-s.cmds: // from core: release, start cipher, ...
			s.apply(cmd)
		case <-ctx.Done():
			return
		}
		s.flush()      // 0x0244 mails out to the cell writer; events to core
		s.rearmTimer() // §6
	}
}
```

Each call into `cdect` feeds the input and drains LAPC in the same C call, the
way `reg_segment()` does. The acknowledgement for an incoming frame is
therefore on its way to the cell writer within the same loop iteration. That
keeps the controller's share of the latency budget (`cell-protocol.md` §2,
10 ms p99) easy to meet.

## 4. Ownership and locking

- **No mutexes around C state.** A session's structs are touched only by its
  goroutine; other goroutines talk to it by channel. This relies on the
  library having no global state, which holds (§1).
- `cdect` handles are documented as not safe for concurrent use. The race
  detector cannot see C memory, so a debug build records the owning goroutine
  and panics if another one calls in (planned for C3).
- **Lifetime:** `New…()` allocates; `Close()` zeroes and frees, and is
  idempotent. The session's `defer` always calls `Close`. A runtime cleanup
  (`runtime.AddCleanup`) that logs a leak is a safety net, never the normal
  path.

## 5. Memory across the boundary

The layer structs contain only scalars and fixed arrays: `dect_lc_rx`,
`dect_lapc`, `dect_mm` and `dect_cc` hold no pointers, and the layers don't
point at each other. The single exception is `struct dect_lapc_frame`, whose
`info` points into the buffer it was parsed from, and which lives only for one
call. Go-allocated structs would therefore be *legal* under the cgo rules. The
design still keeps everything in C memory:

1. **All `libopendectcell` structs and I/O buffers live in one `C.calloc`ed wrapper
   struct per session** (in the PoC, `struct cdect_lc`). Go holds a single C
   pointer.
2. **Bytes in are copied** into a fixed C input buffer inside that struct, after
   a length check in Go; nothing oversized reaches C.
3. **Bytes out** are written by C into fixed C output buffers and copied out
   with `C.GoBytes` before the next call.
4. **No Go pointer ever reaches C.** Tests run with `GOEXPERIMENT=cgocheck2`.

Why C memory when Go memory would be allowed: the structs hold key material
(below), and C memory is the only place where "zero it, then free it" is under
our control. Go's garbage collector and stack growth may leave copies behind
that we cannot wipe. It also makes `dect_lapc_frame`'s internal pointer a
non-issue, and gives one rule with no exceptions. The copies cost a few hundred
bytes per mail at ≤ ~16 kB/s per cell, which is negligible.

**Secrets in C memory.** `struct dect_mm` holds the PIN (`cfg.pin`), the UAK
(`uak`, `cipher_uak`), the DCK (`dck`), the challenges (`rand_f`, `rs`) and
`cipher_res`. `Close()` wipes the whole wrapper struct with a volatile loop
before `free` (`cdect.c`, `cdect_wipe`). On the Go side, a decrypted UAK is a
`[]byte` that is zeroed right after it has been copied into C. That is best
effort, the same limit as for any Go secret, and `docs/threat-model.md` (A5)
will record it.

## 6. Time and timers

- **Timers live in Go.** C never sleeps and never reads a clock.
- The clock is an interface returning `uint64` monotonic milliseconds, matching
  the library's `now_ms`. Production uses `time.Since(start)`; tests use a fake
  clock that advances only when the test says so.
- **LAPC is the only library timer.** Its deadline is
  `last_tx_ms + retry_ms` while `outstanding` is set. The library has no
  "next deadline" function, so `cdect` derives the deadline from those two
  struct fields. That reads library internals, so it is a change request:
  `dect_lapc_next_deadline()` (§11, L3). Until then, a coarse alternative is a
  100 ms ticker per session that calls `dect_lapc_tick` only while an I-frame
  is outstanding. That is accurate enough against a 1 s timer, and needs no
  internals.
- **Procedure supervision is Go's job**, because MM and CC have no timers: the
  registration window (`architecture.md` §5), a registration that stalls
  half-way, call setup, and release. These live in `session`, as ordinary
  `time.Timer`s on the same loop.

## 7. Randomness

- Entropy is an interface `Read(p []byte)`. Production uses `crypto/rand`;
  tests use a seeded deterministic generator, or values recorded in a golden
  trace.
- `cdect` draws `rand_f` (8 bytes) and `rs` (16 bytes) **for every MM message**
  passed to `dect_mm_rx`, as `dect-stubd` does, because the caller cannot know
  in advance whether the message triggers a challenge. The values go into the C
  struct's input area, are wiped after the call, and are never logged.
- Because time and randomness are both injected, **a session is a pure
  function of its inputs.** Replaying a trace with the same clock steps and
  entropy reproduces its output byte for byte (§9).

## 8. Errors and panics

The library reports errors in three ways: `-1` or `0` returns (`lc`, `lapc`,
`cs`, `nwk`), `void` functions with statistics counters (`mm_rx`, `cc_rx`), and
state fields (`mm.state == DECT_MM_FAILED`).

| Failure | Handling |
|---|---|
| A return code reports bad input (Lc checksum, LAPC parse, oversized message) | Typed Go error from `cdect` (`ErrDropped`, `ErrLength`, …). Usually: drop the input; LAPC retransmits |
| MM or CC enters a failure state, or a stats counter shows a reject | `cdect` reports a state change as an event. `session` decides: release the connection, tell the UI the registration failed |
| `dect_lapc_send` returns `-1` (window busy) | Not an error: the message stays queued in MM/CC and goes out on the next drain, as in `drain_nwk()` |
| Go panic in a session | `recoverPanic` logs it (no key, PIN or identity bytes), closes the stack (which wipes and frees C memory), asks the cell to release the connection (`0x017A`), and ends the session. Other sessions carry on |
| Crash inside C (SIGSEGV) | Can't be recovered in-process: the controller dies, and Docker's restart policy brings it back. Cells resume or reconnect (`cell-protocol.md` §6.4). Prevented rather than handled: length checks in Go before every call, fuzzing, sanitizer builds (§9, §10) |

The library uses no `assert`/`abort` (checked at `88d61af`), so a crash means
memory corruption, not a deliberate stop.

**Alternative rejected for v1:** running `libopendectcell` in a worker process per
cell, so that a C crash takes down one cell instead of the controller. It adds
an IPC hop to the latency-critical path, against a failure mode the library's
design (no allocation, no I/O, fixed bounds) makes unlikely. Revisit if crashes
are seen.

## 9. Testing

| Level | What | State |
|---|---|---|
| `cdect` unit tests | Round trips, LSIG learning, corrupt frames, bounds checked before C, use after close. Run under `cgocheck2` | **5 tests, passing** |
| Golden traces | openDECTcell's tests and hardware captures (redacted) replayed through a session with the fake clock and recorded entropy; outgoing mails compared byte for byte | C3 |
| Fuzzing | Go native fuzz targets feed arbitrary bytes into each `*_rx` path through `cdect` | C3; short run in CI, longer nightly |
| Session tests | Inbox overflow resets the session; a panic is contained; `Close` wipes | C3 |

## 10. Build

**Source:** openDECTcell as a **git submodule** at `third_party/opendectcell`, URL
`../opendectcell.git` (relative, so it works over SSH and HTTPS), pinned to a commit
and bumped by an explicit commit here.

| Option | Verdict |
|---|---|
| Submodule, compiled by cgo | **Chosen.** `go build` compiles it, and the version is pinned and visible in review |
| Copying the sources into this repo | Rejected: in practice a fork (`AGENTS.md` §2.3) |
| Prebuilt static library linked through `#cgo LDFLAGS` | Rejected: an extra build step and harder cross-compiles. Still possible if openDECTcell ever ships one |

**How cgo compiles it:** cgo only compiles C files in the package directory, so
`internal/dect/cdect` has one wrapper file per library source, for example
`lc_unit.c`:

```c
// SPDX-License-Identifier: AGPL-3.0-or-later
#include "../../../third_party/opendectcell/src/libopendectcell/dect_lc.c"
```

The library's own `#include "dect_lc.h"` resolves relative to the included
file, and `cdect.go` adds the header path:

```go
// #cgo CFLAGS: -std=c11 -O2 -Wall -Wextra -Werror -I${SRCDIR}/../../../third_party/opendectcell/src/libopendectcell
```

The PoC compiles `dect_lc.c` and `dect_lapc.c`. C3 adds `dect_cs.c`,
`dect_mail.c` (for `struct dect_mail`), `dect_nwk.c`, `dect_mm.c`, `dect_cc.c`
and `dect_dsaa.c`. The library builds cleanly under `-Wall -Wextra -Werror`
with clang.

**Proof of concept** (A2 acceptance): `internal/dect/cdect` wraps the Lc
receiver and the LAPC RR builder. It builds a frame in C, wraps it with
`dect_lc_build`, feeds it back fragment by fragment through
`dect_lc_rx_fragment`, and checks the round trip, LSIG learning (`0x1101`, the
value seen on hardware) and checksum rejection. Verified with Go 1.27.2 and
`zig cc` 0.17.0 as the C compiler:

```
CGO_ENABLED=1 CC="zig cc" GOEXPERIMENT=cgocheck2 go test ./...
ok  github.com/openDECTcell/opendectcell-controller/internal/dect/cdect
```

`zig cc` was used only because this machine has no system C compiler. CI
should use the distribution's gcc.

**CI** (part of B1): check out with submodules (the openDECTcell repo is private,
so the CI token needs read access to it), then build, vet, lint and test with
`CGO_ENABLED=1`. A second job builds the C side with
`-fsanitize=address,undefined`, which is openDECTcell's own `make check` setting.
**Not yet verified:** a UBSan build with `zig cc` failed at link time because Go's
external linker didn't find zig's sanitizer runtime. That is a toolchain issue,
and it needs checking with gcc/clang in CI.

**Docker** (B2): cgo rules out a `scratch` image with a pure-Go static binary.
Either build on Debian and run on a distroless glibc base, or build fully
static against musl on Alpine. B2 decides.

**Licence:** `libopendectcell` is GPL-2.0-or-later (every file in
`src/libopendectcell/` carries that SPDX identifier at `88d61af`), so linking it into
this AGPL-3.0-or-later program is allowed, and the combined binary is
distributed under AGPL-3.0-or-later. The kernel module in the same submodule is
GPL-2.0-only, but it is never compiled here; only `src/libopendectcell/` is. B1
should add a CI check that fails if a file the controller compiles stops being
`GPL-2.0-or-later`, plus a `NOTICE` naming the pinned commit.

## 11. Open items and change requests to openDECTcell

| # | Item | Affects |
|---|---|---|
| L1 | ~~Proof of concept~~ **done** (§10) | — |
| L2 | ~~Real signatures~~ **resolved** from the headers (§1) | — |
| L3 ([opendectcell#69](https://github.com/openDECTcell/opendectcell/issues/69)) | **Change request:** `dect_lapc_next_deadline()`, so the controller doesn't read `last_tx_ms`/`retry_ms` directly (§6) | `cdect` |
| L4 | ~~Internal pointers~~ **resolved:** none except the transient `dect_lapc_frame.info` (§5) | — |
| L5 | ~~`assert`/`abort`~~ **resolved:** none (§8) | — |
| L6 ([opendectcell#70](https://github.com/openDECTcell/opendectcell/issues/70)) | No version macro or function. **Change request:** `OPENDECTCELL_API_VERSION`, so `cdect` can refuse a submodule bump it wasn't written for | `cdect` |
| L7 | The openDECTcell repository is private: CI needs read access (deploy key or token) | B1 |
| L8 | ~~Connection identifier~~ **resolved:** `handle` + 3-byte `tag` from `0x0203` (§3.1) | — |
| L9 ([opendectcell#67](https://github.com/openDECTcell/opendectcell/issues/67)) | **Change request:** move the stack glue (§1.1) into `libopendectcell`, so `dect-stubd` and the controller share it instead of keeping two copies | `cdect`, openDECTcell |
| L10 ([opendectcell#68](https://github.com/openDECTcell/opendectcell/issues/68)) | **Change request:** the MM layer hard-codes subscription slot 1 (`DECT_MM_REG_SLOT`) in the TPUI it assigns. The controller allocates slots (`cell-protocol.md` §7.4), so the slot must be part of `struct dect_mm_cfg` | `core`, openDECTcell |
| L11 ([opendectcell#71](https://github.com/openDECTcell/opendectcell/issues/71)) | `dect_cs.h` says to flush with `0x0202`, but `dect-stubd` found that `0x0202` drops the connection and sends `0x0244` alone. The header comment should be corrected upstream | openDECTcell docs |
