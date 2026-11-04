# How it fits together

## The pieces

```
  handset ~~~ DECT radio ~~~ [ cell: FRITZ!Box + opendectcell ]
  handset ~~~ DECT radio ~~~ [ cell: FRITZ!Box + opendectcell ]  ===== network =====  [ controller ] === SIP === provider / PBX
  handset ~~~ DECT radio ~~~ [ cell: FRITZ!Box + opendectcell ]                         web UI, database
```

- **Handset**: an ordinary DECT cordless phone (GAP or CAT-iq standard).
- **Cell**: a FRITZ!Box running OpenWrt with the `opendectcell` and `opendectcell-cell` packages. It runs the DECT
  radio and nothing else: the DECT protocol stack, the SIP client and all settings live on the controller.
  The cell and the controller talk over TLS 1.3 with mutual authentication (port 7300); the audio of a call
  goes between them encrypted (SRTP).
- **Controller**: one server (Docker, with a PostgreSQL database). It runs the DECT stack for every cell, keeps
  the handsets, owns the SIP accounts, bridges the audio between DECT and SIP, and serves the web
  interface. **It is not a PBX**: dial plans, voicemail and call routing stay with your PBX or provider.
  Every handset appears there as one or two ordinary SIP user agents.
- **VoIP line**: a SIP account you set up in the web interface. A handset has up to two lines (**Line 1** and
  **Line 2**), each holding one VoIP line. A call to a VoIP line rings the handsets that have it as a line; a call
  from a handset goes out on its default line, or on the other if the handset chooses it.

Without a controller the same box can also run on its own as a base with a built-in SIP client (up to six
handsets). See [Standalone](Standalone.md). The two modes are not mixed on one box: enrolling a box as a cell
puts its own handsets and settings aside; leaving cell mode brings them back.

## One handset, many cells

A handset is registered **once**, at the controller, and then works on every cell. All cells of a controller
share one *DECT system identity* (the same operator codes), with a number of their own each, so a handset
that has registered sees them all as one network and moves between them by itself, like between the towers
of a mobile network. The handset decides which cell to use, by signal strength; the controller follows:
it notes where the handset was last heard and offers an incoming call there first. More:
[Roaming and many cells](Roaming-and-many-cells.md).

A call that is in progress **does not move** with the handset: if the cell it is on goes away, the call
ends. DECT has handover, but openDECTcell does not implement it.

## Who may do what

- **Users** register their own handsets, set up their own SIP accounts (VoIP lines) and see their own calls.
  A user may enrol cells only if an administrator allowed it for them.
- **Administrators** approve cells and users, change settings, see all calls, the audit log and the backup page.
- Sign-in is by local account (password, optional second factor with an authenticator app) or by single sign-on
  (OpenID Connect, for example Authentik) with groups mapped to roles.

## Words you will meet

| Word | Meaning |
|---|---|
| **Enrollment string / token** | A one-time secret that lets one box become a cell of this controller. Shown once, valid for a time you choose. |
| **RFPI** | The identity a base broadcasts on air. Every cell has its own, derived from the system identity. |
| **IPEI** | The handset's own serial identity, shown on its page. |
| **PIN (registration)** | Eight random digits (or fewer on request) you type into the handset while its registration window is open. |
| **Device profile** | A small set of habits for a handset model (for example whether it needs list-change notices). Chosen when you register a handset. |
| **TPUI** | The short temporary number a base pages a handset by. It is handed out by the controller and you normally never see it; see [Roaming](Roaming-and-many-cells.md). |
| **Working set** | The handsets a cell currently holds in memory: those that are around. |

More in the [Glossary](Glossary.md).
