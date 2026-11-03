# Organizations and trust

This page is for users. It explains whom your handsets trust and how to share that trust with other people.

## What a base station's operator can see

A DECT base station can only serve a handset if it holds the handset's secret key. Whoever has administrator access to the box
(the person who runs that base station) can read the key, listen to the calls that pass through it, and see the numbers
dialled and who calls. **This is how DECT is built, not something this software can change.** The encryption between the box
and the controller does not help, because the box is the other end of the call.

A key that has been read stays useful until the handset is registered again. Taking a base station out of the controller does
not take the key back. If you suspect a key was read, register the handset again.

So: let your handsets use only base stations whose operators you trust.

## Whose base stations may my handsets use?

Open **Organizations and trust** in the menu. Three settings:

| Setting | Your handsets are put on |
|---|---|
| Every base station of this controller (the default) | any base station, also those of people you do not know |
| Only my own base stations | the base stations you enrolled yourself; without one, your handsets do not work |
| My own and those of the organizations I trust | yours, plus the base stations of every member of the organizations you tick |

When you save, your handsets are taken off the base stations that are no longer allowed. A key that sits in a base station's
chip stays there until the chip loads another handset or restarts; another reason to register the handset again if you worry.

## Organizations

An organization is a group of people, for example a makerspace or a family. It only exists so that you can say "I trust
these people's base stations". It gives its members **no** access to each other's handsets, accounts or base stations.

| You are | You can |
|---|---|
| Owner (whoever created it) | everything an admin can; appoint and remove admins; rename or delete the organization |
| Admin | create invitation tokens; remove members (not other admins, not the owner) |
| Member | see the overview; choose your own name for the organization; trust it or not; leave |

- **Create** one on the Organizations page: you become its owner and an admin.
- **Invite** people with an invitation: an admin creates one on the organization's page and hands over the link or the token.
  It works once, expires after 7 days and is shown only when it is created.
- **Join** by pasting the token on the Organizations page (or opening the link). You are shown the organization's name as the
  owner gave it and can call it something else; only you see your name for it.
- **See** the members' names and how many base stations each runs; above them the number of base stations in the organization
  with the online and offline counts under it, and a card with the same for the members' handsets.

Trusting an organization means trusting everyone in it now and later: when someone joins, their base stations count, and when
they leave or are removed, they stop counting.

## What stays the same

Handsets and accounts are always yours alone. An organization does not share them, and a controller administrator can still see
and manage every organization (needed when an owner's account is gone).
