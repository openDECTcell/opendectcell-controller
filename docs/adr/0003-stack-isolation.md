# ADR 0003: One cell's stack must not take the controller down

Status: **decided** 2026-10-31 (the maintainers' notes): out of process, as proposed, but **several worker processes over all hardware threads, each hosting many cells**, not one per cell (see "The decision" at the end; the table below is the original comparison). **Built 2026-11-02** (`internal/dect/worker`, option `cells.stack_workers`, off by default; see "As built" at the end).

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
  ends the controller" row goes away. The keys of cells in different workers no longer share
  one address space; **cells in one worker do** (`OPENDECTCELL_CELLS_STACK_WORKERS` workers serve all cells), so a takeover of a worker
  exposes the keys of every cell it holds.
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

## As built (2026-11-02)

What differs from the proposal above, and why:

- **The seam is `stack.Stack`, not the cell loop.** The core (`cellRun`, the store, the bridge) stays in the controller. For a
  cell the core holds a *remote stack* (`worker.Remote.NewStack`) that implements `stack.Stack` and forwards to the real stack in
  a worker; the worker forwards the stack's `stack.Env` callbacks back. No database access, keys or network in the worker, and
  no change to the core's logic beyond three small things: a stack that cannot be made now is retried (`r.retryAt`), a stack
  whose worker ended is dropped and rebuilt, and a stack in a worker wakes its cell (`evPump`).
- **Channel:** one `SOCK_SEQPACKET` pair per worker, file 3 of the child; messages are `type, stack id, request number, body`
  (`internal/dect/worker/proto.go`). Many stacks per worker, as decided. Calls that return a value (`Start`, `Ctl`, `Drop`,
  `Recycle`, `SetName`, `SetProfile`, `Close`) are requests; the others are one-way. What the controller reads often (`Seen`,
  `Signal`, `RegState`, `Quiet`, `HasRFPI`) comes from a snapshot the worker pushes when it changes (at most every 100 ms).
- **The worker ticks itself** every 10 ms on the controller's clock (an offset taken at the stack's creation, never going
  back); the core's `Tick` only runs waiting callbacks.
- **Callbacks run on the cell's goroutine.** `stack.Env` is not thread-safe and runs "inside the call". A callback from the worker is
  queued; the cell's goroutine runs it while it waits for the answer to a request, in `Tick`, and when the proxy wakes the cell
  (`stack.Pumper`). The tests run under the race detector to catch a callback on any other goroutine.
- **Supervision** (`worker.Supervisor`, the proposal's policy): N workers (`OPENDECTCELL_CELLS_STACK_WORKERS`, 0 = off), a new stack goes to
  the worker with the fewest; a ping every 2 s, a worker that misses one is killed; restart with backoff 1 s to 60 s (reset after
  5 minutes up); the worker's crash report is logged once. A call to a stack that does not answer in 5 s declares the worker dead.
  Hardening in the worker: no core file, not dumpable, dies with the controller (`PR_SET_PDEATHSIG`), empty environment. Not
  done: `RLIMIT_AS` (the Go runtime reserves address space and does not start under a small one).
  **Confinement (2026-11-02, security review F-03):** the worker is the controller's own binary under the same uid, so "no database, no
  master key, no network" was not true of a taken-over worker (it could read `/secrets` and connect to the database). Once it has its
  socket it now installs a seccomp filter on every thread (`worker/seccomp_linux.go`, `NO_NEW_PRIVS`, TSYNC) that makes opening files,
  creating sockets, `connect`/`bind`/`listen`, `execve`, `ptrace`, `process_vm_*`, `pidfd_getfd`, mounts and namespace changes fail with
  EPERM, and refuses the 32-bit system call ABI. It is a deny-list by design (an allow-list would break with each Go runtime); the files
  and sockets it keeps are the ones already open. Only on Linux amd64 and arm64; elsewhere the worker runs unconfined.
- **Tested:** in process over a socket pair (round trip, death, hang, ping) and with real child processes (a crash, a hung process
  stopped with SIGSTOP, no worker at all); the core (waking, a dead worker, no stack); **on air, 2026-11-02:** the lab controller
  with two workers, the 7520 and the 7412 each in one, a handset paged through each; then `SIGSEGV` sent to the 7520's worker:
  the controller logged it and went on, the other cell's stack was not touched, the worker was started again, the 7520's stack
  started 5 s later on it, and both handsets were paged again.
- **Not yet:** a soak (days); what a worker restart does to a call in progress (it ends: the call went with the stack, as the ADR
  says); CPU and memory of workers with many cells; the default stays 0 until that has run for a while.


## Before the default changes: the soak (plan, 2026-11-03, audit CTRL-SEC-03)

*Started on the Raspberry Pi test ground on 2026-11-04 at 08:49 CET with two real cells and the crash injection, without calls (no SIP peer or handset
is automated there): see [../test-ground.md](../test-ground.md), "The soak". The result goes into the worklog.*

The default stays `OPENDECTCELL_CELLS_STACK_WORKERS=0` until this has run; the controller now logs a warning at start when the stacks
run in the process. The run, on the lab with real boxes and handsets:

| What | How | Passes when |
|---|---|---|
| Duration | 72 hours with N = the CPU count workers, both boxes attached, a handset on each, a call every 30 minutes (scripted from a SIP peer) | no controller restart, no worker restart that the injection below did not cause |
| Crash injection | `SIGSEGV` to one worker every 2 hours, then `SIGSTOP` for 10 s to another | the controller and the other workers go on; a call on a crashed worker ends and the next one works; the worker is back within 70 s |
| Calls during a restart | a call up while its worker is killed | the call ends cleanly on the SIP side (BYE), no leg or media flow is left (check `/metrics` and the bridge's open flows) |
| Load | many cells per worker (the simulated cell, `scale_test.go` rigs, 100 cells over 2 workers) beside the two real ones | the worker's CPU and memory are flat; page-to-ring latency within 150 ms of the in-process figure |
| Architectures | the same crash test on amd64 and arm64, and a check from inside a worker that opening a file, connecting and `execve` fail (seccomp) | EPERM on all three; on an architecture without the filter the start log says the worker runs unconfined |

A worker that hosts several cells still shares one address space between them: a flaw reachable from one cell's mails can read
another cell's stack memory. Isolation per worker is a limit on *crashes*, not a confidentiality boundary between cells that share
a worker; the seccomp filter keeps a taken-over worker from reaching files, the network and the database, not from reading its own
other cells. Only after the soak, and with that sentence in the release notes, does the default move to a worker per hardware thread.
