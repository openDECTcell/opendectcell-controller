# Roaming and many cells

A handset registered at the controller works on every cell. This page explains what happens when it moves,
what you can set, and where the limits are.

## What was seen working

On a 7520 and a 7412 as two cells of one controller, with a Gigaset A540 and AS690H:

- When one box was switched off the handsets found the other within seconds and kept working there: they were
  located, answered pages, made and took calls with audio.
- Handsets that held the same temporary number on two cells were each given a new one where they clashed, and kept
  working in both directions.
- A call to a handset on the other cell reached it.

**(Not yet tried on hardware:)** handsets that choose a cell by walking around with several boxes on at once,
how fast a cell forgets a handset that left, and calling a handset that sits on a far cell while a nearer one is
up. These are built and tested with simulated cells and a database of 500 handsets over 100 cells, but have
not been walked with real boxes.

## How a handset moves

1. A handset that is registered keeps listening for a base it knows. All your cells show the same network name
   (the same DECT system identity), so it picks the best signal by itself. You cannot tell it to prefer a cell.
2. When it first talks to a new cell (a *location registration*), the cell does not know it yet. It asks the
   controller, receives the handset's record from the database and puts it in the cell's **working set** (its
   memory of the handsets that are around). Up to 26 handsets at a time per cell, beyond the six chip slots.
3. The cell tells the handset to register again after a while (**the registration interval**, default 246
   seconds). That is how the controller always knows where a handset last was: a handset that is switched off or
   left the range is noticed by it not registering.
4. A handset that was heard on another cell leaves the old cell's working set after a grace time, and one that
   was not heard for the interval plus a grace time leaves too.
5. Handsets that were placed in a cell only to be called, and never heard there, leave after 90 seconds.

A call that is **in progress does not follow** the handset: if the cell that carries it goes away or the handset
walks out of range, the call ends. There is no handover.

## The settings

**Administration > Settings**, roaming:

| Setting | Default | Meaning |
|---|---|---|
| Handsets register again every | 246 s | How often an idle handset re-registers. It is rounded to units of 40.96 s and can be at most 10445 s. A longer interval saves the handsets' battery; a shorter one notices a handset that left sooner. It does **not** keep a handset reachable: neither chip stops hearing an idle handset. |
| Drop a handset that did not, after another | 60 s | The grace after the interval before a cell forgets a silent handset. |
| Drop a handset that was heard on another cell, after | 180 s | How long the old cell keeps a handset that moved. |

## Incoming calls: where is the handset?

When a call comes in for a handset, the controller offers it first to the cell the handset was **last heard on**.
If that cell does not answer within three seconds it also tries the next 8 cells, then 8 more every three seconds,
**at most 24 cells in all**. The next cells are chosen from the cells the handset was heard on in the last week
(the most recent first), then the nearest to its last cell by the cells' latitude and longitude (set a cell's
position on its page; cells without one come last). The first to answer takes the call and the others are released.

With a few cells this is invisible: the one cell, or all of them, are tried. With hundreds, paging every cell would
flood the radios; the cap is why. A handset is almost always on the cell it was last heard on, because it registers
every few minutes.

## The temporary number (TPUI), and the 7412

A base calls a handset by a short temporary number, the **TPUI**, which it gives the handset when the handset
registers on the cell. The controller manages them so that it never gives the same number to two handsets that are
in one cell's working set; the same number may exist on different cells. This is why you can have **hundreds of
handsets**: the number of handsets does not depend on how many numbers there are.

The two chips differ here, and you should know the consequence:

- The **7520** chip hears a handset whatever its number is.
- The **7412** chip hears a handset only when the last digit of its number is 1 to 6 (in the hexadecimal notation).
  The controller only hands out such numbers: 90 of them per cell. The 7412 also keeps **one link per last digit**:
  two handsets with the same last digit are both reachable but **not both in a call or ringing at the same time**.
  With a handful of handsets you will not notice. For a ring group of many handsets on one 7412 cell, the ones that
  share a digit with a handset already ringing are paged again later; and a handset that rings while its digit
  partner is in a call may not ring until that call ends. A 7520 does not have this limit.

## Limits to plan with

| | |
|---|---|
| Calls at once per cell | 5 (7520), 3 (7412) |
| Handsets heard at once on one cell | 26 beyond the chip slots |
| Cells a call is offered to | 24 |
| Handsets in the controller | no fixed limit; tested with 500 handsets over 100 cells in the database |

Read [Limits](Limits.md) for what is planned.
