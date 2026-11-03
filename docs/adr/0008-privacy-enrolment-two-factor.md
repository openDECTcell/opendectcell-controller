# ADR 0008: Private handsets, disclosure, enrolment without approval, two-factor on known devices

Decided by the maintainer on 2026-11-03, in the order the questions came up.

## Decisions

1. **Handsets are private to their owner.** An administrator does not list, read or change another user's handset (`auth.IsOwner`,
   not `CanAccess`). The owner may **disclose** a single handset, or a single base station, to the administrators
   (`handset.disclosed_to_admins`, `cell.disclosed_to_admins`, both off by default). A disclosed entry shows under its owner in
   Administration > Users: a handset with name, model, status and last contact (never its VoIP lines), a base station with state,
   place, model and last connection. Disclosure shows; it gives no control.
2. **Base stations stay in the administrators' list.** They approve, disable and watch cells in Administration > Cells, which needs
   all of them; the operator is not shown (and not in the API's `enrolled_by`) unless the base station is disclosed. Only the
   operator sets the flag; an administrator's save of the page leaves it.
3. **Who may enroll base stations is a setting** (`cells.everyone_may_enroll`, on by default). Off: administrators and the users
   whose account has `may_add_cells`. **A cell that enrols with a token is active at once**: no administrator approves it. The
   token (single use, hashed, at most 7 days) is the permission. Cells that enrolled earlier without approval stay pending.
4. **Two-factor login is offered, not forced** (a banner, "Not now" for 30 days). Administrators with a local account are still
   required to have it, by the existing setting. With it on, the second step is asked on a **device it has not seen** and on a
   **known device every 21 days**. A device is a browser holding a random token in a cookie (`opendectcell_device`, HttpOnly, Secure
   when the site is https); the database keeps its SHA-256 per user (`trusted_device`). The login page lets the user decline to
   remember the device. Turning two-factor off, setting a new password, or "ask on every device again" forgets all devices.
   Recovery codes work like codes. Single sign-on users are not affected: the second factor is the provider's.
5. **QR code beside the key** when two-factor login is set up: an inline SVG made on the server from the same `otpauth://` link
   (`rsc.io/qr`, BSD-3-Clause), so no image file, no external service and no change to the content policy.

6. **After the review of the same day** ([code-review-2026-11-03.md](../code-review-2026-11-03.md)): personal contacts are private to
   their user like handsets; an administrator without two-factor, where it is required, is a plain user for the whole request;
   an administrator sees a base station's position and statistics only if its operator disclosed it; enrolment tokens follow
   their creator's right to enroll; at most 20 cells per operator who is not an administrator; the cells of a disabled user are
   switched off.

## Consequences and what was weighed

- *Approval-free enrolment* widens what a user can do: an enrolled cell can take handsets of users who trust "all cells". The trust
  setting (ADR 0007) is the user's protection; the default stays `all`.
- *Trusted devices* weaken two-factor a little (a stolen browser profile with the cookie and the password skips the code for up to
  three weeks); that is the price of the maintainer's choice of three weeks, and the cookie is HttpOnly and per user.
- An administrator can no longer repair a user's handset; deleting the user is the way out.
- The admin Calls page still shows every user's remote numbers; undecided.
