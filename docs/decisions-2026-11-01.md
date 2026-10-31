# Decisions and questions for the maintainer, 2026-11-01

## Answered 2026-11-01 (question tool) and what was done

| # | Answer | Done |
|---|---|---|
| 1 | Reload the chip at every cell start | as built; on the 7412 the stale keys are gone after a restart |
| 2 | Mask = slot 1 for the dummy key | as built (`00 01` seen on air) |
| 3 | Bench check alone on the 7412 first | done, pass on 06.88; the 7520 as a cell is waiting for an enrollment string (see below) |
| 4 | E-mail verification mandatory once SMTP exists | built, with resend and password reset |
| 5 | Sign-up password 12 characters or more | all local passwords now need 12 |
| 6 | OIDC only, no SCIM | nothing to do |
| 7 | Exact position by default | migration default `exact` |
| 8 | Add talk time to the shared statistics | in the public view |
| 9 | Duplicate station names allowed | migration 0006 drops the unique index |
| 10 | Admin picks the tile source | as built; plus a switch to make the map public |
| 11 | Default expiry of loud log levels 24 hours | first option in the form |
| 12 | Last three digits of a number at the calls level | as built |
| 13 | Opted-in users plus all admins get the log notice | built |
| 14 | German after the handsets page | handsets page is built; German is next |
| 15 | Show stubs with a badge | as built (Calls is the only stub left) |
| 16 | All of the above in their order | handsets page and public map, two-factor login, operator features: built |

## Answered later the same night (second round of the question tool)

| Question | Answer | Done |
|---|---|---|
| 7520 as a cell | I create a throwaway admin | done; three defects found and fixed (see `opendectcell/docs/bench-2026-11-01.md`), the throwaway admin deleted again, the lab 7520 is now a cell of your controller |
| Retention defaults | 90 days for call details, 365 for audit entries | the default of an installation that never saved the setting; clearing both fields keeps everything |
| Two-factor for admins | enforce | on by default; switch in Settings; `opendectcell-controller user reset-2fa --username NAME` for a lost app |
| Images | my leans | one release 25.12.6, images only on tags or manual runs, a deploy key for the PCM module repo (yours to create, see `opendectcell/docs/images-plan.md`); the image script works locally for the 7520 target, not flashed |

## New questions from the night's work (none blocks) — the first four are answered above

- **7520 as a cell.** It needs an enrollment string from your controller, or your OK for me to make a throwaway admin on it (the sandbox refused me to do that on my own). Then: install `opendectcell-cell`, enrol, restart, read the audit on 08.25 (the cold-chip question), leave cell mode again.
- **Retention defaults** keep everything. Recommended values (90 days for call details, 365 for the audit log) are in the settings text. Make them the default for a fresh installation?
- **Images** (`opendectcell/docs/images-plan.md`): I1 one OpenWrt release for both boxes? I2 the PCM module's repository for CI? I3 what goes in the default image? I5 images only on tags and manual runs?
- **Two-factor for admins:** a notice on the dashboard today. Enforce it (an admin without it cannot use admin pages), or leave it a recommendation?
- **Public map:** off until an admin ticks a box; the JSON feed answers `Access-Control-Allow-Origin: *` when public. Fine?
- **The CI-minutes rule I added:** docs-only pushes skip CI. Say if you want them to run.

---

## Original questions (kept for the record)

Collected while working alone overnight. **Nothing here blocks**; each item says
what I did meanwhile and what changes if you answer differently. Answer in bulk
whenever it suits; short answers ("1 b, 4 yes") are enough.

**About your cut-off sentence.** It ended "…caters both to the privacy loving
open source user and the". I read the rest as *the professional operator who
hosts many cells* (a makerspace network, a hosting provider). If you meant
someone else, say who; `docs/ui-backlog.md` is written from that assumption.

## Cell function (7520 and 7412)

1. **Fresh chip at every cell start.** `start_service` now forgets that the chip
   was booted for `role=cell`, so the chip is uploaded again and its key table
   starts empty. Cost: a few seconds at each `restart`, and a cold chip (the
   first key write on 08.25 may not take). (a) as built, (b) only when a box is
   enrolled as a cell, not on every start. *I kept (a): simplest, never stale.*
2. **The dummy key is in the locked-in mask.** The cell writes a random key into
   slot 1 and sets the mask to slot 1 (07.56 stays silent on an empty table; the
   mask question was never separated from it). Alternative: leave the mask at 0.
   *Bench check, needs both boxes and about 40 minutes with you:* see
   `opendectcell/docs/tasks-2026-10-31.md`, L8.
3. **Not tried on air.** Built, unit-tested (golden traces, a new dummy-key test),
   not installed on either box. Do you want me to run it alone on the 7412 first
   (needs the controller running here and a package build), or together?

## Sign-in and registration

4. **E-mail verification for self-registration.** The address is not verified: no
   mail goes out until an SMTP server is configured. (a) leave it; admins use
   "approve each account" when the instance is open; (b) make verification
   mandatory once SMTP exists (backlog B7, B8). *Built: (a).*
5. **Password rule** for sign-up is the same 10 characters as everywhere. OK?
6. **Authentik** is OIDC only. Do you also want it to be able to *create* users
   from its side (SCIM)? Not planned.

## Base stations and the public map

7. **Default precision of a shared position** is "about 1 km" (changed from
   exact before release). OK, or should the default be exact?
8. **Statistics when shared**: counts only (connected handsets, uptime, number of
   calls), no call times, no numbers, no handset names. Anything else you want on
   the map, or less?
9. **Who may rename or move a base station**: its enrolling user and admins.
   Names are unique across the controller (case-insensitive); a clash is
   refused. Fine for a public map?
10. **Public map hosting.** Tiles are off by default. For a public map of many
    stations a tile server of your own is the clean answer (backlog B18 proposes
    a caching proxy inside the controller). Which direction?

## Logging

11. **Default expiry** of debug and calls levels is one hour in the form; admins
    can choose 6 h, 24 h or "until changed". OK?
12. **Calls level** writes three lines from the call log (started, answered,
    ended) with the number masked to its last three digits. The DECT and SIP
    sides write nothing at that level yet (backlog B15). Mask more strictly
    (nothing but the length)?
13. **Who gets the change e-mail**: only users who opted in on the Logging page.
    Admins are not added automatically.

## Interface

14. **Language**: English now; German next. I need a string table first (B16),
    which touches every template. Do it before or after the handsets page?
15. **Stubs are visible to users** with a "soon" badge. Hide them from users
    until built?
16. **Next**: handsets page (B1) and public map (B3) first, or two-factor login
    (B4)?

## Roaming (added after "implement 1 to 5")

17. **Pooling is on by default** for handsets that enrol from now on (`cells.handset_pool`). It has run in the
    library's tests and the controller's, not on air. Leave on, or default off until bench B1 passes?
18. **Handsets enrolled before stay on their home cells**, in chip slots, with the old TPUI. There is no migration
    (a handset takes a new TPUI only at its next locate, and its key would have to leave the chip). Re-enrolling a
    handset through a window makes it pooled (same IPUI, same owner). Want a button for it?
19. **Limits of the pool**: 249 pooled handsets in the installation (TPUI `{00,11,N}`, N 7 to 255) and ten on a cell
    (record numbers 7 to 16). A cell that is full says so in the log and does not hold the handset. Enough for a
    makerspace; a hosted installation with hundreds of handsets per cell needs a bigger TPUI form, which is a
    question for the clean-room Q&A.
20. **A cell rebuilds its stack 3 s after a pooled handset enrolled on it** (radio stays on; a call in progress on
    that cell would end). Fine while there is no call path; once there is, the rebuild should wait for an idle cell.
21. **Records numbered 8 or more have no entry in the handsets' Internal Names list and no call list** (library
    limits: eight names, seven terminal ids). Widening them is library work; do you want it?
22. **Location history**: every change of cell is a row (handset, from, to, time), shown on the handset page and
    counted in the statistics, and erased with the call details after the retention period (90 days by default).
    OK as location data, or shorter?

## Not decided by me, only noticed

- The signing key for releases (earlier open item), the 7412's PIN (1111) and
  autostart (enabled), and the lab 7520 on 08.25 are as the last notes say.
- `calllog` has no live writer: nothing in the controller writes call-log rows
  until the call path is finished, so statistics show real data only from then.
