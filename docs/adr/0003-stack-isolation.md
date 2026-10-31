# ADR 0003: One cell's stack must not take the controller down

Status: **decided** 2026-10-31 (the maintainers' notes): out of process, as proposed, but **several worker processes over all hardware threads, each hosting many cells**, not one per cell (see "The decision" at the end; the table below is the original comparison). Not built yet.

## The problem

The maintainer's requirement (2026-10-31): whatever goes wrong in one
cell's DECT stack, including a crash inside the C code, the controller and
the other cells carry on. Errors are log lines, not exits.

Today (`libopendectcell-integration.md` §8) every cell's `dect_base` runs inside
the controller process. A Go panic is contained to its cell (`21a8c66`,
`59d3892`), but a crash in C (SIGSEGV, a stack overflow, an endless loop in
a tick) cannot be contained in-process: it ends the controller, all cells'
calls, the SIP user agents and the web UI. The library has no `assert`
or `abort`, so a crash means memory corruption, which a recover cannot
make safe to continue from anyway.

The goal is a hosted instance with dozens to hundreds of cells
(`architecture.md` §7), run by different people, each cell fed by mails
from a box someone else has root on. One bad cell or one library bug that
only one chip build triggers must not be everyone's outage.

## Options

| | A. In-process, recover (today) | B. One worker process per cell (recommended) | C. One worker process for all cells |
|---|---|---|---|
| A C crash ends | the controller: every cell, SIP, web | one cell's calls; the worker restarts | every cell's calls; SIP and web stay |
| A hang in C (endless loop) | blocks one goroutine for ever; undetected | watchdog kills the worker | watchdog kills all cells |
| Memory corruption in one cell reaches | everything, keys of all cells included | that cell | all cells |
| Processes for 100 cells | 1 | 101 (a worker is a few MB: the library allocates nothing, the Go runtime is most of it) | 2 |
| Latency added per mail | none | one local socket hop, tens of µs, against a cell RTT of ms (`cell-protocol.md` §2) | same as B |
| Complexity | none | a supervisor, an IPC protocol, a restart policy | same as B, and the worker still needs per-cell isolation inside |
| Keys of a cell held by | the controller | that cell's worker only | the shared worker |

A middle form of B, **one worker per small group of cells** (a site), trades
blast radius for process count. Not needed at the target scale; the
protocol below does not change if it is wanted later.

## Proposal: B

- **The worker** is the controller's own binary, started as
  `opendectcell-controller cellworker` (one artefact, so the worker and the
  controller can never be different versions). It holds one cell's
  `dect_base` (`internal/dect/cdect`) and runs today's cell loop
  (`internal/dect/core` `cellRun`): ticks every 10 ms, sends the bring-up,
  drives the stack. It has **no database, no master key, no network**: its
  only channel is the socket to the controller.
- **The channel** is a `socketpair(AF_UNIX, SOCK_SEQPACKET)` inherited as
  fd 3, so nothing on the host can connect to it. Messages use the cell
  link's framing (`cell-protocol.md` §3: type, corr, a fixed or TLV body),
  one per packet, so `internal/cell/wire` is reused:

  | Direction | Message | Carries |
  |---|---|---|
  | ctl → worker | `INIT` | the cell id, `stack.Config`, the records (keys in clear: same host, same trust as process memory), the owners for `NameVisible` |
  | ctl → worker | `UP`, `DOWN`, `GONE` | the cell link's `CellUp` (HELLO), link lost, cell gone |
  | ctl → worker | `MAIL_UP`, `REJECT` | a chip mail; a MAIL_REJECT for a slot write |
  | ctl → worker | `WINDOW` | registration opened (PIN, owner, expiry) or closed (ADR 0005) |
  | ctl → worker | `OWNERS` | the owners changed (a handset moved accounts) |
  | worker → ctl | `MAIL_DOWN` | a mail for the cell, with its corr |
  | worker → ctl | `SAVE`, `FORGET`, `NAMED`, `INSTALLED`, `UNKNOWN` | the record writer's jobs (`core/cell.go` `gen`); the controller writes the database, sealed, as today |
  | worker → ctl | `EVENT`, `MEDIA` | bridge lines (step 5), MEDIA_OPEN/CLOSE (step 6) |
  | both | `PING`/`PONG` | the watchdog |

  The records and keys cross the socket only at `INIT` and `SAVE`; the
  worker wipes them as the stack does today.
- **Supervision** in the controller: one supervisor goroutine per active
  cell starts the worker at `CellUp`, stops it at `CellGone`. A worker that
  exits, crashes or misses its pings for 2 s (killed with SIGKILL) is logged
  with its exit status or signal, and restarted with backoff 1 s, 2 s, 4 s
  … up to 60 s, reset after 5 min up. A restart counts as a fresh session
  for the bring-up (radio off first where §0b says so): the calls went with
  the worker. While a worker is down, MAIL_UP for its cell is dropped and
  counted; the cell link stays up.
- **Hardening the worker:** `RLIMIT_CORE` 0 and `PR_SET_DUMPABLE` 0, so a
  crash never writes UAKs into a core file; `RLIMIT_AS` (256 MB) and a CPU
  watchdog by the pings; no environment inherited (the master key never
  reaches it); `PR_SET_PDEATHSIG` so a worker dies with the controller.
  seccomp later, if wanted.
- **What it buys beyond crashes:** the controller binary no longer needs
  the stack in-process, and `libopendectcell-integration.md` §8's "crash in C
  ends the controller" row goes away. The keys of all cells no longer share
  one address space.
- **What it does not change:** S7 (the maintainers' notes point 1). The worker
  is a libopendectcell host like today's `host.c`; as each S7 stage lands, the
  worker calls the library instead of `host.c`. The IPC protocol sits
  above that and does not move.

## Alternatives rejected

- **A, in-process with recover**: cannot meet the requirement (a C crash or
  hang ends or blocks the controller). Rejected for that reason; it was
  the v1 choice in `libopendectcell-integration.md` §8, made against a single
  installation, not a shared network.
- **C, one worker for all cells**: protects SIP and the web from the
  stack, but not cell B from cell A. Rejected: half the requirement.
- **`dect-stubd` with a remote transport as the worker** (ADR 0002's
  option C): after S7.4 `dect-stubd` is the library plus I/O, so it could
  be the worker. It would need the controller's hooks (`name_visible` by
  account, the window's owner, records without its own store) as a
  protocol in a GPL-2.0 daemon, and it would be a second binary to keep in
  version lockstep. Rejected for now; worth a look once S7.4 has landed.

## Cost and order

About the size of C3.2: the protocol (`internal/dect/workerproto`), the
worker main (today's `cellRun` moved), the supervisor (replaces the
goroutine per cell in `core`), tests that kill a worker with SIGSEGV and
SIGKILL mid-call and check that the other cells' mails carry on. The
golden-trace replay and the core's tests run against the worker
unchanged. Proposed order: after step 3 (a real 7412), before step 5
(calls), so calls are built on the final process layout.

## The decision (maintainer, 2026-10-31, the maintainers' notes)

**B' with N workers, N from the hardware threads.** Crash isolation is the
reason for processes; the thread count is the reason for several. The 10 ms
tick and the 20 ms mail budget are time-critical, so the work is spread
over every CPU thread, and libopendectcell has no global state, so one worker
may host many cells.

| | Value |
|---|---|
| Workers | `runtime.NumCPU()` at start (config `dect.workers` overrides; 0 = that) |
| Assignment of a cell | **stable**: a hash of the cell id modulo N, so a cell lands on the same worker across restarts while N is unchanged |
| A worker crashes | every cell it hosts loses its calls and is rebuilt in the restarted worker (a fresh session for each); the other workers carry on. Blast radius 1/N of the cells instead of one cell |
| Inside a worker | one goroutine per cell as in today's `core`, the same `cellRun`; the protocol messages carry the cell id |
| Keys | a worker holds the keys of its cells only |

Changes against the proposal above: the protocol's messages gain the cell
id (they were per-worker before); `INIT` becomes per cell; the supervisor
is one per worker, not per cell. `WINDOW` carries the list of live
registration PINs (ADR 0005), not one PIN and owner: the owner stays in the
controller, which maps the proved PIN to a user when `SAVE` arrives.

**Order:** the controller decides (§0i). Proposed: after step 4
(registration, built now) and step 3 (a real 7412, at the bench), before
step 5 (calls), so calls are built on the final process layout.

## Open

- The ping interval and the backoff numbers are guesses; the bench will say.
- Whether a cell should move to another worker when N changes (a restart
  of the controller with more threads): it does, by the hash; its calls
  went with the restart anyway.
