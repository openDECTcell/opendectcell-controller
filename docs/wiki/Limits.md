# Limits and what is not there yet

An honest list, so you can judge whether openDECTcell fits your case today. It is the summary of
[next-steps-2026-11-02.md](../next-steps-2026-11-02.md), which has the details and the plan.

## Works, and has been seen on hardware

Registering handsets; calls in and out with audio, HD voice (G.722), encrypted on the air; two cells (a 7520 and a
7412) of one controller; **a shared network identity on two boxes, a 7520 and a 7412, and handsets moving to the 7412 within seconds when the 7520 was unplugged** (2026-11-05, with a Gigaset A420, A540, AS690H and a Motorola O201; the AS690H also needed its own restart before it could dial; without a shared identity every box is a DECT system of its own and a handset registered on one finds no other; the first sign-in now proposes the identity); the cell connection (TLS, enrolment,
certificate renewal) against the real controller; SIP registration and calls; caller number on the display.

## Built, but not seen on hardware

The message-waiting indicator (the SIP mailbox to the handset's lamp); the **handset call lists** (missed, received, dialled, from the call log; read-only) and the **handset phonebook** (each handset reads its owner's contacts and the shared ones, and its edits go back to the controller); handsets choosing a cell by walking around with several boxes on at once; hold and resume; two calls on one handset;
more than three parallel calls on one cell (the cells announce three; three at once were up on a 7412 on 2026-11-05, G.722 passed through; the log shows them set up, whether the audio was good is not recorded); call waiting; the audio of a cell beyond a test peer; a handset **imported with its key** from a setup file (it should need no new registration; not tried on air); the LuCI
firmware-upload page.

## Not built

| Area | Not there |
|---|---|
| Calls | Transfer, conference, early media; calls **between** handsets (refused); handover of a call between cells |
| Handset features | Reading a handset's call-list **deletes and read marks** back into the controller (the call lists themselves are built); the date shown by every handset model |
| Cells | A way to update a cell's software from the controller; rotation of the controller's certificate authority (a runbook is in [Operating](Operating.md)); a hard limit of calls per cell |
| Controller | WebAuthn security keys; German or other languages; roles between user and administrator (a site manager, an auditor); a manual for administrators beyond this wiki; a first-run wizard; search, sorting and paging in the long lists (hundreds of base stations or users render every row) |
| Operations | Backups to S3 (the backup service writes to a volume and to SFTP); high availability (one controller, one database); alerts for disk, worker restarts and new releases; a "run a backup now" button; per-tenant quotas |
| Hardware | The 7530 untested; the 7412 and the 7362 SL need extra kernel modules, which the 0.3.7 release includes |
| Releases | Public releases and a package repository (planned after the controller is done) |

## Things that behave in a way you may not expect

- **A call in progress does not follow a handset.** If the cell goes away the call ends.
- **A cell carries three calls at once**, whatever the box (the 7520's chip could do five; not built yet).
- **Two handsets whose temporary numbers end in the same digit** cannot both be in a call or ringing at once on a
  7412 cell ([Roaming](Roaming-and-many-cells.md)).
- **By default all cells' DECT stacks run inside one controller process.** A crash in the DECT library stops every call
  until it restarts. Worker processes (`OPENDECTCELL_CELLS_STACK_WORKERS`) contain a crash to one worker and its cells; built and
  tried on the lab, off by default until it has had a long soak.
- **Scale, measured on the Pi 4** (4 cores, 3.8 GB; [load-test-2026-11-05.md](../load-test-2026-11-05.md) has the method and
  the numbers; [Load testing](Load-testing.md) shows how to measure your own machine). *Calls:* the media engine alone carries about **90 calls at once** in G.722 passthrough, about **70** between
  a narrowband phone line and the cell (PCMA to L16) and about **50 to 60** when it has to convert between a G.711 line and
  a wideband handset, with clean audio (delay variation under 25 ms, no loss); above that the audio degrades quickly
  (110 / 100 / 80 calls: 90 to 340 ms of variation and concealed frames). The Pi gets to 75 to 80 degrees doing it.
  *Cells:* 300 simulated cells connect in 8 s and need 80 KiB each; the database side holds 500 handsets on 100 cells
  (a move takes 7 ms). **Not measured:** the DECT stack's cost per cell and per call (the stacks of the two real cells
  idle at about 5 % of one core together with the controller itself), real radios, and the web interface under load.
  *A comfortable size for one Pi is about 30 simultaneous calls, 20 to 30 cells and 100 handsets.* The limit that
  bites first is the cells' own: a cell carries 3 calls. Cells that restart together behind one address
  reconnect 16 at a time and retry the rest.
- **The cell operator can read the handset keys of its cell** by design (the base has to hold them): do not enrol a
  cell you do not trust. See [threat-model.md](../threat-model.md).
- **A hand-edited file under `/etc` on the box** meets the package manager's `*.apk-new` files on upgrade.
- **English only.**

## What is planned next

The ordered list is in [next-steps-2026-11-02.md](../next-steps-2026-11-02.md): a load test with real connections, a
walk test of roaming, the ring policy for same-digit handsets, a cell update channel, the missing handset features,
and the administrator documentation.
