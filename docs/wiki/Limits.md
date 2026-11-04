# Limits and what is not there yet

An honest list, so you can judge whether openDECTcell fits your case today. It is the summary of
[next-steps-2026-11-02.md](../next-steps-2026-11-02.md), which has the details and the plan.

## Works, and has been seen on hardware

Registering handsets; calls in and out with audio, HD voice (G.722), encrypted on the air; two cells (a 7520 and a
7412) of one controller; handsets moving between them when a box goes away; the cell connection (TLS, enrolment,
certificate renewal) against the real controller; SIP registration and calls; caller number on the display.

## Built, but not seen on hardware

The message-waiting indicator (the SIP mailbox to the handset's lamp); the **handset call lists** (missed, received, dialled, from the call log; read-only) and the **handset phonebook** (each handset reads its owner's contacts and the shared ones, and its edits go back to the controller); handsets choosing a cell by walking around with several boxes on at once; hold and resume; two calls on one handset;
three or five parallel calls on one cell; call waiting; the audio of a cell beyond a test peer; the LuCI
firmware-upload page.

## Not built

| Area | Not there |
|---|---|
| Calls | Transfer, conference, early media; calls **between** handsets (refused); handover of a call between cells |
| Handset features | Reading a handset's call-list **deletes and read marks** back into the controller (the call lists themselves are built); the date shown by every handset model |
| Cells | A way to update a cell's software from the controller; rotation of the controller's certificate authority; a hard limit of calls per cell |
| Controller | A calls page for users (a stub today); WebAuthn security keys; personal API tokens; a list of your sessions; German or other languages; a manual for administrators beyond this wiki |
| Operations | Scheduled backups (a restore is tested by `tools/restore-test.sh`); high availability (one controller, one database); more than about ten metrics; per-call and per-cell quality metrics; per-tenant quotas |
| Hardware | The FRITZ!Box 7362 SL; the 7530 untested; the 7412 needs a kernel module that is not in the release yet |
| Releases | Public releases and a package repository (planned after the controller is done) |

## Things that behave in a way you may not expect

- **A call in progress does not follow a handset.** If the cell goes away the call ends.
- **A 7412 cell carries three calls at once**, a 7520 five.
- **Two handsets whose temporary numbers end in the same digit** cannot both be in a call or ringing at once on a
  7412 cell ([Roaming](Roaming-and-many-cells.md)).
- **By default all cells' DECT stacks run inside one controller process.** A crash in the DECT library stops every call
  until it restarts. Worker processes (`OPENDECTCELL_CELLS_STACK_WORKERS`) contain a crash to one worker and its cells; built and
  tried on the lab, off by default until it has had a long soak.
- **Scale.** Designed and tested at the database and routing level for 100 cells and 500 handsets; not tested with that
  many real connections. The documented comfortable size today is about 10 cells and 50 handsets.
- **The cell operator can read the handset keys of its cell** by design (the base has to hold them): do not enrol a
  cell you do not trust. See [threat-model.md](../threat-model.md).
- **A hand-edited file under `/etc` on the box** meets the package manager's `*.apk-new` files on upgrade.
- **English only.**

## What is planned next

The ordered list is in [next-steps-2026-11-02.md](../next-steps-2026-11-02.md): a load test with real connections, a
walk test of roaming, the ring policy for same-digit handsets, a cell update channel, the missing handset features,
and the administrator documentation.
