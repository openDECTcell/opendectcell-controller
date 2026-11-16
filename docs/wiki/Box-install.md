# Prepare a box: OpenWrt, packages and the chip firmware

This page takes a FRITZ!Box from "OpenWrt installed" to "ready to become a cell". The details of every LuCI
field are in the [LuCI manual](https://github.com/openDECTcell/opendectcell/blob/main/docs/luci-manual.md).

## 1. What you need

- A FRITZ!Box with **OpenWrt 25.12.6** and internet access during the installation (the package manager
  fetches what openDECTcell builds on).
- The release files from [GitHub releases](https://github.com/openDECTcell/opendectcell/releases), signed packages for
  your box:

| File | For |
|---|---|
| `kmod-opendectcell-ipq40xx-…apk` | the kernel module of a 7520 or 7530 |
| `kmod-opendectcell-lantiq-…apk` | the kernel module of a 7412 (plus `kmod-lantiq-ssc-pcm`, see [Hardware](Hardware-and-handsets.md)) |
| `opendectcell-…apk` | the DECT base: firmware loader, daemon, service |
| `opendectcell-cell-…apk` | **cell mode**: the link to a controller (needed for this wiki) |
| `luci-app-opendectcell-…apk` | the web pages Network > DECT and Status > DECT |
| `opendectcell-baresip-…apk` | the SIP client, only for a box that runs on its own ([Standalone](Standalone.md)) |
| `opendectcell-signing-key.pem`, `SHA256SUMS` | the key the packages are signed with, and the checksums |

- A PC with Python 3 and the repository (or at least `tools/dect-fw-extract.py`, `tools/sqfs_be.py`,
  `tools/avm_nand_sqfs.py`).
- The official FRITZ!OS image for your box from AVM (the `fritz.os` folder of your box on
  download.avm.de). openDECTcell is tested with FRITZ!OS 07.56 of the 7520; 08.25 is known as well.

> **Status of releases.** Releases and a package repository are being prepared; until a public release
> exists you build the packages from the `opendectcell` repository (`tools/release/build.sh`) or take them from
> the maintainers. The kernel module is built for one exact kernel; if the version differs it will not load.

## 2. Install the packages

On the box (`ssh root@192.168.1.1`), with the files copied to `/tmp`:

```sh
cd /tmp
sha256sum -c SHA256SUMS --ignore-missing
cp opendectcell-signing-key.pem /etc/apk/keys/
apk update
apk add ./kmod-opendectcell-*.apk ./opendectcell-[0-9]*.apk ./opendectcell-cell-*.apk ./luci-app-opendectcell-*.apk
```

Or in LuCI: *System > Administration > Repo Public Keys* (paste the key), *System > Software > Update lists…*,
then *Upload Package…* in this order: kernel module, `opendectcell`, `opendectcell-cell`, `luci-app-opendectcell`. Without
the key, the upload ends in "UNTRUSTED signature".

## 3. The chip firmware

The DECT chip needs AVM's firmware, which the router uploads at every start. openDECTcell may not ship it, so you
take it from the official image, once, on your own PC. Why and which builds are known:
[firmware-sourcing.md](https://github.com/openDECTcell/opendectcell/blob/main/docs/firmware-sourcing.md).

```sh
python3 tools/dect-fw-extract.py FRITZ.Box_7520-07.56.image
```

The tool finds the two firmware files, checks them, saves them to `./opendectcell-firmware/` and prints the next
commands: copy them to the box, for example

```sh
scp -O opendectcell-firmware/*.hex root@192.168.1.1:/etc/opendectcell/firmware/
```

and a `uci set` line that tells the box which build it got. Run them. Instead of `scp` you can upload the two
`.hex` files in LuCI: **Network > DECT**, tab *Firmware*. The box checks them against the build it needs.

The firmware, the handset stores and (in cell mode) the cell's credentials **stay across OpenWrt upgrades**.

The **7412** uses its own firmware, from the 7412's FRITZ!OS image, under the names its board profile expects;
the tool knows both stages by their hash. Running it on a 7412 image has not been tested yet.

## 4. Become a cell

Continue with [Cells](Cells.md): create an enrollment string on the controller and paste it into
**Network > DECT**, tab *General*, **Make this box a cell…** (or `/etc/init.d/opendectcell enroll '<string>'`).

The box needs the right date once it is a cell (NTP): its certificate is renewed over the link, and a
certificate that has run out cannot be renewed.

## 5. Upgrading and removing

A newer release installs the same way, over the old one. Your settings in `/etc/config/opendectcell` stay. A
sysupgrade (a new OpenWrt image) keeps the firmware, the handset stores and a cell's enrolment, because the
packages list them for it. `apk del opendectcell-cell opendectcell luci-app-opendectcell kmod-opendectcell-ipq40xx` removes
everything again.

If you edited a file under `/etc` that a package also installs, `apk` leaves your file and writes the new
one next to it as `*.apk-new`; look at the difference and merge it.
