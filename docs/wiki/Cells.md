# Cells

A **cell** is a FRITZ!Box that runs the DECT radio for a controller. This page is about getting cells in,
keeping them healthy and taking them out.

## Enrolling a cell

1. In the controller: **Base stations > Enroll a base station** (visible to administrators, and to users an
   administrator allowed to enrol cells). Fill in a **note** (it becomes the cell's name if the cell sends
   none), choose how long the token is **valid** (1 hour, 24 hours or 7 days) and whether the cell is
   **approved automatically**. Press **Create token**.
2. The page shows the **enrollment string**, starting with `opendectcell-enroll:v1?`, **once**. Copy it. The token
   list below shows tokens that are open, used or expired; you can delete one.
3. On the box: **Network > DECT**, tab *General*, **Make this box a cell…**, paste, **Enrol**. Or on the command
   line: `/etc/init.d/opendectcell enroll '<string>'` (or `enroll @<file>` with the string in a file). It takes up to a
   minute; the box then runs in cell mode.
4. Tick **Enable the cell** and save. The box boots the DECT chip and connects to the controller.
5. Unless you chose automatic approval: **Administration > Cells**, open the new cell, press **Approve**.

What happens underneath: the cell makes a key and a certificate request and shows the token; the controller
checks the token, and once the cell is approved it issues a certificate (valid 90 days). From then on the
link is TLS 1.3 with **mutual** authentication, and the cell renews its certificate over the link at two
thirds of its life. The one-time token is no longer needed.

The cell must reach the controller at the address in the enrollment string (`OPENDECTCELL_CELLS_PUBLIC_ADDRESS`,
port 7300 by default). If it cannot, the box log says so; see [Troubleshooting](Troubleshooting.md).

## The cell list

**Administration > Cells** shows, for every cell: its name, location, **state** (*pending*, *active*,
*disabled*, *revoked*), the **link** (up or down), the round-trip time (95th percentile), the software
version, the DECT **identity (RFPI)** and when it was last seen. Open a cell for its details: the chip build,
the calls, the handsets it holds, the audio capabilities.

On the cell's own page an administrator can **approve**, **disable** and **enable** it (a disabled cell is
kept but its radio is off). The owner's settings are on **My base stations**: the name, the location and
**Revoke**, which permanently removes the cell's right to connect (you must type its name to confirm; a
revoked box has to enrol again with a new token).

There is no call limit to set and no way to allow unencrypted audio. A cell takes as many calls as its voice
channels allow (the **Capacity announced** line shows them: records are handsets the chip can hold, the
narrowband and wideband numbers are the same voice channels, not two sets), and a call's audio is always
SRTP between the cell and the controller.

## Leaving cell mode on the box

On the box, **Network > DECT** shows *Leave cell mode…*. It erases the controller's key and certificate on
that box and runs it as its own DECT base again, with its own handsets and settings. To be a cell again it
needs a new enrollment string. Revoke the old cell on the controller as well.

## The DECT network identity

All cells of a controller share one **DECT system identity** so that a handset sees them as one network.
**Administration > Network identity** shows it. Each cell gets an identity of its own, derived from the system
identity (the same operator codes, a number of its own per cell), and every cell reloads its chip with it when
it next connects.

> **Handle with care.** Replacing the system identity makes **every handset registered so far lose its
> base** and register again. You do not need to touch this page for normal use. It exists for the day you
> want to give the system an identity of your own (for example your own EIC, the operator code from ETSI) or
> take one cell's identity as the system's. It asks for confirmation twice.

## Base stations for users, and the map

**Base stations > My base stations** shows the cells a user enrolled. An owner can choose what may be shown
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
