# Decisions and questions for the maintainer, 2026-11-01

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

## Not decided by me, only noticed

- The signing key for releases (earlier open item), the 7412's PIN (1111) and
  autostart (enabled), and the lab 7520 on 08.25 are as the last notes say.
- `calllog` has no live writer: nothing in the controller writes call-log rows
  until the call path is finished, so statistics show real data only from then.
