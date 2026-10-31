# ADR 0004: A shared network of everyone's cells and handsets

Status: **direction decided** by the maintainer 2026-10-31 (the maintainers' notes point 4); **answered in part 2026-10-31 (§0i)**: the trust model (B8) is accepted as it is, keys go onto a chip **only when needed**, foreign handsets are accepted by default (a per-cell opt-out is the maintainer's to decide). The rest is proposed. Nothing beyond the mandatory owner (`0c6715d`) is built.

## The decision

The controller is one network, like The Things Network for DECT: a handset
of user A may attach to a base of user B and the reverse, and **a user needs
no base of their own to register handsets.** Until now the model was one
installation's handsets on that installation's cells, placed by a home set
the user picked (`architecture.md` §5, `cell-protocol.md` §7.4).

## Consequences

| Area | Before | Now (proposed) |
|---|---|---|
| Handset owner | a user, or NULL = shared | **mandatory**: the user whose registration window it enrolled through (built, `0c6715d`) |
| Cell operator | implicit: `cell.enrolled_by`, admins manage cells | a separate, optional attribute `cell.operator_user_id`: who runs the box and may change its settings. NULL = run by the installation's admins. It grants no rights over the handsets on the cell |
| Who may attach where | home set = access rule | **any handset on any cell** that accepts foreign handsets (per-cell policy, below) |
| Home set (`handset_home_cell`) | where the record always is | a **placement hint** (§0i): cells to prefetch the record to (the owner's own cells, the cell it registered at, cells it was last seen on). Prefetch is weighed against "keys only when needed", below |
| Placement | home set only (§7.4 rules 1-2) | **on demand at the handset's first contact** on a cell (§7.4 rule 3), plus the prefetch. Needs the chip to admit a handset it holds no key for (below) |
| Eviction (§7.4) | least recently seen | as before, **plus: a foreign handset is evicted before one of the operator's own** (proposal), so a busy public cell never locks its operator out |
| Names list, internal calls | per account | unchanged: a handset sees and calls only its owner's handsets, wherever they are attached (`list-access.md` §1.1) |
| Lines, SIP | per handset | unchanged: a handset's calls use its owner's SIP accounts on whichever cell it is attached to; the cell's operator's accounts are never involved |
| Admin UI | admins see all | an operator sees **that** foreign handsets use their cell and how many (capacity), not whose; admins see all |

## What on-demand placement depends on

A handset's first contact on a cell without its record only reaches the
controller if the chip lets it in:

| STUB build | Admits a handset whose key it does not hold? | Source |
|---|---|---|
| 07.56 | **yes**: attaches, locates, is paged, rings | openDECTcell `docs/multicell-experiments.md` M1, M2' |
| 08.25 | **no** in `vF427` (clearing a slot stopped a foreign handset); to be re-tested | openDECTcell `docs/more-than-six.md`, T-B on 2026-11-01 |
| 06.88 (7412) | not tested | T-B on 2026-11-01 |

Whether ciphering then needs the key in a slot is T-D on the same bench day.

- **If T-B says yes** for a build: placement on that build is on demand.
  The controller sees the handset's IPUI at the host's identity gate,
  looks the handset up network-wide, adds its record to the cell's stack
  and writes the slot (evicting per §7.4), and the attach goes on. That
  needs the library to take a record into a running stack: **not in the
  API today** (records go in before `Start`); a `Controller Q:` once T-B
  has an answer, or part of S7.1.
- **If T-B says no**: the handset cannot make first contact on a cell that
  does not already hold its record, so a cell can only serve handsets
  placed there beforehand: the home set and its prefetch. The shared
  network still works for handsets that registered at the cell or were
  prefetched (the owner's own cells, a cell they were seen on, an
  operator's "neighbours" list, §7.4 rule 4), but not for a stranger
  walking up to a cell for the first time. With six slots per chip that
  is a real limit; openDECTcell `docs/more-than-six.md` (the slot cache) is
  the way round it on the cell side.
- **Until then** step 3 keeps home-set placement as its working
  assumption, as the maintainer said.

## Keys only when needed (maintainer, §0i)

A handset's key is on a cell's chip **only while it is needed** (just in
time) and leaves it afterwards. That is on-demand placement, so it rests on
T-B as above; it is not assumed to work on 06.88.

| Fact | Source |
|---|---|
| A key write is effective within 20 ms, on 08.25 and on 06.88; a read in the same instant gets no answer. The read-back waits 50 ms on both (`0f9efc9`), and one missing answer is asked again (a library matter: its read-back has no repeat today) | openDECTcell T-A, 2026-10-31 |
| A slot clear does not wipe the key bytes: to take a key off a chip, overwrite the slot with the next handset's key, or reload the chip | openDECTcell Q-0073 |

So "leaves the chip" means: the slot is overwritten when it is next
needed, and a cell whose operator should no longer hold a key gets its
chip reloaded. Until T-B, step 3 places by home set and the prefetch is
the only placement; the key then stays until the slot is overwritten.

## Identity across sites

All cells of one network share the TPUI pool (`cell-protocol.md` §7.4),
which is already installation-wide, so a handset keeps its TPUI on every
cell. Across sites the identity plan is `architecture.md` §7's class B
route (one ARI per site, one PARK valid at every site), which is what makes
a handset accept another site's cell as "its" system at all. Without it
(today: each box's factory RFPI) a handset registered at cell X only
attaches to cells with X's identity, whatever the controller allows.
**That is a precondition of the shared network, not a detail:** with factory
identities, handsets do not roam between boxes.

## Open, for the maintainer

1. **Per-cell policy "accept foreign handsets"**: default **yes** (§0i:
   the shared network means yes). A per-cell opt-out is the maintainer's
   to decide; not built.
2. **The trust model** (`threat-model.md` §3 B8): **accepted** by the
   maintainer (§0i: "That's the way it is"). The opt-in per handset
   proposed here was not taken up; "keys only when needed" (above) limits
   the exposure instead. The controller side still thinks the opt-in
   matters for a handset whose owner does not want its key on strangers'
   boxes at all (worklog 2026-10-31); it is not built.
3. **Eviction priority for the operator's own handsets** (table above).
4. Whether the cell operator role needs its own permission beyond
   `may_add_cells`.

## Doc changes made with this ADR

`architecture.md` §3 (cell operator), §5 (the registration flow, ADR
0005); `cell-protocol.md` §7.4 (placement); `threat-model.md` (scope,
B8, §4.4). The schema changes (`cell.operator_user_id`,
`cell.accept_foreign`, the home set's new meaning) wait for this review.
