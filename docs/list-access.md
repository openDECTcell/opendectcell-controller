# CAT-iq list access: controller side

Status: draft, 2026-10-22 (opendectcell-controller#49); §1.1 added 2026-10-31. **The Contact List is wired 2026-11-02** (§5); the call lists are not yet.

A CAT-iq handset reads and edits lists held by the base: the phonebook and
the call lists (ETSI TS 102 527-3). The transport (`<<IWU-TO-IWU>>`
sessions, `dect_iwu.c`) is verified on hardware, but the record layout is
**not decoded** (`c3 04` unhandled; payload fields unknown). So this note
models what a *user* needs, and the DECT wiring waits for #46.

These sessions are never calls: they arrive as `<<BASIC-SERVICE>>` class
`0x2` and are refused by the SIP bridge (`internal/bridge`, #48).

## 1. Lists

| List (TS 102 527-3) | Held in | Read by a handset |
|---|---|---|
| Contacts | `contact` + `contact_number` | the owner's personal contacts **and** the shared contacts |
| Missed calls | `call_log`, incoming, never answered | calls on the SIP accounts of the handset's lines |
| Incoming accepted calls | `call_log`, incoming, answered | the same |
| All incoming calls | union of the two | the same |
| Outgoing calls | `call_log`, outgoing | calls placed **from this handset** |

Line settings and system settings, also lists in the standard, come from
the handset and line configuration (#31). They are not separate data.

### 1.1 Internal Names List in the controller (decided 2026-10-30)

A standalone box lists every handset registered on it, and any handset may
rename any entry. That is fine for a desk. In the controller it is not:

| Rule | Detail |
|---|---|
| **A handset sees only the handsets of its own user's account** in its Internal Names List, itself included | A handset of a user with two handsets sees two entries; a team's handsets, owned by one organisation or admin account, see each other. Never another user's. Every handset has an owner (2026-10-31, the maintainers' notes) |
| **No handset can rename any entry** | The entry's Name field is not editable (properties bit 7 clear) and a save is answered "procedure not allowed". Names are set in the admin UI and the user's own page |
| **Internal calls stay inside the account** | The list is also the dial list for internal calls, so a handset can only call handsets in its list. Calls to anyone else go through the SIP server like any other call (`AGENTS.md` §1). *Confirmed by the maintainer 2026-10-31 (the maintainers' notes point 6):* short internal calls within one user's handsets are fine; nothing across accounts, no PBX features beyond that |
| A standalone box keeps today's behaviour | Everything visible, renaming allowed |

The names themselves belong to the controller's handset record (the
admin and the owner edit them), not to what a handset saved. The library
has the hook since API 138: `name_visible()` in `struct dect_base_ops` (by
the user's account), `cfg.names_readonly`, and `dect_base_names_refresh()`
after an account change (openDECTcell `docs/notes-for-the-controller.md`, "The
protocol is complete; the handsets in the controller"). Unit-tested, not on
air; the fallback there is to ignore a rename in `named()`.

## 2. Decisions

| Decision | Alternatives | Reason |
|---|---|---|
| **Contacts belong to a user, or are shared** (`owner_user_id` NULL, admin-managed), like handsets and SIP accounts | Per handset; one global list | A person with two handsets wants one phonebook. A company wants a reception list everyone sees. Per-handset lists would duplicate both |
| **A handset reads its owner's contacts plus the shared ones.** A team's handset reads its organisation account's contacts plus the shared ones (2026-10-31: no handset is without an owner) | Shared handsets also read nobody's / everyone's personal lists | Personal contacts are personal data (`threat-model.md`, S12) |
| **Edits from a handset** go to the owner's personal list; the shared contacts are read-only from every handset. *Changed 2026-10-31:* a team's handset (owned by an organisation account) edits that account's list, where it used to be read-only | Shared handsets edit the shared list | Changing everyone's phonebook should need the admin UI. A team that wants its handsets' list read-only keeps its contacts shared instead |
| **Incoming call lists follow the line, not the handset.** A handset sees the calls on the SIP accounts assigned to its lines, including those another handset answered or that rang everywhere and were missed | Per handset | That is what a line is: on CAT-iq the missed-call list is per line, and #44 (missed-call notification) counts per line |
| **Outgoing calls follow the handset** | Per line | "What did I dial" is per device, as on a mobile phone |
| **One "read" mark per missed call** (`call_log.missed_read_at`), set when any handset or the UI marks it read | Per-handset read marks | The standard's read status is per entry; per-handset state would make the missed-call count differ between handsets on one line, and #44 needs one count |
| **Entry identifiers:** contacts have a stable `bigint` id; the DECT layer maps them to the 16-bit entry ids the protocol uses, per session | Expose 16-bit ids in the database | The wire format is unsettled; the mapping belongs with it |
| **A contact has up to 5 numbers**, each with a kind (`fixed`, `mobile`, `work`, `other`) and one default | Free-form JSON | CAT-iq contact entries carry a few typed numbers; 5 covers every handset seen |

## 3. API

| Endpoint | Rule |
|---|---|
| `GET /contacts?scope=personal\|shared` | own + shared; admins may filter by `owner` |
| `POST /contacts` | creates a personal contact; admins may set `owner_id` / `null` (shared) |
| `GET/PATCH/DELETE /contacts/{id}` | owner-or-admin; shared contacts readable by all, writable by admins |
| `GET /handsets/{id}/lists/{list}` | owner-or-admin; the view a handset reads (§1). Waits for handsets (C3) in the API, but the store function exists and is tested |
| `POST /calls/{id}/read` | marks a missed call read; owner-or-admin of the call |

## 4. Open

- The `c3 04` record and the payload field meanings (cell side).
- Melodies, photos and per-contact line selection, which some handsets
  support. Left out until a handset needs them.

## 5. The Contact List as built (2026-11-02)

libopendectcell API 151 gives each record a Contact List of its own (`dect_base_record_contacts`); the handset a record serves reads
that one. The controller keeps the contacts in the database and the stack's lists follow:

- **Format.** `contacts.FileText` writes a handset's list as the library's contacts file (`dect_contacts.h`): the handset owner's
  contacts and the shared ones, three numbers per entry (the default first; the database allows five), names cut to 31 bytes at a
  character, a leading `+` as `00`. A handset's change comes back as one journal line (`put`, `del`, `clear`), `contacts.ParseChange`.
- **When a handset gets its list.** When it comes into a cell's table, when a stack starts, and whenever its owner's contacts (or a
  shared one) change in the web interface or from another handset (`Contacts.OnChange` → `core.ContactsChanged` → every cell
  refreshes its handsets of that owner).
- **What a handset can change.** Its owner's personal contacts: make, edit, delete. The shared contacts are read-only and "delete
  list" is refused (a handset must not empty the address book). Numbers beyond the three it sees, and a name it saw cut, are kept
  when it edits the contact. A contact it makes keeps its entry identifier on that handset (the file's hint).
- **In a worker process** the list crosses the socket as one message (at most about 120 KB, 500 entries), the stack's journal lines
  come back as `ContactsChanged` callbacks on the cell's goroutine.
- **Tested:** the format and the changes against the database; the real library through cdect (load, refuse, journal); the core
  (start, arrival, store a change and give the list again with its sequence and hint, refresh only one owner's handsets). **Not seen
  on a handset yet:** open the phonebook on the A540 (a `Testkontakt` was added for the lab's owner), edit and delete an entry.
- **Open:** handsets that open the phonebook while a list is replaced are not specially handled (the library keeps their
  locks, an entry being edited keeps the handset's version).

## 6. The call lists as built (2026-11-02)

The library keeps one set of call lists per base, each entry saying which terminal (record slot) sees it, on which line. The
controller gives it, from the database, the calls of **every handset in the cell's table**: `contacts.CallsFile` builds the
library's call lists file (`dect_calls.h`: missed, received, dialled; incoming by the lines a handset has, outgoing by the
handset; one entry for a call several handsets see, each on its own line; newest first, 40 per list; times in UTC), the stack
loads it (`SetCalls`; a handset with more unread missed calls than before is told, so its missed-call display updates). It is
given when a handset comes into a cell's table, when a stack starts, and within half a second of a call of one of those handsets
ending (`bridge` → `core.CallListsChanged`, a burst is one load), so the lists follow a handset from cell to cell.

- **Read-only from the handset.** What a handset does to its lists (reads or deletes an entry) is not sent back: the next load
  shows the database again. The missed calls' read mark (`call_log.missed_read_at`) is still set from the web interface only.
- **Not seen on a handset yet.** Tested: the file against the database, the real library through cdect, the core (start, arrival,
  burst of calls), the bridge (a finished call tells the core).

