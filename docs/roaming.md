# Roaming: what a controller can do for idle handsets

Investigation of 2026-11-01 (the openDECTcell agent), requested by the maintainer: can the controller
move an idle handset from one cell to another when the signal gets better at the other cell?
Read from our own notes first (`../opendectcell/docs/multicell-identities.md`, `multicell-experiments.md`,
`diagnostics.md`, `notes-for-the-controller.md` API 18-27, `more-than-six.md`, `cell-protocol.md`
§6.4 and §7.4, ADR 0004) and from EN 300 175-3 and GAP on disk. Nothing in this document was measured
on air by this investigation; every fact names where it comes from.

## 1. The short answer

**Not as asked, and the reason is in the standard, not in our code.**

1. **An idle handset picks its own cell.** EN 300 175-3 §11.3.3.0: an idle locked PP "may occasionally
   scan for RFPs with a stronger signal strength. If a stronger RFP is found, then the PP may lock to
   this RFP instead." Nothing in the DECT air interface lets a base tell an idle PP to go elsewhere:
   there is no steering command (no counterpart of 802.11k/v). A PP is bound to a cell only as long as
   it hears that cell's beacon: it leaves after T207 = 5 s without a correct A-field or T208 = 20 s
   without an NT message (§11.3.3.0, annex A).
2. **The controller cannot see an idle handset's signal at the other cell.** An idle PP transmits
   nothing and listens to one RFP only. A cell measures a handset only while a link exists (the chip's
   per-link record `0xF625`, ten carrier levels, `diagnostics.md`), and only on the cell the handset is
   locked to. So "the signal improved at cell B" is not observable for a handset that sits on cell A.
3. **What we can do is make the handset's own roaming work, follow it, and route calls to where it is.**
   Section 4 lists that work; section 3 lists the levers that come close to "bumping" and why most are a
   bad idea.

(Vendors do the same. A Gigaset N870 cluster roams idle handsets and hands calls over, but "a handover is
only possible if the cells are synchronised" and the handset chooses; see the
[Gigaset N870 planning guide](https://teamwork.gigaset.com/gigawiki/download/attachments/829377542/Gigaset%20N870_Planning_guide_en_IE-UK_INT.pdf?api=v2)
and [its FAQ](https://pro-wiki.gigaset.com/bin/view/GPPPO/Products/N870/).
One N870 feature does read RSSI of neighbours *from a handset*, a proprietary alarm-server function; we
have nothing like it for the handsets we serve.)

## 2. What has to be true before a handset can roam at all

Today **handsets do not roam between our boxes**. Every cell broadcasts its box's factory RFPI, so every
box is a separate DECT system (ADR 0004 "Identity across sites": "with factory identities, handsets do not
roam between boxes"). The controller can already make up a class B identity (Cells page, `cell.SystemIdentity`,
`RFPI(rpn)`), but the cells do not take it:

| Need | State |
|---|---|
| One PARI on every cell, a distinct RPN per cell | **not built.** The chip has no runtime mail for the RFPI; it is tag 1 of the STUB image at upload, so a change is a chip reload (room A-0131). `dect-fwload --rfpi` and uci `rfpi_test` exist for the bench; class B RPN 8 bits stored verbatim on 06.88 (bench B0, 2026-10-30). The cell protocol has no message that carries the identity |
| PARK bytes and LAL as parameters | **not built** in the library (LAL 36 is hard-coded for class A; `cfg.park` is planned, `multicell-identities.md` §7.4). Assigned IPUI O exists (`assign_id`, bench B4a open) |
| Class A or B | A: at most 7 cells (RPN 3 bits) with a borrowed ARI; B: 256 cells per site with an EIC (the maintainer allows a made-up one while this is experimental) |
| Handsets registered under that PARK | they re-register once, whatever class; the subscription is the pair IPUI + PARK |

## 3. The levers that come near "bumping", ranked

| Lever | What it does | Verdict |
|---|---|---|
| **Do nothing; let the handset choose** | The PP re-locks to the strongest RFP of its PARI by itself, no network traffic | **The method.** Needs section 2 and good cell placement. Behaviour differs per model (the A540 did not re-locate after a six-minute outage, the AS690H did; `multicell-experiments.md` M4), so measure with the handsets we support |
| **Probe a handset's level on its cell** | Page it without ringing, or send `{MM-INFO-SUGGEST}` "locate suggest" (GAP 8.29): the handset opens a link and the chip's `0xF625` gives its level on *that* cell | Gives one number for one cell. Useful as a diagnostics and placement tool ("this handset is weak in the kitchen"), not a steering signal. Costs battery and a link per probe; not built |
| **Park a cell for a few seconds** | All its idle handsets lose it after T207 = 5 s and re-lock to the strongest other cell; after un-parking they stay where they are unless the old cell is stronger | Blunt: cannot be aimed at one handset, cannot be done with calls up, and a Gigaset that finds nothing backs off for minutes after a long gap (162 s after 17 minutes, `multicell-experiments.md`). Fit for fail-over and maintenance only. The cell protocol's `orphan_s` already parks a cell that lost the controller |
| **Reduce a cell's transmit power** | A weaker beacon makes handsets near the edge prefer the neighbour | Whether the STUB lets the host set the power is **not known** (a question for the clean-room Q&A); the diagnostics list "radio power" as a field to read, not to set. Cell placement by hand does the same |
| **Reject or terminate to push a handset away** (`{LOCATE-REJECT}`, access rights termination) | Would make a handset give up the cell | Wrong tool: it damages the registration, not the lock; GAP does not offer FT-initiated access-rights changes (`multicell-identities.md` §5). Not recommended |
| **Move a call** (connection handover) | The only DECT mechanism that moves a handset between cells on purpose | PT-initiated (EN 300 175-4 §10.5), FT support optional, needs synchronised cells and a DLC and key that move between chips. Not possible now, as the maintainer says (`multicell-identities.md` §8.3-8.4) |

## 4. What "proper roaming" is in the controller: the work

In order; 1 and 2 are the preconditions, 3 to 5 are what the user feels.

1. **Shared identity on every cell** (section 2): a cell reports its factory RFPI in HELLO, the controller
   assigns RPN and sends RFPI (plus PARK, LAL), the cell patches tag 1, reloads the chip and reports the
   RFPI it reads back (`0xF601`). Chip reload is already part of every cell start since 2026-11-01.
2. **A handset's record on every cell** without using chip slots. Keyless admission is measured on 06.88,
   07.56 and 08.25 (`more-than-six.md` §6), so a standby handset needs no slot; the host cache is decided.
   In the controller the stack of a cell is loaded with `CellRecords(max = chip slots)`
   (`internal/dect/core/cell.go`, `max := CapSubRecords` capped at 6): that cap must go, and records are
   held in host memory for every handset of the network, or fetched on demand with the held
   `{LOCATE-REQUEST}` / `{CC-SETUP}` of API 18/19/24.
3. **Know where each handset is.** `handset.last_seen_cell_id/at` exist in the schema and the pages show
   them, but nothing writes them: the "locate/idle tracking" item of C3 is open. Write them on every
   locate, page response, call setup and list access (the library already tells the core which link
   belongs to which handset). With one location area per system (LAL 36 or the class B equivalent) a handset
   that changes cell does **not** locate again (EN 300 175-5 §13.4; seen on the A540), so this is only as
   fresh as the handset's last contact. A smaller location area makes every cell change visible:
   - class A: LAL 39 is one LA per cell;
   - class B: the LAL counts bits of PARI + RPN from the top, so LAL = 31 + j makes cells that share their
     top j RPN bits one location area. The controller can number cells so that a floor or a room is one
     area. The price: a link and a locate per crossing, ping-pong at borders. Which setting the handsets
     tolerate is bench B2.
4. **Route calls to where the handset is.** Page on the last cell first and on every cell of the location
   area after about 3 s (`<LCE.03>`); the first cell that gets a response takes the call. Whether a pending
   page can be withdrawn on the other cells is an open question to the clean-room Q&A (Q7); without it, the extra
   pages are harmless but wasteful. The shared TPUI pool (API 141, change request L10) is a precondition so
   that a page by TPUI means the same handset on every cell; it is not wired yet.
5. **Show it.** Handsets page: "last seen on cell X at time"; statistics: crossings per day; the public map
   needs none of it.
6. **Optional, later: levels.** A diagnostics message in the cell protocol (none exists, `ROADMAP.md` C2)
   carrying `dect_diag` per link: dBm and quality of a handset in a call or a probe. With neighbour RFPIs
   and levels from the chip (Q8, the RFPI_IND primitive is not known) the controller could draw a
   cell-to-cell picture, which is what a placement tool needs.

### 4.1 Status, 2026-11-01 (built, tested with fakes and the real library; not yet on air)

| Point | State |
|---|---|
| 1 Shared identity | Built end to end and **verified on the lab 7520 on 2026-11-01**: the controller assigned RPN 1 and a class B RFPI, the cell patched tag 1 and reloaded its chip (08.25 boots with a class B RFPI), the chip read it back, and the revert brought the factory RFPI back (`../opendectcell/docs/bench-2026-11-01.md`). Not tried: a handset on a class B base, the 7412 (its packages predate the identity messages), two cells with one identity. |
| 2 Record on every cell | Built. A handset that enrols gets a TPUI from the pool `{00, 11, N}` (N from 7) and every cell holds it under a record number from 7 to 16 (libopendectcell API 144), host-only: no chip slot, no key in the chip, admitted keyless (`more-than-six.md` §6). **Because every cell holds every pooled handset, the table of one cell is the limit of the whole installation: record numbers 7 to 32 (API 145), so 26 pooled handsets in total, not the 249 the TPUI form would allow (and that form was benched up to N = 0x43 only).** More handsets need a working set per cell instead of all of them everywhere (§4.2). The cell a handset enrolled on rebuilds its stack 3 s later, so the handset leaves the chip slot it enrolled in; the other cells take it into the running stack without a restart. A handset that deregisters anywhere is dropped from every cell (the others rebuild). Config switch `cells.handset_pool` (default on). Handsets enrolled earlier stay chip-slot handsets of their home cells; there is no migration. |
| 3 Where it is | Built: `last_seen_cell_id` / `last_seen_at` are written from the stack's per-record contact time (at most one write per 30 s per handset and cell), and every change of cell is a row in `handset_move`. |
| 4 Routing | Built as an API without a caller: `core.Ring(handset, call)` offers the call on the cell the handset was last heard on, after 3 s on every other cell that holds it, and the first answer takes the call; the other legs get `remote-released`. Busy legs do not hold the next wave back. It is tested against fake stacks that speak the bridge verbs; there is no SIP side to call it yet, and nobody has paged a handset by pool TPUI on air. Withdrawing a pending page on the other cells is Q7 and is not done: the extra legs are released with `remote-released`; whether the stack then stops its page was not checked. |
| 5 UI | Handset page: "every cell" or "home cell only", the last ten cell changes. Statistics: cell changes in 24 h and 7 days. Cell changes are location data: retention erases them with the call details. |

What is not built: the controller does not choose the location area size (class A or B, LAL 31 + j) for any cell; the
maintainer sets the identity per cell on the cells page. IPUI O for class B cross-site subscription (needs a window
option). Records numbered 8 or more have no names-list entry and no call-list terminal id (the library keeps eight and
seven, `api-versions.md` #144); a pooled handset beyond the first eight records of a cell is a plain phone there.

### 4.2 Beyond 26 handsets: a working set per cell (proposal, not built)

The controller keeps every record in the database and every cell's stack lives in the controller, so a cell
does not need all handsets, only the ones that can be on it. Proposed: a cell's table is a cache.

- **In:** a handset enters a cell's table when it is heard there (a locate or an access request that the stack
  could answer), when the router is about to page it there (`addHandset` already adds a record to a running
  stack), and at enrolment on the cell it enrolled on.
- **Out:** a record that has not been heard on that cell for a few multiples of the re-location interval is
  dropped. The interval is the lever of API 20: `locate_validity` puts <<DURATION>> into {LOCATE-ACCEPT}, the
  handset locates again after N x 40.96 s (N up to 255, about 2.9 h). The AS690H was measured to renew at
  41.5 s with N = 1 (`multicell-experiments.md` M6a); the A540 and C430A are not measured. The controller
  does not set it today (`stack.Config` has no field for it). A handset that stops renewing is off or gone, so
  "not heard for 2 x validity" is also presence.
- **Needs:** (1) a library call to remove a record from a running stack (today only a rebuild does it);
  (2) a hook for a locate from an IPUI the cell does not hold, so the controller can look the record up and
  admit it instead of rejecting it (a handset told "IPUI unknown" may take it as losing its registration; API 19
  holds a {CC-SETUP} from an unknown identity, the same idea for the locate); (3) TPUIs that are unique per
  cell, not per installation, or a wider TPUI form (bench).
- **Cost of a re-location interval:** a link setup per handset per interval, which is battery. 30 minutes to
  2 hours is plausible; that is a number for the bench.

## 5. What can be tried now, with what we have

The two lab boxes are cells of the maintainer's controller. Bench B1/B2 of `multicell-identities.md` §10.2
need only two cells with one PARI and RPN 1 and 2, which `rfpi_test` (uci, bench option) already allows on
06.88 and 08.25: patch both cells' tag 1 with `controller identity RFPI(1)` and `RFPI(2)`, register a
handset under that PARK, carry it between the rooms, and look in the controller log for which cell serves
its list access, its page response and its calls. Predictions to pre-commit before the run:

- B1 (LAL 36): the handset changes cell with no `{LOCATE-REQUEST}` and answers a page sent on both
  cells; falsified if it re-registers, shows "no base" for minutes or answers on neither.
- B2 (LAL 39, library change needed): each change brings a `{LOCATE-REQUEST}` with the old PARI and RPN.
- B3: a call while walking from one cell to the other drops (no sync); record whether the handset tries a
  handover.
- B5: park one cell for 2 s with the handset idle on it and time how long it takes to appear on the other.

The cells are not synchronised; for idle roaming that is a conformance gap, not a blocker in practice
(`multicell-identities.md` §8.4).

## 6. Questions

For the clean-room Q&A (clean-room record): (a) can the host set the radio's transmit power? (b) the RFPI_IND
primitive: what does it carry, can it run with the radio on, does it give a level per neighbour? (c) Q7:
can a pending page be withdrawn?

For the maintainer: (1) class A with a borrowed ARI (7 cells, no EIC) or class B with the made-up EIC for
the first roaming test; (2) one location area per system or per group of cells (affects how fresh
"last seen" is and how much locate traffic there is); (3) whether parking a cell as a roaming push is
acceptable as an admin action at all (it drops the cell's idle handsets for seconds).
