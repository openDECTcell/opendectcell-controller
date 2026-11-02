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
can serve it. Only the owner (the user who started the registration) can see and change the handset; an
administrator sees all.

## After registering: give it a line

Open the handset's page and choose a **VoIP profile** (a SIP account, see [SIP lines](SIP-lines.md)) as
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

The **INT list** of a handset shows the handsets of **your account**, including those that are on **other cells** at the
moment (they are listed by name, up to 32; the list is read again within seconds of a handset moving or being renamed).
**(Built, not yet tried on a handset.)** A call from one handset to another handset **is not supported yet**: the controller bridges between DECT and
SIP, and refuses an internal call with a message on the handset. Use the SIP side (call the other handset's
line number) if you need it.

## The phonebook

A CAT-iq handset's phonebook shows **your contacts** (Phones > Contacts) and the **shared** ones. What you add, change or
delete on the handset goes back to your personal contacts; the shared ones are read-only from a handset, and "delete all"
on the handset is ignored. A handset shows three numbers per contact: if a contact has more, they are kept. A change in
the web interface reaches the handset the next time it connects to a cell or within a moment if it is on one. **(Built, not yet
tried on a handset.)** **Call lists:** the missed, received and dialled calls of the lines a handset has (and what it dialled itself) are in the handset's lists, newest first, 40 per list, and follow it from cell to cell. They are read-only: deleting an entry on the handset is undone at the next update. **(Built, not yet tried on a handset.)**

## Names and numbers on the display

The caller's number and, if the SIP side provides it, the name are shown on the handset. CAT-iq handsets show
the line and the name; on some handsets the name appears only as vendor text. The controller learns a handset's
capabilities when it first talks to it; until then it may ring a CAT-iq handset without showing the number
(the log says "terminal capability is not known yet").

## Device profiles

A profile is a few settings for a model: for instance the A540 should not be sent list-change notices after its
location registration. Profiles are chosen when registering; the list of known profiles is built in. If your
handset misbehaves, say which model it is: a profile is the fix.
