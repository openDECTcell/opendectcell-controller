# openDECTcell wiki

openDECTcell turns the DECT radio of a FRITZ!Box running OpenWrt into an open base station for cordless
phones (DECT handsets), and connects those handsets to SIP: a provider, or your own telephone system.
This wiki explains how to use what the project builds, from nothing to a handset that rings.

**Two ways to use it.** Pick one before you start:

| | **One box on its own** | **Cells and a controller** |
|---|---|---|
| What runs where | The box runs the DECT base and the SIP client (baresip). You configure it in OpenWrt's web interface (LuCI). | The box is only a **cell** (the radio). A **controller** (a server, in Docker) runs the DECT stack, the SIP accounts, the web interface and the database. |
| Good for | A home with one box and up to six handsets. | Several boxes: handsets **roam** between them; many handsets; a makerspace, a house plus a workshop, a site with several buildings. |
| Start here | [Standalone: one box on its own](Standalone.md) | [Quick start](Quick-start.md) |

## Quick start and guides

1. [**Quick start**](Quick-start.md): a controller, one box as a cell, one handset, one call. About an hour.
2. [How it fits together](How-it-fits-together.md): the pieces and the words (cell, controller, line, TPUI) in plain language.
3. [Hardware and handsets](Hardware-and-handsets.md): which boxes and phones work, and what to expect from each.
4. [Install the controller](Controller-install.md): Docker, the first administrator, ports, a reverse proxy, single sign-on.
5. [Prepare a box: OpenWrt, packages and the chip firmware](Box-install.md)
6. [Cells](Cells.md): enrolling, approving, settings, the DECT network identity, the map.
7. [Register handsets](Handsets.md): the registration window, device profiles, names, lines, removing a handset.
8. [SIP lines](SIP-lines.md): connecting handsets to a provider or a telephone system; codecs, encryption, NAT.
9. [Roaming and many cells](Roaming-and-many-cells.md): how handsets move, what the settings do, the limits.
10. [Organizations and trust](Organizations-and-trust.md): what a base station's operator can read, and whose base stations your handsets may use.
11. [Move or back up your setup](Move-or-back-up.md): export and import of handsets (with keys), VoIP lines and contacts; many lines from a CSV.
12. [Notifications](Notifications.md): what the controller tells you, in the interface and by mail.
13. [Statistics](Statistics.md): calls, peaks, quality, line availability, comparison, CSV, the monthly usage report.
14. [API tokens](API-tokens.md): scripts and widgets using your account, with only what you allowed.
15. [Webhooks](Webhooks.md): the controller posts calls, handsets and notifications to Home Assistant, ntfy or your own script, signed.
16. [Operating a controller](Operating.md): backups, upgrades, monitoring, logs, secrets, data retention.
17. [Legal texts](Legal-texts.md): a legal notice, privacy notice, emergency-call warning and an experimental notice that every user signs off.
18. [Load testing](Load-testing.md): how many calls, cells and handsets your machine carries, and how to measure it.
19. [Troubleshooting](Troubleshooting.md)
20. [Limits and what is not there yet](Limits.md)
21. [Glossary](Glossary.md)

## Where the technical documents are

This wiki is for people who **use** the system. For people who build on it:

- The controller: [docs/architecture.md](../architecture.md), [cell-protocol.md](../cell-protocol.md),
  [roaming.md](../roaming.md), [configuration.md](../configuration.md), the maintainers' notes.
- The box side and the DECT library: the [opendectcell repository](https://github.com/openDECTcell/opendectcell) (`docs/status.md`
  says what works, `docs/luci-manual.md` describes every LuCI field, `docs/notes-for-the-controller.md` is
  the library's integration guide).
- What to do next on the project: [next-steps-2026-11-02.md](../next-steps-2026-11-02.md).

## About this wiki

It is written against the controller and packages as of 2026-11-02. Things that have been seen working on
real hardware are written as facts. Things that are built but not yet tried on air are marked
**(not yet tried on hardware)**, so you know where you are the first to look. If a page is wrong, please
open an issue.
