# ADR 0005: How a handset registers, and on which cell

Status: **flow decided** by the maintainer 2026-10-31 (the maintainers' notes point 5); **the details below are proposed**, for review. Built in C3 step 4, after the review.

## Why the controller cannot pick the base

A handset sends **nothing** before its user starts "register" on it, so the
controller has no handset to detect and no signal to choose a cell by.
Registration is the handset's initiative: it scans for bases that
advertise open registration, sets up a link to one and sends its
access-rights request with the PIN its user typed. (Standard DECT behaviour,
as handsets show it in practice; not measured by us with two cells.) So the
only lever the controller has is **which cells advertise open registration,
and with which PIN.**

## The flow

| # | Who | What |
|---|---|---|
| 1 | user | "Register handset" in the UI. **Picks the cell they stand at**: their own cells first, then cells that accept foreign handsets (ADR 0004), searchable by name and location. A user without a base sees only the second list. An admin may register on behalf of another owner (an organisation account) |
| 2 | controller | Checks: the user may register (rate limit), the cell is active and connected, it accepts foreign handsets or is the user's, **no other window is open on it** (`409 cell_busy`: the library has one PIN per base). Creates the window: owner, cell, PIN, expiry |
| 3 | controller | **PIN: 8 digits**, from the CSPRNG, single use, valid for this window only; kept as a hash, shown to the user once. Tells the cell's stack to open registration with it (on the selected cell only) |
| 4 | user | Starts "register" on the handset and types the PIN. The handset picks the cell that advertises open registration: the selected one |
| 5 | stack | The access-rights request, authentication with the PIN, the key allocation; the record is saved (`Save` with a new index) |
| 6 | controller | The enrolment is bound to the window's owner (`Core.Window`, built in `0c6715d`); the handset row, its TPUI from the pool, the cell as its first prefetch cell. The window closes on every cell it was open on, and the PIN is spent |
| 7 | user | The UI (polling `GET /registration-windows/{id}`) shows the new handset; the user names it and assigns lines |

**Ends without a handset:** expiry (default 120 s, as today), the user
cancels, or the cell goes down; the cell's registration closes, the PIN is
void. An enrolment that reaches the core with no window open is refused and
the stack rebuilt (built, `0c6715d`).

## Scope of a window

- **One cell**, the default and the only form in v1.
- **"All cells of a site"** may come later (the maintainer allows it), for
  a user who does not know which box is nearest. It needs a site in the
  data model (today `cell.location` is free text) and the
  one-window-per-cell rule on each of them.
- **Never the whole network.**

## Limits

| Limit | Value (proposed) | Why |
|---|---|---|
| Open windows per user | 1 (as `openapi.yaml` today) | one handset at a time |
| Windows per user per hour | 10 | stops a user keeping a busy cell's registration open, and PIN guessing by re-opening |
| Windows per cell | 1 at a time | the library's one PIN per base |
| Window length | 120 s, at most 300 s | a radio attacker near the cell has only that long (`threat-model.md` B1) |
| PIN attempts | one window = one PIN; a failed attempt does not extend it | the DECT procedure limits the rate of attempts per link |

## What the library must offer

Opening and closing registration with a PIN **on a running stack**. Today
the PIN is part of `stack.Config` at build time; S7.3 moves "the register
window" into the library (openDECTcell `docs/stack-in-library.md`). Step 4 waits
for that stage or asks for the call as a `Controller Q:`; it does not copy
`dect-stubd`'s glue (the maintainers' notes point 1).

## Open, for the maintainer

1. **A handset registering again under another user** (sold, passed on):
   refused today (`0c6715d`). Proposal: allowed once the old owner (or an
   admin) has deleted the handset, which frees its IPUI; no silent
   transfer.
2. **Two windows on neighbouring cells of one site**: a handset in
   register mode may lock onto the other user's cell, fail on the PIN and
   leave its own user puzzled. Does a failed attempt close the other
   user's window in the library? Not known; a question to the cell side
   when step 4 starts. One window **per site** would avoid it, at the cost
   of the site's other users waiting.
3. **PIN length fallback**: 8 digits; §1h allowed 6 or 4 for a handset that
   will not take 8. Kept as an admin option, not offered to users.

## Doc changes made with this ADR

`architecture.md` §5 (this flow), `openapi.yaml` (one cell per window,
`409 cell_busy`), `threat-model.md` B1 and §5 (8 digits, the rate limit).
