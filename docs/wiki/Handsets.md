# Register handsets

A handset is registered **once**, at the controller. It then works on every cell and moves between them.

## Registering

1. **Phones > Handsets**, then the registration page (**Register**).
2. **Name**: what the other handsets show in their lists, at most 15 characters.
3. **Manufacturer** and **Model**: choose them if listed. The *device profile* tells the controller how this
   model behaves. A handset that is not listed works as a generic DECT GAP/CAT-iq handset; ask for it to be
   added if it needs something special.
4. Press **Start registration**. The page shows a **PIN**, and a registration window of **one minute** opens
   on **every cell that is online**.
5. On the handset: *Menu, Settings, Registration, Register handset* (names differ by maker). Choose the base
   station, then enter the PIN. Some handsets try the PIN 0000 first and fail once; they then ask again.
6. When the handset is registered the page says so. It appears in **Phones > Handsets**.

If nothing happens, the window may have closed (press *Start registration* again), or no cell is online.

The handset registers on the cell it hears best; the controller stores its key and from then on every cell
can serve it. Only the owner (the user who started the registration) can see and change the handset. Handsets are
private: administrators do not see other users' handsets either, unless you tick **Disclose to administrators** on that
handset's page. They then see it, with its name, model, status and last contact (never its VoIP lines), under your
account in **Administration > Users**. It is off for every handset and you can switch it off again at any time.

If a handset cannot be registered because it is registered under another user's name (it was registered first, or it changed
hands), an administrator **releases** it: **Administration > Release a handset**, enter the IPEI from the label, see whose it is,
type the IPEI again. The handset is removed from its owner and from every base station, and the action is in the audit log.
Then register it again.

## After registering: give it a line

Open the handset's page and choose a **VoIP line** (a SIP account, see [SIP lines](SIP-lines.md)) as
**Line 1**; Line 2 is an optional second account that also rings the handset. Without a line the handset
registers but has nothing to call with. (Refusing external calls to a handset that has no line is still on the
task list: today the call may ring it anyway.)
A profile may be the line of several handsets: a call to it rings them all, and the first to answer takes it.

On a CAT-iq handset (for example the A540) the lines appear in its Line menu under the profile's name.

## What the handset page shows and lets you change

- Name, owner, manufacturer and model, **IPEI** (the handset's serial identity) and when it was **last seen**,
  and the cell it was last on, with its recent moves between cells.
- The two lines.
- **Deregister**: removes the handset from the controller and from every cell. The handset itself still thinks
  it is registered until you delete the registration in its menu (some handsets deregister themselves when
  asked; the A540's own-menu deregistration has not been tried yet).

## Calls between handsets

Every handset has an **internal number** (200 and up), shown in the handset list and on the handset's page. To
call another handset of **your account**, press **INT** and:

- on an **NG-DECT Part 3 handset** (A540): choose the handset's name in the list. The list also shows your
  handsets that are on **other cells** at the moment (up to 32, by name);
- on a **Part 1 handset** (Gigaset AS690H, C430A, which have no list from the base): type the other handset's
  internal number.

The other handset rings with the internal pattern and shows your internal number and name. **You can only call
the handsets of your own account**, never another user's. Internal calls run through the controller, also when both
handsets are on one cell. **(Built, not yet tried on a handset.)** No transfer, no second call yet.

## Online, unresponsive, offline

The handset list and a handset's page show whether the handset can be reached. **Online**: heard within its
registration interval (**Administration > Settings**, roaming: 246 s by default). **Unresponsive**: it did not register
again in time, or did not answer a page. **Offline**: it said it was switched off (many handsets send a message
when you switch them off), or it has been unresponsive for 10 minutes, or it was never heard. [Details](../internal-calls.md).

## The phonebook

A CAT-iq handset's phonebook shows **your contacts** (Phones > Contacts) and the **shared** ones. What you add, change or
delete on the handset goes back to your personal contacts; the shared ones are read-only from a handset, and "delete all"
on the handset is ignored. A handset shows three numbers per contact: if a contact has more, they are kept. A change in
the web interface reaches the handset the next time it connects to a cell or within a moment if it is on one. **(Built, not yet
tried on a handset.)** **Call lists:** the missed, received and dialled calls of the lines a handset has (and what it dialled itself) are in the handset's lists, newest first, 40 per list, and follow it from cell to cell. They are read-only: deleting an entry on the handset is undone at the next update. **(Built, not yet tried on a handset.)**

### Address books from your own server (CardDAV)

**Phones > Contacts > Address books** links an address book from a CardDAV server (Nextcloud, Radicale, iCloud, Google ...). The
controller **reads** it every 15 minutes to a day (your choice, and "Read it now") and copies the contacts here; nothing is ever
written to the server, and the copies cannot be changed here or on a handset. You can link several, each with its own server,
user and password (the password is stored encrypted).

On a **handset's page**, tick which address books that handset shows, the way you choose its VoIP lines. A handset's phonebook
holds up to 500 contacts: your own come first, then the shared ones, then the address books'. A book is read up to its first
500 contacts. Contacts of a book you unlink disappear from the handsets at once.

The address must be `https://` and must name the address book itself (for Nextcloud:
`https://host/remote.php/dav/addressbooks/users/NAME/contacts/`; use an app password). The controller does not connect to addresses
on its own machine or network unless the operator allows it (`OPENDECTCELL_CARDDAV_ALLOW_PRIVATE`, see the configuration).

**Names of callers.** When a call comes in, the handset shows the **name of the contact** whose number it is, if there is one
among the contacts that handset reads (yours, the shared ones and the address books ticked for it); it wins over the name the other
side sent. The Calls page shows the name next to the number, too. A number matches however it is written (`+49 30 1234567`,
`030 1234567`, `0049 30 1234567`); numbers of fewer than seven digits are never matched.

## Names and numbers on the display

The caller's number and, if the SIP side provides it, the name are shown on the handset. CAT-iq handsets show
the line and the name; on some handsets the name appears only as vendor text. The controller learns a handset's
capabilities when it first talks to it; until then it may ring a CAT-iq handset without showing the number
(the log says "terminal capability is not known yet").

## Device profiles

A profile is a few settings for a model: for instance the A540 should not be sent list-change notices after its
location registration. Profiles are chosen when registering; the list of known profiles is built in. If your
handset misbehaves, say which model it is: a profile is the fix.

A handset that is not plain GAP or CAT-iq may need more than a profile can say today. The Motorola O201 is the first
case: its profile sets the basic service of an incoming call to narrowband (it refuses the wideband setup) and asks for
an in-band indication on its outgoing calls (it keeps its audio closed without it). The plan for finding what such a
handset needs, a step at a time and from the standard, is in openDECTcell's
[vendor-specific-support.md](https://github.com/openDECTcell/opendectcell/blob/main/docs/vendor-specific-support.md) and the catalogue
of variations [handset-variation-catalog.md](https://github.com/openDECTcell/opendectcell/blob/main/docs/handset-variation-catalog.md).
