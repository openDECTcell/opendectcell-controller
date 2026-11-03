# The test ground

A Raspberry Pi 4 (4 GB, Debian 13, arm64) on the maintainer's LAN, `pi@opendectcell-pi.fritz.box`. It is the permanent place where the
controller runs against the lab's boxes and handsets: the soak ([adr/0003-stack-isolation.md](adr/0003-stack-isolation.md)), release
checks, the demo for others. Its web interface is public, with a proper certificate, at **https://opendectcell.example.org**.

## What runs where

| Part | How | Notes |
|---|---|---|
| Controller, database, `migrate` | Docker Compose, the repository's `docker-compose.yml` | as in [deployment.md](deployment.md); the controller runs as the limited database role; DECT stacks in **4 worker processes** (one per CPU, the soak's configuration) |
| TLS proxy | Caddy 2 in the host network, `deploy/pi/Caddyfile` | gets and renews the Let's Encrypt certificate by itself; redirects http to https; passes to the controller on `127.0.0.1:8080` |
| Name | `opendectcell.example.org` at ipv64.net, **AAAA only** | `opendectcell-ddns.timer` (every 5 minutes) sets it to the Pi's stable global IPv6 address, only when that changed (the account allows 48 updates a day); the key is in `/etc/opendectcell/ddns.env` (root, 0600) and nowhere in the repository |
| Host firewall | nftables, `deploy/pi/nftables.conf` | inbound dropped except SSH and mDNS **from the LAN**, ports 80 and 443 from anywhere, ICMPv6; Docker's tables are left alone |
| Updates | `unattended-upgrades` | security updates of the OS; the stack is updated by `deploy/pi/deploy.sh` |

## Which ports to open

The line has no public IPv4 address, so the only way in from the internet is **IPv6**, and the FRITZ!Box's firewall blocks it by default.
Open these for the Pi (IPv6 port sharing in the FRITZ!Box), and nothing else:

| Port | Protocol | Why | Required |
|---|---|---|---|
| **443** | TCP | the web interface (https), and the certificate authority's validation as a fallback | **yes** |
| **80** | TCP | the certificate authority's validation (HTTP-01) and the redirect to https | recommended: without it Caddy validates on 443 only, and `http://` does not redirect |

Do **not** open: 22 (SSH: from the LAN only), 7300 (the cells: the lab's boxes are on the LAN and reach it at `192.168.1.10:7300`),
5060 and 41000 to 41199 (SIP and audio: LAN only). They are published on the LAN address only (`OPENDECTCELL_*_BIND` in `deploy/pi/.env`),
Docker publishes on IPv4 only, and the Pi's firewall would not let IPv6 traffic to them in anyway. Open them (and change the binds) only
for a cell or a SIP provider that is not on the LAN, and then with the source addresses limited, as in [deployment.md](deployment.md).

**FRITZ!Box** (menu names vary a little between FRITZ!OS versions): *Internet → Freigaben → Portfreigaben → Gerät für Freigaben hinzufügen*,
choose `opendectcell-pi`, then *Neue Freigabe* → *Portfreigabe*, protocol TCP, port 443 to 443, and tick the **IPv6** variant ("Freigabe auch für
IPv6" / *Nur IPv6*; for IPv6 the box opens the firewall for the device's interface identifier, so it survives prefix changes). Repeat for 80.
Check from outside (mobile data with IPv6, not the home Wi-Fi): `curl -6 -I https://opendectcell.example.org`.

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

Point the 7520 and the 7412 at this controller by enrolling them with a token made on its *Enroll a base station* page (the string names
`192.168.1.10:7300` and pins the controller's CA). The soak plan in [adr/0003-stack-isolation.md](adr/0003-stack-isolation.md) runs here.
