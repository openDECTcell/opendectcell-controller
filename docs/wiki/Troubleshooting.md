# Troubleshooting

Look first at the controller's log (`docker compose logs controller`) and, on the box, at **Status > DECT**,
tab *Log* in LuCI. Most problems are the network between the two.

## The cell does not connect

| What you see | What it means, what to do |
|---|---|
| Enrolling fails: "cannot reach the controller" | The box must reach the address in the enrollment string (`OPENDECTCELL_CELLS_PUBLIC_ADDRESS`, port 7300). Check from the box that the port answers (for example with `curl -v telnet://<address>:7300`, or from another computer in the box's network), the firewall and, behind Docker, that port 7300 is published. |
| Enrolling fails: token invalid | The token is single use and expires (1 hour to 7 days). Create a new one. |
| The cell stays *pending* | It enrolled with a token made before tokens approved by themselves. An administrator has to **Approve** it (**Administration > All base stations**). |
| The cell is *active* but the link is down | Look at the box log: the date (the certificate needs a right clock, check NTP), the address, a firewall. A certificate that ran out cannot renew: enrol again. |
| "built without libopendectcell" in the controller log | The image was built without the DECT core: cells connect but their messages are dropped. Rebuild with `docker compose up -d --build` with the `opendectcell` submodule checked out. |

## A handset does not register

| What you see | What it means, what to do |
|---|---|
| The handset says the PIN is wrong | Enter the PIN shown **now**; a new *Start registration* makes a new one. Some handsets try 0000 first and fail once. |
| The handset cannot find the base | The registration window is **one minute** and only on cells that are **online**. Press *Start registration* again and start the handset's search right away. |
| It found the base once and now searches | Handsets wait longer and longer between searches after the base was off. Give it half a minute, or press a key on it. |
| The registration page says registration is not available | The controller runs without the DECT core (see above). |

## Calls

| What you see | What it means, what to do |
|---|---|
| **A call connects but there is no audio** | The two media addresses are wrong: `OPENDECTCELL_CELLS_MEDIA_IP` (cells to controller) and `OPENDECTCELL_SIP_MEDIA_IP` (controller to the SIP peer), and the UDP ranges must be open (41100–41199 and 41000–41099 by default). The controller log says so at start when the cells address is invalid. |
| Audio in one direction only | The same, for the other address. SIP behind NAT: check the SIP peer can reach `OPENDECTCELL_SIP_MEDIA_IP`. |
| The handset shows `---` or "not possible" when you dial | It has no line, or the cell it is on does not hear it; see "A handset is deaf" below. |
| An incoming call does not ring on one handset, which is shown as offline | Some old GAP handsets (Siemens C45, C450) never answer the base's page; see [Hardware and handsets](Hardware-and-handsets.md). Check that the handset is switched on and registered; its outgoing calls usually still work. |
| A call is "busy" right away | The handset is in a call or ringing, or the cell has used all its voice channels (three on every cell for now; the 7520's chip could carry five, which is planned). |
| A call dropped when a box was switched off | A call does not move to another cell; there is no handover. |
| The line shows "not registered" | The reason is shown next to the profile: user name, password, registrar, transport, a firewall. |
| An incoming call is refused with 403 or does not ring | The account has to be registered, and the call must come from its server (or the one it is routed through). |
| Only one of two handsets rings | See [Roaming](Roaming-and-many-cells.md): on a 7412 two handsets that share the last digit of their temporary number cannot ring together. |

## A handset is deaf (it locates but is never reached, or cannot dial)

This was the main problem with the **7412** and is fixed: a 7412 chip only hears temporary numbers whose last digit
is 1 to 6, and the controller now hands out only those. If a handset that was registered **before** that fix is
deaf on a 7412, restart the cell's service once (`/etc/init.d/opendectcell restart` on the box) so the handset re-registers
and takes a new number; if it still is, switch it off and on, and tell us.

## A handset shows no base station, or cannot dial, after its base went away

Seen on the lab (2026-11-05): the 7520 and the 7412 shared a network identity, all handsets had been registered under
it, and the 7520 was unplugged. The A420 and the A540 were on the 7412 within seconds. A **Gigaset AS690H** showed
`---` and **could not dial until it was restarted** (battery out, or its own off/on); the controller saw nothing from
it on the 7412 until then, and it dialled at the first try afterwards. If a handset does not find the remaining
base station, restart the handset once. Before that, check that the base stations share an identity (Administration >
Network identity): without one, a handset registered on one box cannot use another at all. We have seen the AS690H do
this once; we do not know whether other models do, or whether it recovers by itself if you wait.

## Other

| What you see | What it means, what to do |
|---|---|
| The map is empty | The map is off by default; an administrator turns it on, and owners choose what to share. |
| Sign-in locked out of two-factor | An administrator with server access runs `docker compose exec controller opendectcell-controller user reset-2fa --username N`. |
| `docker compose up` stops at the build | The `opendectcell` submodule is missing: `git submodule update --init`. |
| A cell and a handset were fine, then a box lost power | They recover on their own. A handset needs up to a few minutes to find its base again; a key press helps. |

**Reporting a problem.** Open an issue and include: the controller version (`opendectcell-controller version`), the box
model and OpenWrt version, the chip build (shown on the cell's page), the handset model, and the log lines around
the problem. Do **not** include the master key, passwords or enrollment strings.
