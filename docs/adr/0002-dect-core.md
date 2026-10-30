# ADR 0002: Where the controller's DECT core comes from

Status: **proposed** 2026-10-30, for the maintainer to decide (C3).

## The problem

C3 needs the base-station side of DECT on the controller: registration
with the chip's slot table, locate, authentication and ciphering, calls in
and out (several, call waiting, hold), CLIP/CNIP, date and time, message
waiting, the list access service and the missed call notification.

`docs/libopendectcell-integration.md` (2026-10-19) planned to glue `libopendectcell`'s
layers together in a cgo package (`cdect.c`) and a Go session layer. Since
then the cell side has grown that glue in `dect-stubd`: about 13,000 lines
of C on top of the 11,000-line library, tested on two box models and three
handset models (Gigaset A540, C430A, AS690H). Most of what C3 needs is in
those 13,000 lines, not in the library. A second copy in the controller is
the fork `AGENTS.md` §2.3 forbids, and it would start without the bench
history behind the first.

## Options

| | A. Glue in the controller (the 2026-10-19 plan) | B. Move the stack into `libopendectcell` first | C. Run `dect-stubd` on the controller, one per cell |
|---|---|---|---|
| What is built | `cdect.c` + `internal/dect/session` + `core`: registration, slot writes, MM/CC drive, calls, cipher, lists, again | A per-base stack object in the library, with I/O and timers as callbacks; `dect-stubd` and the controller both call it | `dect-stubd` gets a remote transport (`-R`): chip mails as MAIL_UP / MAIL_DOWN frames on a socket instead of the PCM lane. The controller pipes a cell's tunnel into it and speaks openDECTcell's bridge interface to it, as baresip's module does on a box |
| Reuse of tested behaviour | None; re-derived | All, after the move | All, as it is |
| Drift between box and controller | Two implementations | None | None |
| Size before a handset registers through the controller | Largest | Large: a refactor of a hardware-tested daemon, every bench test again | Smallest: a transport in `dect-stubd`, a process supervisor and a bridge client in Go, media mapped to MEDIA_OPEN |
| Multi-cell (roaming, one handset on several cells) | Natural: one handset table | Natural, once built | Not by itself: each `dect-stubd` owns one cell's subscriptions. Needs records shared across instances, or option B later |
| Keys off the cell (`cell-protocol.md` §1) | Yes | Yes | Yes: `dect-stubd` runs on the controller |
| Licence | Library linked into AGPL code | Same | Separate process (GPL-2.0-or-later beside AGPL) |

## Recommendation

**C now, B as the path to multi-cell.** C gets the [hw] milestones (a
handset registers through the controller, an end-to-end call) with the
code that already passes them on a box, and turns the controller's C3 into
plumbing: tunnel, process supervision, bridge, media. Once one cell works,
the stack moves into the library piece by piece (B), driven by what
roaming needs, and `dect-stubd` stays a thin host on the box. A is not
recommended: it duplicates the largest and best-tested part of the
project.

## Open, if C is chosen

- `dect-stubd -R`: the contact handshake and the control-slot search end at
  the cell, so the remote transport starts at "session up" (cell state
  ACTIVE) and maps MAIL_REJECT to its send errors.
- Timing: LAPC and the procedure timers run on the controller's clock; the
  WAN round trip is inside `cell-protocol.md` §2's budget, to be measured.
- Media: `dect-stubd`'s lane binding becomes MEDIA_OPEN / MEDIA_CLOSE to the
  cell, the audio its bridge socket of `docs/bridge-interface.md` §3.
- Subscriptions: one store per cell for the first version; a handset
  registered through cell A works on cell A only.
