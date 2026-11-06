# Cells

A **cell** is a FRITZ!Box that runs the DECT radio for a controller. This page is about getting cells in,
keeping them healthy and taking them out.

## Enrolling a cell

1. In the controller: **Base stations > Enroll a base station** (visible to administrators, to every user while
   **Administration > Settings > Base stations > Every user may enroll base stations** is on, which is the default, and
   otherwise to the users an administrator ticked "May enroll base stations" for). Fill in a **note** (it becomes the cell's name if the cell sends
   none), choose how long the token is **valid** (1 hour, 24 hours or 7 days). Press **Create token**. A base station
   that enrols with the token is **approved automatically**: nobody has to approve it.
2. The page shows the **enrollment string**, starting with `opendectcell-enroll:v1?`, **once**. Copy it. The token
   list below shows tokens that are open, used or expired; you can delete one.
3. On the box: **Network > DECT**, tab *General*, **Make this box a cell…**, paste, **Enrol**. Or on the command
   line: `/etc/init.d/opendectcell enroll '<string>'` (or `enroll @<file>` with the string in a file). It takes up to a
   minute; the box then runs in cell mode.
4. Tick **Enable the cell** and save. The box boots the DECT chip and connects to the controller.
5. Nothing else: the cell is *active* as soon as it has enrolled. (A cell that enrolled with a token made before
   tokens approved by themselves is *pending*; an administrator approves it under **Administration > All base stations**.)

What happens underneath: the cell makes a key and a certificate request and shows the token; the controller
checks the token, and it issues a certificate (valid 90 days). From then on the
link is TLS 1.3 with **mutual** authentication, and the cell renews its certificate over the link at two
thirds of its life. The one-time token is no longer needed.

The cell must reach the controller at the address in the enrollment string (`OPENDECTCELL_CELLS_PUBLIC_ADDRESS`,
port 7300 by default). If it cannot, the box log says so; see [Troubleshooting](Troubleshooting.md).

## The cell list

**Administration > All base stations** shows, for every cell: its name, its **hardware** (the model), location, **state** (*pending*, *active*,
*disabled*, *revoked*), the **link** (up or down), the round-trip time (95th percentile), the software
version, the DECT **identity (RFPI)** and when it was last seen. Open a cell for its details: the chip build,
the calls, the handsets it holds, the audio capabilities.

On the cell's own page an administrator can **approve**, **disable** and **enable** it (a disabled cell is
kept but its radio is off). The owner's settings are on **Base stations**: the name, the location and
**Revoke**, which permanently removes the cell's right to connect (you must type its name to confirm; a
revoked box has to enrol again with a new token). A revoked cell stays in **All base stations** for three days, so you can
see what happened to it, and is then deleted automatically (its calls stay in the history, without the base station's name).

There is no call limit to set and no way to allow unencrypted audio. A cell takes as many calls as its voice
channels allow (the **Capacity announced** line shows them: records are handsets the chip can hold, the
narrowband and wideband numbers are the same voice channels, not two sets), and a call's audio is always
SRTP between the cell and the controller.

## Hardware and names

A base station tells the controller which box it is (for example *FRITZ!Box 7520*) each time it connects, so nobody has to type
it in. The owner can still enter another model on the base station's page; that wins. The model shows in both base station
lists, on the pages, on the public map (if the owner shares the specs) and in **Statistics** (administrators), which counts
the active base stations by hardware.

A box that has not been named reports its hostname, which on OpenWrt is *OpenWrt* until somebody changes it. If a base station
enrolled with exactly that hostname (the controller numbers a second one *OpenWrt-2* and so on) and still carries the name when
it has told its model and the identity its chip broadcasts, the controller names it after both:
**model plus the last five digits of the identity (RFPI)**, for example *FRITZ!Box 7520 72a00*. This happens once. Any other
hostname, and any name the owner typed later, is kept. The rename is in the audit log.

Owners see the same technical data of their own base stations as administrators do (link, software, capacity, identity,
certificate, counters), in the list and on the base station's page.

## Leaving cell mode on the box

On the box, **Network > DECT** shows *Leave cell mode…*. It erases the controller's key and certificate on
that box and runs it as its own DECT base again, with its own handsets and settings. To be a cell again it
needs a new enrollment string. Revoke the old cell on the controller as well.

## The DECT network identity

Base stations are **one DECT system**, with handsets that register once and move between them, only when they share
an **identity**. A base station without one keeps the factory identity of its box, and then every box is a DECT
system of its own: a handset registered on one box does not find another, and when that box is switched off or
unplugged the handset says *no base station*. The identity does not exist until an administrator creates it.

**On a new controller you are asked right after your first sign-in.** The controller proposes an identity (class B,
up to 255 base stations; the EIC is made up on the controller, or you type one you got from ETSI) and tells you what
to know before you accept:

- A made-up EIC cannot be checked against a list (ETSI publishes none), so it may belong to a real system. Near
  such a system, handsets of either may try the other's base station. Fine for a home, a lab or an experiment; for
  a service run for others, use an EIC allocated to you.
- The base stations reload their DECT chip with the identity when they next connect (a short outage each).
- **Changing the identity later makes every handset register again.** Until the first handset is registered it
  costs nothing, which is why the controller asks first.
- A Gigaset A540 did not find a class B base station again after a restart in one of our tests (a class A identity
  worked); on 2026-11-05 it followed from the 7520 to the 7412 on a class B identity without trouble. If you own such a handset, decline here, connect your base stations first and choose a class A identity
  on the Network identity page (up to 7 base stations).

**You can say no.** The page says what that means: the boxes keep their factory identities, each is a system of its
own, and the overview shows a warning until an identity is set. The question is not asked again. **Administration >
Network identity** sets or replaces it later; once handsets are registered that page asks you to confirm, more than
once, because every handset has to register again.

On a controller that already has handsets, nothing is proposed automatically (it would break them); the overview
warns that there is no identity, and the Network identity page does it with the confirmations.

Each base station gets an identity of its own, derived from the system identity (the same operator codes, a number
of its own per base station). **Administration > Network identity** also shows what is in use and whether the base
stations have taken it.

> **Handle with care.** Replacing the system identity makes **every handset registered so far lose its
> base** and register again. It asks for confirmation twice.

## Base stations for users, and the map

Administrators see every base station in **Administration > All base stations** (they approve, disable and watch them), but not who
operates it. The operator can tick **Disclose to administrators** on the base station's page; the base station then
also appears under their account in **Administration > Users**. Only the operator can change that.

**Base stations** in the menu shows the cells a user enrolled. An owner can choose what may be shown
on the **public map**: the cell's name, its position (exact, approximate or only the region), its
specifications and its statistics. **Public map** shows the stations that are shared. An administrator
decides under **Administration > Settings** whether the map is public (also without signing in) and where
the map tiles come from (none, OpenStreetMap, or your own tile server). The map is off by default.

## Monitoring a cell

The cell's own pages in LuCI (**Status > DECT**, tab *Cell*) show the chip and link state, the certificate's
expiry date and the counts of messages to and from the controller. On the controller, the dashboard and the
cell list show which cells are up. When a cell goes offline the controller can send an alert mail, if you set
up mail ([Controller install](Controller-install.md)).

## What is not there yet

There is no way to update the software of a cell from the controller: you update boxes with `apk` or a new
OpenWrt image, one by one. Rotation of the controller's certificate authority is not built either; see
[Limits](Limits.md).
