# ADR 0005: How a handset registers, and on which cell

Status: **decided** by the maintainer 2026-10-31 (the maintainers' notes point 5, **superseded on the choice of cell by §0i**). The first version (the user picks one cell, one window per cell, `409 cell_busy`) is replaced: registration opens on **every cell that can take a handset**, with **one PIN per user's window**, several PINs live at once. Built in C3 step 4 (worklog 2026-10-31, "registration windows").

## Why the controller cannot pick the base

A handset sends **nothing** before its user starts "register" on it, so the
controller has no handset to detect and no signal to choose a cell by.
Registration is the handset's initiative: it scans for bases that
advertise open registration, sets up a link to one, and only then asks its
user for the PIN; the base checks the handset's reply against the key
derived from a PIN (openDECTcell `notes-for-the-controller.md`, "Several
registration PINs at once"). So the PIN is not needed to find the base, and
a base can try several PINs against one reply: which one matched names the
user. The controller does not need to know where the handset is.

## The flow

| # | Who | What |
|---|---|---|
| 1 | user | "Register handset" in the UI. No cell to pick. Chooses 8 digits (default) or 6 if the handset refuses 8. An admin may register on behalf of another owner (an organisation account) |
| 2 | controller | Checks: no live window of this user (`409 window_open`), the user's rate limit, a free PIN index. Issues a **random PIN**, 8 (or 6) digits from the CSPRNG, unequal to every live PIN; lifetime **60 s**, single use. Shown once |
| 3 | controller | Every cell's stack gets the list of live PINs (openDECTcell API 139, `reg_pins`). Registration is **open on every cell that can take a handset** (a free record and chip slot, not locked out) while any PIN is live: the cells' window closes 60 s after the latest opening |
| 4 | user | Starts "register" on the handset, types the PIN. The handset picks whichever open base it reaches |
| 5 | stack | Access rights, authentication against every live PIN, key allocation; the record is saved with `reg_pin1` = 1 + the index of the PIN it proved |
| 6 | controller | `reg_pin1` names the PIN, the PIN names its user: the handset is bound to that user. The PIN is spent and leaves every cell's list; if no PIN is live, every cell closes |
| 7 | user | The UI (polling `GET /registration-windows/{id}`) shows the new handset; the user names it and assigns lines |

**Ends without a handset:** the PIN's 60 s pass, or the user cancels. An
enrolment the controller cannot bind to a live PIN is refused and the
cell's stack rebuilt (`0c6715d`).

## The PIN index and the timing

The library copies the PIN list into a link when the link is set up; a
change takes effect for the next link (openDECTcell API 139). So `reg_pin1`
refers to the list the link was given, which may be up to one procedure
older than the controller's. The controller therefore:

- gives each PIN a **fixed index** for its life (the list has empty
  entries for unused indices, which the library skips);
- does **not reuse an index for 120 s** after its PIN left the lists, so an
  old link's index never names a newer user's PIN;
- accepts an enrolment whose PIN **expired at most 30 s ago** (the link was
  set up while it was live; the procedure finished late). A PIN that was
  spent or cancelled is never accepted.

32 indices (the library's maximum) and the 120 s quarantine allow about 32
windows per three minutes network-wide; beyond that `503 busy`.

## Limits

| Limit | Value | Why |
|---|---|---|
| Live windows per user | 1 (`409 window_open`) | one handset at a time; cancel to get a new PIN |
| Windows per user per hour | 10 | PIN guessing by re-opening |
| PIN lifetime | 60 s, single use, never extended | maintainer, §0i |
| Cells' window | open while any PIN is live: until 60 s after the latest opening | maintainer, §0i: one global timer |
| Live PINs | 32 network-wide (with the quarantine above) | the library's `DECT_MM_REG_PINS_MAX` |
| Failed attempts per cell | 5 within 60 s close that cell's registration for 5 min, logged | the library has no limit; each attempt costs a radio link. A blind attempt hits with probability `live PINs / 10^8` (`/ 10^6` with 6 digits) |

The attempt limit is counted from the library's log line
`reg: registration rejected` until the library offers a callback (asked as
a `Controller Q:`). A handset model that tries PIN 0000 by itself first
(the library says so in that line) is not counted.

## Cells that cannot take a handset

Registration writes the new key into a chip slot. A cell whose records
are all in use (`dect_base_free_record()` finds none) is **not opened**.
Freeing a slot by overwriting an idle handset's (a clear does not wipe the
key bytes, Q-0073) is eviction, `cell-protocol.md` §7.4: not built.

## Open

1. **A handset registering again under another user** (sold, passed on):
   refused today (`0c6715d`). Proposal: allowed once the old owner (or an
   admin) has deleted the handset, which frees its IPUI; no silent
   transfer. Not answered.
2. **Whether an open window disturbs a live call**: nothing suggests it (a
   flag in the base's broadcast), but it is unconfirmed on air; the bench
   day of 2026-11-01 tests it.
3. **The lockout is a lever for a nuisance**: someone near a cell can keep
   its registration closed by failing five times a minute. The other cells
   stay open, which is why the lockout is per cell.

## Doc changes made with this ADR

`architecture.md` §5, `openapi.yaml` (`/registration-windows` without a
cell), `threat-model.md` B1 and §5.
