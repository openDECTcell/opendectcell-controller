# Hardware and handsets

## Boxes

| Box | State | What to know |
|---|---|---|
| FRITZ!Box **7520** | ✅ works, the development box | Chip build 08.25 and 07.56 both seen. Up to **five** calls at once per cell. |
| FRITZ!Box **7530** | 🟡 same hardware and image as the 7520, not tested on one yet | |
| FRITZ!Box **7412** | 🟡 works as a cell (registration, calls in and out with audio, roaming with a 7520) | Chip build 06.88: **three** calls at once per cell. Needs `kmod-lantiq-ssc-pcm`, a second kernel module that is **not in the release yet** (see below). One open defect: in about 4 of 190 service stops the box's network went dead; no cause yet. |
| FRITZ!Box **7362 SL** | ❌ not usable yet | Radio and echo tests passed on a bench; the service and SIP calls are not done. |
| Other FRITZ!Boxes | ❌ | Many use the same DECT chip; [hardware-abstraction.md](https://github.com/openDECTcell/opendectcell/blob/main/docs/hardware-abstraction.md) says what a new model needs. |

**OpenWrt version.** Only **25.12.5** is supported: the kernel module has to be built for the exact kernel on
the box. The release lists the version it was built for.

**The DECT chip firmware** is AVM's and may not be shipped with openDECTcell. You take it once from the official
FRITZ!OS image of your own box with a tool in the repository: [Prepare a box](Box-install.md).

**The 7412 and its PCM module.** The 7412's voice bus needs `kmod-lantiq-ssc-pcm`, which comes from its own
repository and is not part of the release. Without it, handsets register but calls have no audio. Until the
release includes it you build it with the OpenWrt SDK (`lantiq/xrx200`); see
[bench-7412.md](https://github.com/openDECTcell/opendectcell/blob/main/docs/bench-7412.md) in the openDECTcell repository.

**Serial console.** On these boxes the DECT chip shares the serial port with the router's console. openDECTcell
switches the console off so the chip can use it. This matters only if you have soldered on a serial cable.

## Handsets

| Handset | Standard | Seen working with openDECTcell |
|---|---|---|
| Gigaset **A540** CAT | CAT-iq | Registration, calls in and out, HD voice, encryption, name and number on the display, roaming between a 7520 and a 7412 |
| Gigaset **AS690H** | GAP / NG-DECT Part 1 | Registration, calls, HD voice, encryption, number, roaming |
| Gigaset **C430A** | GAP / NG-DECT Part 1 | Registration, calls, HD voice, encryption, number; shows no date and time from the base |
| AVM **FRITZ!Fon C4** | CAT-iq | Registered and called during development; not tried with the current release |

openDECTcell follows the standards, so other GAP and CAT-iq handsets should work. A handset that is not in the
list of device profiles works as a generic one. Please [tell us](https://github.com/openDECTcell/opendectcell/blob/main/CONTRIBUTING.md)
how yours does.

**One base at a time.** Some handsets keep only one registration (the AS690H dropped the old one when it
registered elsewhere). Registering a handset with the controller means all its cells count as one base, so
this does not hurt roaming; it only means a handset registered here forgets another system.

## What a cell can carry

| | 7520 / 7530 | 7412 |
|---|---|---|
| Calls at the same time per cell | 5 | 3 |
| Handsets known to a cell | all of the controller's (the cell keeps the ones that are around in memory, up to 26 at a time) | the same |
| Handsets that can hold a link **at the same time**, in call or ringing | see below | see below |

On the 7412's chip, handsets are told apart by the last digit of their temporary number, and only one of
two handsets that share a digit can be reached at a time. With a few handsets you will never notice; with
a ring group of many handsets on one 7412 cell, not all ring at once (the others are tried again).
Details: [Roaming and many cells](Roaming-and-many-cells.md).
