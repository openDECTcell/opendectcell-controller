# Quick start

From nothing to a handset that makes and takes a call, with a controller and one box as a cell. It takes
about an hour, most of it waiting for downloads. Later boxes need only steps 3 to 5.

**What you need**

- A FRITZ!Box **7520** with OpenWrt 25.12.5 (the 7530 should be the same; the 7412 works but needs extra
  steps, see [Hardware and handsets](Hardware-and-handsets.md)). Installing OpenWrt is described on the
  [OpenWrt wiki](https://openwrt.org/toh/avm/avm_fritz_box_7530).
- A computer or server for the controller that the box can reach over the network: Linux with Docker and
  Docker Compose is the tested setup. It must keep running, since all calls go through it.
- A DECT handset. Tested: Gigaset A540, C430A, AS690H.
- A SIP account (a provider, or an "IP phone" on a FRITZ!Box or PBX), if you want to call outside.
- The official FRITZ!OS image of your box from AVM, **on your own PC**. The DECT chip needs AVM's firmware,
  which openDECTcell may not ship; a tool takes it out of the image for you (step 3).

## 1. Start the controller

On the server:

```sh
git clone --recurse-submodules https://github.com/openDECTcell/opendectcell-controller
cd opendectcell-controller
docker compose up -d --build
docker compose exec controller opendectcell-controller user create --username admin --role admin
```

The last command asks for a password on its standard input: type it and press Enter. The image is built
**with the DECT core** (the build needs the `opendectcell` submodule; without it the build stops with a message).
Open <http://localhost:8080> and sign in as `admin`. The first thing you see is **the DECT network identity**: the
controller proposes one and says what it means (below, and [Base stations](Cells.md#the-dect-network-identity)).
Accept it unless you have a reason not to, **before you register the first handset**.

Back up the master key now, separately from the database. Without it, every stored key and SIP password
in a database backup is lost:

```sh
docker compose cp controller:/secrets/master_key ./master_key.backup
```

**If the box is not on the same machine** (it never is): tell the controller its own address.
Create a file `.env` next to `docker-compose.yml`:

```sh
OPENDECTCELL_HTTP_BASE_URL=http://192.168.1.50:8080
```

and add to the `controller` service's `environment` in a `docker-compose.override.yml`:

```yaml
services:
  controller:
    ports: ["8080:8080"]          # the web port is on 127.0.0.1 only by default
    environment:
      OPENDECTCELL_CELLS_PUBLIC_ADDRESS: 192.168.1.50:7300   # how the cells reach the controller
      OPENDECTCELL_CELLS_MEDIA_IP: 192.168.1.50              # where the cells send a call's audio
      OPENDECTCELL_SIP_MEDIA_IP: 192.168.1.50                # the address your SIP peer sends audio to
```

Use the server's real address. Without a valid media address **calls connect but have no audio**; the log
says so at start. Then `docker compose up -d`. More in [Install the controller](Controller-install.md).

## 2. Get an enrollment string

In the controller: **Base stations > Enroll a base station**, choose how long the token is valid and
press **Create token**. The page shows an **enrollment string**
(it starts with `opendectcell-enroll:v1?`) **once**. Copy it.

## 3. Prepare the box

On the box (OpenWrt, with internet access), install the packages: the kernel module for your box, `opendectcell`,
`opendectcell-cell` and `luci-app-opendectcell`. The packages are signed with the project's key, which you add once.
The steps, with every command, are in [Prepare a box](Box-install.md). In short:

```sh
cp opendectcell-signing-key.pem /etc/apk/keys/
apk update
apk add ./kmod-opendectcell-*.apk ./opendectcell-[0-9]*.apk ./opendectcell-cell-*.apk ./luci-app-opendectcell-*.apk
```

Then the chip firmware. On your PC, with the FRITZ!OS image:

```sh
python3 tools/dect-fw-extract.py FRITZ.Box_7520-07.56.image
```

It saves the two firmware files, checks them, and prints the commands (`scp` to the box and one `uci`
line). Run them.

## 4. Make the box a cell

In the box's LuCI: **Network > DECT**, tab *General*, **Make this box a cell…**. Paste the enrollment string
and press *Enrol*. It takes up to a minute; the page reloads in cell mode. Tick **Enable the cell** and
*Save & Apply*.

Or on the command line: `/etc/init.d/opendectcell enroll '<enrollment string>'`.

## 5. Check the cell

In the controller: **Administration > All base stations**. The new cell is *active* at once (nobody has to approve it) and
its link *up* a moment after the box has started its cell service; the DECT radio is switched on by the controller.
Details: [Cells](Cells.md).

## 6. Add a SIP line

**Phones > VoIP lines > New**: a name, the user name and password and the registrar (the SIP server)
your provider or PBX gives you. Save. The profile shows whether it is registered. See [SIP lines](SIP-lines.md).

## 7. Register a handset

**Phones > Handsets > Register**: give the handset a name (at most 15 characters), choose its manufacturer
and model if listed, press **Start registration**. The page shows a **PIN** and a window of one minute opens
on every cell that is online. On the handset: *Menu, Settings, Registration, Register handset*, choose the
base station, enter the PIN. (Some handsets try 0000 first and fail once; they then ask again.)

Then open the handset's page and choose the profile of step 6 as **Line 1**. See [Handsets](Handsets.md).

## 8. Call

Dial a number on the handset. Call the SIP account from another phone: the handset rings. The handset page
and *Statistics* show the calls.

**Something does not work?** [Troubleshooting](Troubleshooting.md) lists what you will see and what to do.
The most common cause is the network: the cell must reach the controller on port 7300, and the audio needs
the two media addresses of step 1.

**More cells.** Repeat steps 2 to 5 for every box. A handset registered once works on all of them and moves
between them: [Roaming and many cells](Roaming-and-many-cells.md).
