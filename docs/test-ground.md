# The test ground

A Raspberry Pi 4 (4 GB, Debian 13, arm64) on the maintainer's LAN, `pi@opendectcell-pi.fritz.box`. It is the permanent place where the
controller runs against the lab's boxes and handsets: the soak ([adr/0003-stack-isolation.md](adr/0003-stack-isolation.md)), release
checks, the demo for others, and **remote cells over the internet** (real latency). Its web interface is public, with a proper certificate, at
**https://opendectcell.example.org**; cells connect to the same name on port 7300.

## What runs where

| Part | How | Notes |
|---|---|---|
| Controller, database, `migrate` | Docker Compose, the repository's `docker-compose.yml` | as in [deployment.md](deployment.md); the controller runs as the limited database role; DECT stacks in **4 worker processes** (one per CPU, the soak's configuration). The controller is in the **host network**: it listens on the Pi's own IPv6 and IPv4 addresses, so remote cells are seen with their real addresses and each cell is sent its speech to the address it connected to (`OPENDECTCELL_CELLS_MEDIA_IP=auto`) |
| TLS proxy | Caddy 2 in the host network, `deploy/pi/Caddyfile` | gets and renews the Let's Encrypt certificate by itself; redirects http to https; passes to the controller on `127.0.0.1:8080` |
| Name | `opendectcell.example.org` at ipv64.net, **AAAA only** | `opendectcell-ddns.timer` (every 5 minutes) sets it to the Pi's stable global IPv6 address, only when that changed (the account allows 48 updates a day); the key is in `/etc/opendectcell/ddns.env` (root, 0600) and nowhere in the repository |
| Host firewall | nftables, `deploy/pi/nftables.conf` | inbound dropped except SSH and mDNS **from the LAN**, SIP and its audio **from the LAN**, ports 80, 443 and 7300 and UDP 41100 to 41199 from anywhere, ICMPv6. It is the one place that decides what is reachable, since the controller does not run behind Docker's port mapping |
| Updates | `unattended-upgrades` | security updates of the OS; the stack is updated by `deploy/pi/deploy.sh` |

## Which ports to open

The line has no public IPv4 address, so the only way in from the internet is **IPv6**, and the FRITZ!Box's firewall blocks it by default.
Open these for the Pi (IPv6 port sharing in the FRITZ!Box), and nothing else:

| Port | Protocol | Why | Needed |
|---|---|---|---|
| **443** | TCP | the web interface (https), and the certificate authority's validation as a fallback | **yes** |
| **80** | TCP | the certificate authority's validation (HTTP-01) and the redirect to https | recommended |
| **7300** | TCP | the cells' control link (TLS 1.3, mutual authentication; enrolment with a one-time token) | for remote cells |
| **41100 to 41199** | UDP | the cells' speech (SRTP), one port per call: 100 ports are far more than the Pi has audio capacity for; 41100 to 41139 is enough | for remote cells |

Do **not** open: 22 (SSH: from the LAN only), 5060 and 41000 to 41099 (SIP and the audio of SIP calls: the controller listens on the LAN address
only, and the firewall says so too), 8080 (the web port is on the loopback address, behind Caddy), 5432 (the database, loopback).

**FRITZ!Box** (menu names vary a little between FRITZ!OS versions): *Internet → Freigaben → Portfreigaben → Gerät für Freigaben hinzufügen*,
choose `opendectcell-pi`, then *Neue Freigabe* → *Portfreigabe* with the **IPv6** variant ("Freigabe auch für IPv6" / *Nur IPv6*). For IPv6 the box opens
its firewall for the device's **interface identifier**: it must match the Pi's address (`ip -6 addr show eth0`: the last four groups of
the `2003:...` address, now `bcf9:2bda:7dd1:b7df`). A wrong identifier looks exactly like a closed port. Check from outside the LAN (mobile data
with IPv6): `curl -6 -I https://opendectcell.example.org`; for the cell port, enrol a cell.

## Remote cells

A remote FRITZ!Box with openDECTcell becomes a cell of this controller across the internet. It needs **IPv6** (the controller has no public IPv4;
an IPv4-only site cannot reach it; DS-Lite lines have IPv6 and are fine) and nothing opened on its side (it only connects out; its speech
goes to the controller's UDP port and the replies come back through the same flow).

1. On https://opendectcell.example.org: *Enroll a base station*, make a token (it works once). The string names `opendectcell.example.org:7300` and pins the controller's CA.
2. On the box (LuCI *Network → DECT → Enrol*, or `/etc/init.d/opendectcell enroll '<string>'`): paste it. The cell appears on the *Cells* page; approve it.
3. The page shows the link's round-trip time (median and 95th percentile) and the last contact. Calls between a handset on the cell and a SIP peer on the
   LAN test the speech path over the real route; the speech goes to the address the cell connected to.

To see how the system copes with worse links than the real one, add delay and loss on the **box** with `tc qdisc add dev <wan> root netem delay 80ms 20ms loss 2%`
(remove it with `tc qdisc del dev <wan> root`) and watch the round-trip figures, the jitter in the call statistics and what the handset hears.

## Certificate

`ACME_CA` in `deploy/pi/.env` chooses the authority. **While ports are not open or something is being tried, keep the staging authority**
(`https://acme-staging-v02.api.letsencrypt.org/directory`): Let's Encrypt allows only five failed validations per hour for a name, and the
staging one has generous limits but issues certificates browsers do not trust. When `curl -6 https://opendectcell.example.org` gets a certificate
from staging, remove the line, run `deploy/pi/deploy.sh`, and Caddy fetches the real one. The account key and the certificates are in the
`caddy_data` volume: back it up with the master key, or a rebuild asks for new ones (the limit is five a week per name).

## Operating it

| Task | Command (on the Pi, in `~/opendectcell-controller`) |
|---|---|
| Update from a working tree | on the maintainer's machine: `deploy/pi/deploy.sh` (rsync, build on the Pi, `up -d`) |
| Status | `sudo docker compose --env-file deploy/pi/.env -f docker-compose.yml -f deploy/pi/docker-compose.pi.yml ps` |
| Logs | the same with `logs -f controller` or `logs caddy` |
| The DynDNS updater | `journalctl -t opendectcell-ddns`; `systemctl list-timers opendectcell-ddns.timer` |
| Firewall | `sudo nft list table inet opendectcell_filter` |
| First administrator | `sudo docker compose ... exec controller opendectcell-controller user create --username NAME --role admin` (the password is read from standard input) |

**Back up** the master key (`docker compose cp controller:/secrets/master_key .`, kept away from the database) and the `caddy_data` volume.
Whoever has root on the Pi has the master key, the database and every key in it: this is a test system, with test handsets.

## The lab's cells

Point the 7520 and the 7412 at this controller by enrolling them as above: on the LAN they resolve the same name and connect over IPv6 as well
(the controller listens on both families). The soak plan in [adr/0003-stack-isolation.md](adr/0003-stack-isolation.md) runs here.
