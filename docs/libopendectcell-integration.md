# `libopendectcell` integration design (cgo boundary)

Status: draft, 2026-10-19 (task A2 in `docs/first-tasks.md`). Checked against
the `libopendectcell` headers at openDECTcell commit `88d61af`. The submodule is now
pinned at `ee72c4d` (API version 3, 2026-10-22): the proof of concept still
builds and passes (§10), and the delivered change requests are in §11. The
rest of the design is frozen with the integration (opendectcell-controller#46) and
has not been re-checked line by line against the new pin. The library
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
| LAPC (DLC class A) | `dect_lapc.h` | `dect_lapc_init`, `_parse`, `_rx`, `_send`, `_tick`, `_poll_tx` | Window 1, modulus 2; segments NWK messages up to 256 octets; retransmit after `retry_ms` (default 2000 since API 1; was 1000) |
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
| `internal/dect/cdect` | The **only** cgo package: one cell's `struct dect_base` and the host steps beside it (`host.c`), C memory, copies in and out | yes |
| `internal/dect/stack` | The types that cross between a stack and the controller (`Mail`, `Record`, `Env`, `Config`) and the `Stack` interface, without cgo | no |
| `internal/dect/core` | One stack per active cell: its goroutine, the radio bring-up, the records in the database, MAIL_UP in and MAIL_DOWN out | no |

No other package may import `"C"`. That keeps the boundary in one place for
review, for `cgocheck2`, and for API churn in `libopendectcell`. Only `cmd/` picks
the implementation: built with `-tags libopendectcell` (the submodule and cgo) it
hands `cdect.NewStack` to the core; without the tag the core is off and the
cells' mails are counted and dropped, as before C3, so everything else builds
and tests without a C compiler or the submodule.

## 3. One host per cell

*Rewritten 2026-10-31 (C3 step 2). Until then this section planned one
session per radio connection; ADR 0002 option B and the library's design
superseded that: `struct dect_base` owns all of one radio's links, records
and calls. The guide for a host is openDECTcell's
[`docs/hosting-dect-base.md`](https://github.com/openDECTcell/opendectcell/blob/main/docs/hosting-dect-base.md).*

### 3.1 What a host is

A **host is one cell's `struct dect_base`** (`cdect.Host`), with the host
steps `dect-stubd` keeps beside the library (`host.c`, each step naming the
`dect-stubd` function it follows). It holds the records of the handsets the
cell's chip serves (at most six, `cell-protocol.md` §7.4), every radio
connection on the cell (the main link and three side links), and their calls.
The golden traces pin its behaviour (§9).

| Event (`cell.CellWatcher`) | The core |
|---|---|
| `CellUp`, a fresh session, a new chip, or the cell made active | Builds a new host: the cell's records from the database (home-set placement, §7.4 rules 1–2), free stores for enrolment, the chip settings of the cell's STUB build; sends the radio bring-up (one mail per 100 ms, `0x015B 00` first on 06.88 or a radio that is on); then `RadioUp`, and the records go into the chip |
| `CellUp`, resumed (`cell-protocol.md` §6.4) | Keeps the host and its calls |
| `CellDown` | Keeps the host; mails fail until the cell is back |
| `CellGone` (grace over, or the cell no longer active) | Closes the host |

Persistent handset state lives in the database, not in C structs: IPUI, TPUI,
UAK (sealed), the call identities, the cell slots. The host gets it at
`CellUp` and hands back changes through `Env.Save`.

### 3.2 Goroutines

| Goroutine | Count | Does |
|---|---|---|
| Cell session | 1 per connected cell (`internal/cell`) | Decodes frames; hands MAIL_UP and MAIL_REJECT to the core without blocking |
| Cell writer | 1 per connected cell | The priority scheduler of `cell-protocol.md` §9 |
| Cell stack | 1 per cell seen (`internal/dect/core`) | Owns the host; the only goroutine that ever touches its C state. Ticks it every 10 ms |
| Record writer | 1 per host | Writes the host's record changes to the database in order, so a slow database never stalls the stack's timers |

The cell session never blocks on the core: each cell's inbox is a bounded
channel (1024 events); a full one drops the event and logs an error.

### 3.3 The cell loop

```go
for {
	select {
	case ev := <-r.in: // MAIL_UP, MAIL_REJECT, CellUp/Down/Gone
		r.handle(ev)    // MAIL_UP: stack.Mail(m, now)
	case <-tick.C: // 10 ms
		r.tick() // a bring-up mail when due, then stack.Tick(now)
	case <-ctx.Done():
		return
	}
}
```

A call into the host runs the stack and drains LAPC in the same C call; the
host's mails reach `cell.Server.SendMail` from inside it (`Env.Send`). The
acknowledgement for an incoming frame is therefore queued within the same
loop iteration, which keeps the controller's share of the latency budget
(`cell-protocol.md` §2, 10 ms p99) easy to meet.

## 4. Ownership and locking

- **No mutexes around C state.** A host is touched only by its cell's
  goroutine; other goroutines talk to it by channel. This relies on the
  library having no global state, which holds (§1).
- `cdect` handles are documented as not safe for concurrent use. The race
  detector cannot see C memory, so a debug build records the owning goroutine
  and panics if another one calls in (planned for C3).
- **Lifetime:** `New…()` allocates; `Close()` zeroes and frees, and is
  idempotent. The cell goroutine's `defer` always calls `Close`. A runtime cleanup
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
  `dect_lapc_next_deadline()` (§11, L3). openDECTcell agreed the shape
  `uint64_t dect_lapc_next_deadline(const struct dect_lapc *)`, returning
  `UINT64_MAX` when nothing is outstanding (openDECTcell#69). Until it lands, a
  coarse alternative is a 100 ms ticker per session that calls `dect_lapc_tick`
  only while an I-frame is outstanding. That is accurate enough against a 1 s
  timer, and needs no internals.
- **`retry_ms` will change** from 1000 to the 2 s that EN 300 175-4 specifies
  (openDECTcell Gap 3), and **more library timers are coming**: CC's mandatory
  `F<CC.03>` is not implemented yet (openDECTcell Gap 1), and openDECTcell intends to
  widen L3 to a stack-level "next deadline" then. So `session` arms **one**
  timer per session from whatever deadline `cdect` reports, and nothing in Go
  assumes LAPC is the only clock or that its period is constant.
- **Procedure supervision is Go's job**, because MM and CC have no timers: the
  registration window (`architecture.md` §5), a registration that stalls
  half-way, call setup, and release. These live in `session`, as ordinary
  `time.Timer`s on the same loop.

*2026-10-31:* the timer design above predates ADR 0002. `dect_base` has
one tick for all its timers (`dect_base_tick()`, openDECTcell API 100+), and
the core ticks each host every 10 ms (§3.3), the period `dect-stubd`'s
traces use. A deadline-driven timer is an optimisation for later.

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
| Go panic in a cell's goroutine | **Contained to the cell** (`21a8c66`). Each exported callback recovers a panic (a recover must not unwind through C frames), keeps the first as a `cdect.Fault` and returns a safe default (`send` → -1, `room`/`name_visible`/`clock` → 0); the `Host` method that called into C panics with it once C has returned. The cell's goroutine recovers, logs it with both stacks, closes the host and rebuilds the stack after the retry delay (5 s); the other cells carry on. The record writer logs a panicking job and goes on. Calls on that cell are lost |
| Crash inside C (SIGSEGV) | Can't be recovered in-process: the controller dies, and Docker's restart policy brings it back. Cells resume or reconnect (`cell-protocol.md` §6.4). Prevented rather than handled: length checks in Go before every call, fuzzing, sanitizer builds (§9, §10). **Not acceptable any more** (the maintainer, 2026-10-31): `adr/0003-stack-isolation.md` proposes a worker process per cell |

The library uses no `assert`/`abort` (checked at `88d61af`, rechecked at `ee72c4d`), so a crash means
memory corruption, not a deliberate stop.

**Superseded 2026-10-31:** the maintainer requires that no failure in one
cell's stack, a C crash included, stops the controller; the worker
process per cell is now the proposal (`adr/0003-stack-isolation.md`). The
original v1 reasoning, kept for the record: **alternative rejected for
v1:** running `libopendectcell` in a worker process per
cell, so that a C crash takes down one cell instead of the controller. It adds
an IPC hop to the latency-critical path, against a failure mode the library's
design (no allocation, no I/O, fixed bounds) makes unlikely. Revisit if crashes
are seen.

## 9. Testing

| Level | What | State |
|---|---|---|
| `cdect` unit tests | Round trips, LSIG learning, corrupt frames, bounds checked before C, use after close. Run under `cgocheck2` | **5 tests, passing** |
| Golden traces | openDECTcell's `tests/stubd-traces/` replayed through the host (`host.c`) with a fake clock and test_stubd.c's entropy; mails and bridge lines compared line for line (`trace_test.go`). Hardware captures: not yet | **15 of 15 traces, passing** (2026-10-31); dect-stubd-only lines skipped, one known difference, both listed in the test |
| Fuzzing | Go native fuzz targets feed arbitrary bytes into each `*_rx` path through `cdect` | C3; short run in CI, longer nightly |
| Core tests | `internal/dect/core`: bring-up order per STUB build, resume keeps the host, a fresh session rebuilds it, enrolment saves, name visibility, slot-write corr and MAIL_REJECT, retry after a failed load; and the core over the real host (`-tags libopendectcell`): a cell's start matches `register.trace`. The record store against PostgreSQL | **Passing** (2026-10-31; the database tests run in CI only, this machine had none) |

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
`src/libopendectcell/` carries that SPDX identifier at `88d61af`, rechecked at `ee72c4d`), so linking it into
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
| L3 ([opendectcell#69](https://github.com/openDECTcell/opendectcell/issues/69)) | ~~`dect_lapc_next_deadline()`~~ **delivered** (pin `ee72c4d`), with `UINT64_MAX` when idle. `DEFAULT_RETRY_MS` is now 2000 (`<DL.04>`). Also new: `dect_cc_tick()` / `dect_cc_next_deadline()` for CC's own timer, as §6 anticipated | `cdect` |
| L4 | ~~Internal pointers~~ **resolved:** none except the transient `dect_lapc_frame.info` (§5) | — |
| L5 | ~~`assert`/`abort`~~ **resolved:** none (§8) | — |
| L6 ([opendectcell#70](https://github.com/openDECTcell/opendectcell/issues/70)) | ~~Version macro~~ **delivered:** `OPENDECTCELL_API_VERSION` in `dect_proto.h`, now **3** (2: CC timer fields; 3: `dect_iwu.h`, and `struct dect_cc` embeds `struct dect_iwu`, so its layout changed: recompile, don't relink). `cdect.h` refuses any other version with `#error` | `cdect` |
| L7 | ~~The openDECTcell repository is private: CI needs read access~~ **resolved:** the openDECTcell repository is public, CI checks the submodule out like any other (an earlier version used a read-only deploy key) | B1 |
| L8 | ~~Connection identifier~~ **resolved:** `handle` + 3-byte `tag` from `0x0203` (§3.1) | — |
| L9 ([opendectcell#67](https://github.com/openDECTcell/opendectcell/issues/67)) | **Change request:** move the stack glue (§1.1) into `libopendectcell`, so `dect-stubd` and the controller share it instead of keeping two copies. **Accepted** (2026-10-22) in the proposed shape (`struct dect_link` plus a stack object with `rx_segment` / `tick` / `poll_append_mail`); no date. It is the last blocker for the #46 thaw. Scope: `reg_segment()`, `drain_nwk()`, `pump_lapc()` only. The audio bind stays with the controller (`cell-protocol.md` §8.1), which is what we assumed | `cdect`, openDECTcell |
| L10 ([opendectcell#68](https://github.com/openDECTcell/opendectcell/issues/68)) | ~~TPUI as a parameter~~ **delivered:** `struct dect_mm_cfg` takes `tpui[3]` and `sub_slot`, decoupled; all-zero TPUI falls back to `{0x00, 0x11, sub_slot}`. Accessors `dect_mm_tpui()`, `dect_mm_sub_slot()`. **Trap:** a page (`0x0300`) must take its octets from `dect_mm_tpui()`, never from the slot index | `core` |
| L11 ([opendectcell#71](https://github.com/openDECTcell/opendectcell/issues/71)) | ~~`0x0202` comment~~ **delivered** (`4557505`): `dect_cs.h` says `0x0244` is self-flushing; `dect_cs_trigger_mail()` is deprecated | — |
