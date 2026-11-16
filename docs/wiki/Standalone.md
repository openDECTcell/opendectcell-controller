# Standalone: one box on its own

Without a controller, one FRITZ!Box runs everything itself: the DECT base, the SIP client (baresip) and a
web interface in LuCI. It suits a home with one box and up to **six** handsets. If you want more handsets or more
boxes, use [cells and a controller](Quick-start.md) instead.

The complete guides live in the openDECTcell repository; this page is the short path.

1. **Prepare the box**: OpenWrt 25.12.6 and the signed packages `kmod-opendectcell-…`, `opendectcell`, `luci-app-opendectcell`
   and, for SIP, `opendectcell-baresip`. The commands are on [Prepare a box](Box-install.md) (leave out `opendectcell-cell`).
2. **The chip firmware**: extract it from the FRITZ!OS image on your PC with `tools/dect-fw-extract.py` and copy it
   to the box (same page, step 3).
3. **Switch on**: **Network > DECT**, tick *Enable the DECT base*, *Save & Apply*. **Status > DECT** shows the base
   on the air.
4. **Register a handset**: **Network > DECT**, tab *Handsets*, choose the model if listed, press *Register a
   handset*, and enter the **8-digit PIN** shown on the handset. The window closes after one handset registers or
   after two minutes. *Edit* renames it.
5. **SIP**: tab *VoIP Profiles*, *Add a profile* (user name, password, registrar). Then *Edit* the handset and pick the
   profile as **Line 1**.
6. *Save & Apply*. **Status > DECT**, tab *VoIP* shows whether the line is registered.

**What you get:** up to three calls at once, over SIP or between the box's own handsets; HD voice and encryption on
the air; caller number and line name on the display; call waiting and switching between two calls on CAT-iq handsets;
a phonebook from a CardDAV server (tested with Nextcloud).

**What you do not get:** roaming (there is one box), more than six handsets, hold on every handset model.

**Everything about it:**

- [Installing openDECTcell](https://github.com/openDECTcell/opendectcell/blob/main/docs/install.md): the full steps and what to do when
  something fails.
- [The web pages: a manual](https://github.com/openDECTcell/opendectcell/blob/main/docs/luci-manual.md): every field.
- [What works and what does not](https://github.com/openDECTcell/opendectcell/blob/main/docs/status.md).

A box can be moved to a controller later: **Network > DECT**, *Make this box a cell…*. Its own handsets and settings
stay saved but unused, and come back if it leaves cell mode.
