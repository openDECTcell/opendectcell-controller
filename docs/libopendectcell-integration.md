# `libopendectcell` integration design (cgo boundary)

Status: draft, 2026-10-19 (task A2 in `docs/first-tasks.md`).

**Basis and limits.** This design rests only on what `first-tasks.md` §4 (A2)
says about `libopendectcell`. The openDECTcell repository was not available while it was
written, so no header was read. Function names below come from the brief.
**Signatures, struct names and the timer interface are assumptions**, marked
**[api?]** and collected in §11. The library's API is also still changing
(the maintainers' notes, 2026-10-19). The design deliberately depends on the library's
*shape*, not its exact signatures, so changes there stay inside one package.

## 1. What the library gives us

| Property (from the brief) | What it means for Go |
|---|---|
| Pure state machines: no I/O, syscalls, threads or global state | Calls are short CPU-bound functions. No C thread ever calls back into Go |
| Each layer is a struct plus functions over it | One set of structs per session, owned like any Go value |
| Feed in: `dect_lc_rx_fragment()`, `dect_lapc_rx()`, `dect_mm_rx()`, `dect_cc_rx()` | Driven by MAIL_UP frames (`0x0203`) |
| Drain out: `dect_mm_poll()`, `dect_cc_poll()`, `dect_lapc_poll_tx()` | Drained after every input, turned into MAIL_DOWN frames and Go events |
| Caller supplies randomness and time (e.g. `dect_mm_rx()` takes challenge values as parameters) | Entropy and clock are Go interfaces, so tests are deterministic |
| Largest NWK message 256 bytes; largest mail parameter block 249 bytes | Fixed-size C buffers suffice; nothing is variable-length at the boundary |

## 2. Packages

Proposed layout (to be confirmed in B1):

| Package | Role | Imports `"C"` |
|---|---|---|
| `third_party/opendectcell` | git submodule of the openDECTcell repo, pinned to a commit | — |
| `internal/dect/cdect` | The **only** cgo package. Thin wrapper: allocates C structs, copies bytes in and out, maps return codes to Go errors | yes |
| `internal/dect/session` | One goroutine per session: owns a `cdect` handle, timers, entropy; turns mails into events | no |
| `internal/dect/core` | Session registry, dispatch from cells to sessions, registration window, link to the database and the SIP side | no |

No other package may import `"C"`. That keeps the boundary in one place for
review, for `cgocheck`, and for API churn in `libopendectcell`.

## 3. Sessions and goroutines

### 3.1 What a session is

A **session is one radio connection of one handset on one cell.** It is created
when the cell reports connection-up (`0x0200`), and ends at connection release
(`0x0201`), on a protocol error, or when the cell's grace period expires
(`cell-protocol.md` §6.4). It owns the full stack for that connection: LC,
LAPC, MM and CC structs.

The brief speaks of "one goroutine per handset session". A per-connection
session is the same thing in practice, since a handset has at most one radio
connection at a time (handover is C9 research), but it starts before the
handset has identified itself. The connection-up mail arrives before the first
NWK message names the IPUI. Once it does, the session binds to the handset's
database record. If the handset already has a live session elsewhere (a stale
one on another cell), the **newer one wins** and the old one is closed.

Persistent handset state (IPUI, TPUI, UAK, capabilities, last cell) lives in
the database, not in C structs. A session loads what it needs when it binds,
and writes back changes (for example a new TPUI) through `core`.

### 3.2 Goroutines

| Goroutine | Count | Does |
|---|---|---|
| Cell reader | 1 per cell | Decodes frames, routes MAIL_UP to a session inbox by connection id **[cell? C4]**, or to the cell manager for non-session mails (radio, registration window, status) |
| Cell writer | 1 per cell | The priority scheduler of `cell-protocol.md` §9 |
| Session | 1 per radio connection | Owns all C structs of the session. The only goroutine that ever touches them |

The cell reader never blocks on a session. Each session inbox is a bounded
channel (32 mails); a full inbox resets that session (`cell-protocol.md`
§9.2) instead of stalling the other handsets on the cell.

At the design target (10 cells, 50 handsets) this is at most ~70 goroutines,
which is nothing for Go.

### 3.3 The session loop

```go
// Illustrative. cdect names and signatures are assumptions [api?].
func (s *Session) run(ctx context.Context) {
	defer s.close() // frees C memory, zeroes keys, releases the radio link
	defer s.recoverPanic()
	for {
		select {
		case m := <-s.inbox: // MAIL_UP for this connection
			s.stack.FeedFragment(m.Param, s.clock.NowMs())
		case <-s.timer.C:
			s.stack.Tick(s.clock.NowMs())
		case cmd := <-s.cmds: // from core: e.g. "ring", "release", "SIP answered"
			s.apply(cmd)
		case <-ctx.Done():
			return
		}
		s.drain()      // poll every layer until empty → MAIL_DOWN frames + events
		s.rearmTimer() // earliest deadline across layers
	}
}
```

Everything the session does to its stack happens on this goroutine, between
two `select`s. The LAPC acknowledgement for an incoming frame is therefore
drained and handed to the cell writer in the same loop iteration that fed the
frame. That is what keeps the controller's share of the latency budget
(`cell-protocol.md` §2, 10 ms p99) easy to meet.

## 4. Ownership and locking

- **No mutexes around C state.** A session's structs are touched only by its
  goroutine. Other goroutines talk to it by channel. This is the whole locking
  strategy, and it relies on the library having no global state (brief).
- The `cdect.Stack` handle is not safe for concurrent use, and says so in its
  doc comment. A `-race` test that tries it is not useful, because the race
  detector does not see C memory. Instead, the debug build records the owning
  goroutine and panics if another one calls in.
- **Lifetime:** `cdect.New()` allocates; `Stack.Close()` zeroes and frees. The
  session's `defer` always calls `Close`. A runtime cleanup
  (`runtime.AddCleanup`) on the handle is a safety net that logs a leak; it is
  never the normal path.

## 5. Memory across the boundary

Go's cgo rules allow passing a Go pointer to C only for the duration of a call,
and only if C neither keeps it nor stores Go pointers in it. Layered protocol
structs routinely hold pointers to each other (LAPC to LC, for example)
**[api?]**, so the design avoids the question entirely:

1. **All `libopendectcell` structs live in C memory.** `cdect.New()` makes one
   `C.calloc` for a wrapper struct that contains every layer struct plus the
   I/O buffers, and wires the layers together in C. Go holds a single
   `*C.struct_cdect_stack`, which is a C pointer, so the cgo rules don't apply
   to it.
2. **Bytes in are copied** into a fixed C input buffer inside that struct
   (256 bytes, the NWK maximum) with one `memcpy`-equivalent, then the C
   function is called on that buffer. Input longer than the buffer is rejected
   in Go before any C call.
3. **Bytes out** are written by C into fixed C output buffers. Go copies them
   out (`C.GoBytes`) before the next call can overwrite them.
4. **No Go pointer ever reaches C**, so nothing can be retained by mistake.
   Tests run with `GOEXPERIMENT=cgocheck2` to prove it.

The copies are a few hundred bytes per mail, at a rate bounded by ~16 kB/s per
cell. Their cost is unmeasurable next to the network.

**Secrets in C memory.** The UAK and any derived keys sit inside the C struct.
`Close()` zeroes the whole struct with `explicit_bzero` (or an equivalent
volatile loop in our wrapper C) before `free`. On the Go side, the decrypted
UAK is a `[]byte` that is zeroed right after it has been copied into C. This
is best effort, since Go's GC may have copied it; it is the same limit as for
any Go secret, and `docs/threat-model.md` (A5) records it.

## 6. Time and timers

- **Timers live in Go.** C never sleeps and never reads a clock.
- The clock is an interface: `NowMs() uint32` (monotonic milliseconds from an
  arbitrary origin, wrapping). Production uses `time.Since(start)`; tests use a
  fake clock that only advances when the test says so.
- Each session has **one** `time.Timer`. After every input and drain, the loop
  re-arms it for the earliest deadline any layer reports.

How the C library tells us its deadlines is **[api?]**. The wrapper supports
either of the two shapes such libraries usually have, and the rest of the
design doesn't care which:

| If `libopendectcell`… | The wrapper… |
|---|---|
| takes `now` on every call and exposes "next deadline" per layer (or a `tick(now)`) | computes the minimum and re-arms the timer; on fire, calls `tick(now)` |
| emits "start timer X for N ms" / "stop timer X" in its poll output | keeps a small table of named timers in Go, re-arms the session timer for the earliest one, and calls the layer's timeout function when it fires |

Expected scale: a handful of timers per session (LAPC retransmit around 1 s,
MM and CC procedure timers in seconds). Timer precision of a few milliseconds
is plenty.

## 7. Randomness

- Entropy is an interface `Entropy.Read(p []byte)`. Production uses
  `crypto/rand`; tests use a seeded deterministic generator, or fixed values
  taken from a golden trace.
- The Go side draws exactly the values a call needs, immediately before the
  call, and passes them as parameters (as `dect_mm_rx()` does with its
  challenge values). Values are zeroed after the call and never logged.
- Because both time and randomness are injected, **a session is a pure function
  of its inputs.** Replaying a recorded trace with the same clock steps and
  entropy reproduces its output byte for byte (§9).

## 8. Errors and panics

| Failure | Handling |
|---|---|
| C function returns an error code (malformed input, unexpected state) | Mapped to a typed Go error in `cdect`. The session decides by type: drop the frame (LAPC will retransmit), or release the connection |
| Go panic inside a session | `recoverPanic` logs it (no key, PIN or identity bytes), closes the stack (which frees C memory), asks the cell to release the radio connection, and ends the session. Other sessions carry on |
| Crash inside C (SIGSEGV, `abort()` from an `assert`) | Cannot be recovered in-process: the whole controller dies, and Docker's restart policy brings it back. Cells resume or reconnect (`cell-protocol.md` §6.4). Prevented, not handled: bounds checks in the wrapper before every call, fuzzing, and sanitizer builds in CI (§10) |

**Alternative rejected for v1:** running `libopendectcell` in separate worker
processes (one per cell) so that a C crash takes down one cell instead of the
controller. It adds an IPC hop to the latency-critical path, and process
management, to guard against a failure mode the library's design (no
allocation, no I/O, fixed bounds) makes unlikely. Revisit if crashes are seen.

## 9. Testing

| Level | What |
|---|---|
| `cdect` unit tests | Allocation and free, boundary sizes (0, 249, 256, 257 bytes), each feed/poll function once, error-code mapping. Run under `cgocheck2` and `-asan` |
| Golden traces | openDECTcell's simulator traces replayed through a session with the fake clock and recorded entropy; outgoing mails compared byte for byte |
| Fuzzing | Go native fuzz targets feed arbitrary bytes into each `*_rx` function through `cdect`. A short run in CI, longer runs nightly. These exercise the C code, which is exactly where a crash would hurt most |
| Session tests | Inbox overflow resets the session; a panic is contained; `Close` zeroes the struct |

## 10. Build

**Source:** openDECTcell as a **git submodule** at `third_party/opendectcell`, pinned to a
commit and bumped by an explicit commit here.

| Option | Verdict |
|---|---|
| Submodule, compiled by cgo | **Chosen.** `go build` compiles it, and the version is pinned and visible in review |
| Copying the sources into this repo | Rejected: it is a fork in practice, which `AGENTS.md` §2.3 forbids |
| Prebuilt static library linked through `#cgo LDFLAGS` | Rejected: an extra build step and harder cross-compiles. Still possible later if openDECTcell ships one |

**How cgo compiles it:** cgo only compiles C files in the package's own
directory. `internal/dect/cdect` therefore contains one small wrapper `.c` per
library source file, each of which `#include`s that file from the submodule,
plus our own glue code (`cdect.c`):

```c
// SPDX-License-Identifier: AGPL-3.0-or-later
// lc_unit.c — compiles libopendectcell's LC layer into this package.
#include "../../../third_party/opendectcell/<path>/lc.c"   /* path [api?] */
```

```go
// #cgo CFLAGS: -std=c11 -O2 -Wall -Wextra -Werror -I${SRCDIR}/../../../third_party/opendectcell/<include path>
import "C"
```

**CI** (part of B1): `git submodule update --init`, then build, vet, lint and
test with `CGO_ENABLED=1` and gcc. A second job runs the `cdect` tests and fuzz
seeds with clang and `-asan` (AddressSanitizer + UndefinedBehaviorSanitizer).
If the openDECTcell repository is private, CI needs read access to it, for example
a deploy key **[open, §11]**.

**Docker** (B2): cgo rules out a `scratch` image with a pure-Go static binary.
Either build on Debian and run on a distroless glibc base, or build fully
static against musl on Alpine. B2 decides.

**Licence:** `libopendectcell` is GPL-2.0-or-later (`first-tasks.md` §2), so
linking it into this AGPL-3.0-or-later program is allowed. The combined binary
is distributed under AGPL-3.0-or-later. The pinned openDECTcell commit and its
licence go in a `NOTICE` file. CI should fail if the submodule's licence
identifier ever changes to GPL-2.0-only.

## 11. Open items

| # | Item | Affects |
|---|---|---|
| L1 | **Proof of concept not done.** Acceptance asks for a trivial PoC that compiles and calls one `libopendectcell` function from Go. It couldn't be built in this session: the openDECTcell repository isn't reachable from this machine's checkout or GitHub account, and no Go or C toolchain is installed. It needs either access to openDECTcell or a decision to install toolchains | A2 acceptance |
| L2 | Actual signatures and struct names of the functions in §1 **[api?]** | `cdect` only |
| L3 | Timer interface: deadline/`tick` or start/stop requests (§6) **[api?]** | `cdect` only |
| L4 | Do the layer structs point at each other, and does any `*_rx` function keep a pointer to its input buffer after returning? (§5 copes with either; the answer decides whether the copy is necessary or merely defensive) | `cdect` only |
| L5 | Does the library use `assert()`/`abort()`? Are asserts compiled in for release builds? (§8) | crash strategy |
| L6 | Is there a version macro or function to check against at start-up, given the API is still moving? | `cdect` |
| L7 | Is the openDECTcell repository public, or does CI need a deploy key? | B1 |
| L8 | Which field of `0x0200` / `0x0203` identifies the radio connection, for dispatch (= `cell-protocol.md` C4) | `core` dispatch |
