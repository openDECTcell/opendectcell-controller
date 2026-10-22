# UI mock-ups

Status: draft, 2026-10-22 (task A7). Pending maintainer review.

Wireframes to settle the API (`docs/api/openapi.yaml`), not the look. Each
screen lists the API operations it needs; a screen that needs something the
API lacks is a bug in one of them. Server-rendered with htmx
(`docs/adr/0001-web-frontend.md`); 🔄 marks a fragment that refreshes itself.

Navigation: **Overview · Handsets · SIP accounts · Calls** for everyone;
**Cells · Users · Audit log · Settings** added for admins.

## User

### U1 Handsets

```
Handsets                                             [ + Register a handset ]
┌──────────────┬────────────────┬──────────────────────┬──────────┬──────────┐
│ Name         │ Model          │ Lines                │ Last seen│          │
├──────────────┼────────────────┼──────────────────────┼──────────┼──────────┤
│ Kitchen      │ Gigaset A540   │ 1: Home  2: —        │ Cell 2·5m│ [Edit]   │
│ Desk         │ FRITZ!Fon C4   │ 1: Office 2: Home    │ Cell 1·1h│ [Edit]   │
└──────────────┴────────────────┴──────────────────────┴──────────┴──────────┘
```

`listHandsets`. Admins see an extra **Owner** column and a filter
(`owner=`, including *shared*).

### U2 Register a handset (the PIN window)

```
Register a handset
Cells it should work on:  [x] Ground floor   [x] First floor   [ ] Garage
                          (the handset's home set; more can be added later)
[ Admins: owner ▾ me / another user / shared ]
                                                        [ Start registration ]
```

```
Register a handset                                         🔄 every 2 s
  PIN   4 8 2 0 1 7          Window closes in 1:42
  1. On the handset: Menu → Settings → Registration → Register handset
  2. Choose the base station and enter the PIN above.
  Waiting for a handset…                                        [ Cancel ]
```

→ on success: "Registered *Gigaset A540*. Give it a name and assign lines." → U3.
On expiry: "No handset registered within 2 minutes." [Try again].

`listCells` (users get name/location only), `openRegistrationWindow`
(returns the PIN once), `getRegistrationWindow` (poll), `cancelRegistrationWindow`.

### U3 Handset details

```
Kitchen  (Gigaset A540, registered 2026-10-22)                      [Delete]
Name            [ Kitchen            ]
Line 1          [ Home ▾ ]        enabled [x]
Line 2          [ — none — ▾ ]
Default line    (•) 1  ( ) 2
Both lines ring at once:  (•) call waiting on the second   ( ) second is busy
Home cells      [x] Ground floor  [x] First floor  [ ] Garage
Features        Date/time [x]   Missed calls [x]   Message waiting [ ] (unsupported)
                                                                       [Save]
```

`getHandset`, `updateHandset`, `setHandsetLine` / `clearHandsetLine`
(the account dropdown is `listSipAccounts`: the owner's accounts, plus shared
ones for admins), `setHomeCells`, `listHandsetFeatures` / `setHandsetFeature`,
`deleteHandset` (confirm: "The handset stops working on every cell.").

### U4 SIP accounts

```
SIP accounts                                                  [ + Add account ]
┌────────┬────────────────────────────┬───────────┬───────────────┬─────────┐
│ Label  │ Address                    │ Transport │ Registration  │         │
├────────┼────────────────────────────┼───────────┼───────────────┼─────────┤
│ Home   │ 49301234@sip.example.net   │ TLS·SRTP  │ ● registered  │ [Edit]  │ 🔄
│ Office │ 210@pbx.office.lan         │ UDP       │ ● 403 Forbid. │ [Edit]  │ 🔄
└────────┴────────────────────────────┴───────────┴───────────────┴─────────┘
```

`listSipAccounts`, `getSipAccountStatus` per row.

### U5 SIP account form

```
Label [Home]   Display name [Paula]
User [49301234]  Domain [sip.example.net]  Auth user [        ]
Password [••••••••] (leave empty to keep)
Transport (•) TLS ( ) TCP ( ) UDP      Register [x]  every [600] s
Registrar [        ] (empty: from the domain, via DNS)   Outbound proxy [        ]
Media encryption ( ) off (•) optional ( ) mandatory
  ⚠ SRTP over UDP/TCP signalling sends the keys in clear      (only shown when it applies)
Codecs  [G722][PCMA][PCMU]  (drag to order)     DTMF (•) RFC 4733 ( ) INFO ( ) in-band
▸ Advanced: session timers, User-Agent, identity header, STUN/public address,
  local port, digit timeout, line prefix
                                                             [Save] [Delete]
```

`createSipAccount` / `updateSipAccount` (the response's `warnings` drive the ⚠),
`deleteSipAccount` (409 *in use* → "Used by: Kitchen line 1").

### U6 Calls

```
Calls                                      Handset [all ▾]  Since [2026-10-01]
 ↙ 10:42  +49 30 1234567   Kitchen · Home   3:12   G.722 · TLS/SRTP
 ↗ 09:15  210              Desk · Office    0:47   PCMA
 ↙ 08:01  +49 171 …        Kitchen · Home   missed
                                                              [Older calls]
```

`listCalls`. Admins also see the quality column (jitter, loss).

### U7 Account

Already built (`/account`): profile, and password change for local users.
`getMe`, `changeMyPassword`.

## Admin

### A1 Overview (admin home)

```
Cells    3 online · 1 pending approval · 1 degraded (RTT 180 ms)     🔄
Handsets 12 · 9 seen in 24 h
SIP      14 registered · 1 failing (Office: 403 Forbidden)
Calls    2 active · 41 in 24 h
```

`getStatsSummary`.

### A2 Cells

```
Cells                                                   [ + Enroll a cell ]
┌──────────────┬──────────┬────────┬─────────┬───────────┬──────────┬───────┐
│ Name         │ State    │ Health │ RTT p95 │ Slots     │ Software │       │
├──────────────┼──────────┼────────┼─────────┼───────────┼──────────┼───────┤
│ Ground floor │ active   │ ● ok   │  12 ms  │ 4/6       │ 0.9.1    │ [Open]│ 🔄
│ Garage       │ pending  │ —      │  —      │ —         │ 0.9.1    │[Review│
└──────────────┴──────────┴────────┴─────────┴───────────┴──────────┴───────┘
```

`listCells`, `getCellStats`.

### A3 Cell details

```
Ground floor · active · ● ok · cert valid until 2027-01-20
Name [Ground floor]  Location [Hall]  Max calls [1]  Plain RTP allowed [ ]
Capacity announced: 6 records, 4 NB / 4 WB audio
Subscription slots                            policy: auto (Settings)
  1 Kitchen   present  written 08:00  [pin]
  2 Desk      present                  [pin]
  3 —         empty
  …
[Disable]  [Revoke…]
```

Pending cells show "Approve / Reject" (`updateCell state=active`, `revokeCell`).
`getCell`, `updateCell`, `listCellSlots`, `updateCellSlot`, `revokeCell`
(confirm: "The cell is disconnected immediately and can never reconnect with
this certificate.").

### A4 Enroll a cell

```
Enroll a cell
Note [Garage repeater]   Valid for [24 h ▾]   Approve automatically [ ] (admins)
                                                            [Create token]
──
Enrollment string (shown once — copy it now):
  opendectcell-enroll:1:ctrl.example.org:7300:…:…                        [Copy]
On the cell: LuCI → Services → opendectcell → Controller → paste → Enroll.
```

`createEnrollmentToken`, `listEnrollmentTokens`, `deleteEnrollmentToken`.
Visible to users with *may add cells*; their cells always wait in *pending*.

### A5 Users

Already built read-only (`/admin/users`); C7 adds create, edit (role,
may-add-cells, disable) and delete. `listUsers`, `createUser`, `updateUser`,
`deleteUser` (409 *has objects* → "Owns 2 handsets and 1 account: reassign
them first").

### A6 Audit log

Already built (`/admin/audit`), with filters by object. `listAudit`.

### A7 Settings

```
Slot policy          (•) automatic eviction  ( ) manual only
Registration window  [120] s    PIN length [6] digits
Default max calls per new cell [1]
Keep audit log       [forever ▾]
                                                                    [Save]
```

`getSettings`, `updateSettings`.

## What this settled in the API

- Users need a **cell list without admin fields** for picking a home set
  (U2, U3): `listCells` returns reduced objects to users.
- The PIN and the enrollment string are **shown once**, so the create
  responses carry them and nothing else ever does.
- The SIP form needs **warnings** from the server (SDES without TLS), not a
  client-side copy of the rule: `SipAccount.warnings`.
- Deleting in-use objects returns a **409 with a reason code**, so the UI can
  say what blocks it.
