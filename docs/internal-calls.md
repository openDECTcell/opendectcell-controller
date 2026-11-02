# Internal calls and presence

Calls between handsets of one account, and the "Online / Unresponsive / Offline" status of a handset.
Built 2026-11-02; **not tried on a handset yet**.

## 1. Internal calls

### The rule

A handset can call **only the handsets of its own account** (the account that owns it: a user's, or an
organisation's). There is no way to name another account's handset: it is checked where the number is
resolved (`core.resolveInternal`) and again where the call is made (`bridge.internalCall`), and a call
to anything else is released as "user unknown".

### Numbers

Every handset has an **internal number**, 200 to 999, the lowest free one among its owner's handsets
(migration 0018; a database trigger gives it, so every way a handset is created gets one). The web
interface shows it in the handset list and on the handset's page. A handset beyond the 800th of one
account has none and cannot be called internally. It is shown on the called handset as the caller's
number, with the caller's name.

What a handset dials after INT is one of three things, all resolved by the core:

| Dialled | Means | Who dials it |
|---|---|---|
| 200 and up | the internal number of a handset of the account, wherever it is | any handset: type it after INT |
| 64 to 127 | an entry of its INT list for a handset on **another cell** (`directory.go`) | an NG-DECT Part 3 handset (A540): choose the name in the list |
| 1 to 32 | a handset in **this cell's table**, by its record number | the same: the list entries of local handsets |

A Part 1 handset (the Gigaset AS690H, C430A) has no list from the base: it types the number after INT.

### How a call goes

1. The caller's stack (libopendectcell API 153, `cfg.bridge_internal`) sends `call-out class=int number=...`
   instead of running the call inside the base.
2. `core.callOut` resolves the number to a handset of the caller's account (`OutCall.Target`) or none.
3. `bridge.Manager.internalCall` rings the target through `Core.Ring` (any cell, the usual last-cell-first
   waves) with `call-in int=1`: the internal ring pattern, the caller's number and name, no line.
   A target in a call is busy; an internal call never waits.
4. The caller hears ringing; when the target answers, both handsets' audio channels are bound and the
   controller opens **both cells' speech** and joins them: a pipeline per cell, with an in-process relay
   between them (`media.NewRelay`). Both handsets wideband: G.722 end to end (passed through when the
   cells carry G.722); otherwise G.711 between the pipelines and each handset gets what it uses.
5. A hang-up on either side ends both. The call is in the caller's call log (direction out, the target's
   internal number) and in the web interface's call list.

### Not built / not tried

- Not tried on a handset or on air. The relay is tested in simulation (a tone from a narrowband to a
  wideband cell).
- No transfer, no second call. A call **between handsets on the same cell** takes the same path
  through the controller (the audio goes out to the controller and back): simple, costs a little delay.
- The called handset's name goes in the SETUP, not in a {CC-INFO} after it rings as stock does; an
  A540 may show it differently from stock.
- A missed internal call is not in the target's missed list (only the caller's call log has it).
- More than 32 handsets of one account on other cells: the INT list shows the first 32 by name; the rest
  can still be dialled by their internal number.

## 2. Presence

Shown in the handset list and on the handset's page.

| State | When |
|---|---|
| **Online** | heard within the registration interval (the roaming setting, 246 s by default, plus 30 s grace) |
| **Unresponsive** | it did not register again in time, or nobody answered a page for it |
| **Offline** | it sent **{DETACH}** (switched off) and nothing since, or it has been unresponsive for **10 minutes**, or it was never heard |

Worked out when the page is read from three times the DECT core keeps on the handset (`last_seen_at`,
`detached_at`, `page_failed_at`) and the interval: nothing is stored as a state, so there is no state to
get stuck. With the interval set to 0 (handsets are not asked to register again) silence says nothing.

- **{DETACH}** is a message a handset sends when it is switched off (EN 300 175-5 13.4.2). libopendectcell API 153
  counts it. It is not authenticated: someone with a handset can make another look offline until it is
  heard again; it never stops a call from being tried.
- **An unanswered page**: every cell the handset was paged on gave up without it ringing
  (`released by=base`). A cell that was down or busy does not count.
- Whether a given handset model sends {DETACH} on power-off is for the first hardware test to show.
