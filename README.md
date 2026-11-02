# opendectcell-controller

Central controller for [openDECTcell](https://github.com/openDECTcell/opendectcell): it connects DECT cells (FRITZ!Boxes running
OpenWrt and openDECTcell) to VoIP, so that many boxes act as one DECT network and handsets roam between them.

> **The controller translates between DECT and SIP. It is not a PBX.** Dial plans, voicemail, call routing,
> conferences and the like stay in the PBX or with the provider. Every registered handset appears there as one or
> two ordinary SIP user agents.

**Status: it makes real calls.** A controller with a FRITZ!Box 7520 and a 7412 as cells registers Gigaset
handsets, carries calls in and out over SIP with HD voice and encryption, and moves handsets between the two
boxes. Many features are still missing (transfer, the handset phonebook, message waiting, internal calls) and
scale has been tested only in simulation: [what works and what does not](docs/wiki/Limits.md).

## Quick start

You need a Linux server with Docker, a FRITZ!Box with OpenWrt and the openDECTcell packages, a DECT handset and a SIP
account. The whole way, step by step, is in the wiki: **[docs/wiki/Quick-start.md](docs/wiki/Quick-start.md)**. The
short version, for the controller:

```sh
git clone --recurse-submodules https://github.com/openDECTcell/opendectcell-controller
cd opendectcell-controller
docker compose up -d --build
docker compose exec controller opendectcell-controller user create --username admin --role admin
# type the password and press Enter, then open http://localhost:8080
docker compose cp controller:/secrets/master_key ./master_key.backup   # keep it apart from the database backups
```

The controller needs the DECT library from the `opendectcell` submodule; the image is built with it by default. Behind
Docker's port mapping, tell the controller the addresses the cells and SIP peers reach it on, or calls have no
audio: [Install the controller](docs/wiki/Controller-install.md).

## Scope

- **DECT side:** registration database, DLC/NWK sessions per handset (via the shared C protocol library `libopendectcell`
  from the openDECTcell repo), cell management, roaming across cells.
- **VoIP side:** per handset **up to two SIP accounts** (lines), with registration, calls, audio bridging
  (RTP/SRTP) and the usual interoperability features; see [docs/voip-requirements.md](docs/voip-requirements.md).
- **Web frontend:** per-user authentication (local accounts with optional two-factor, and OIDC, e.g. Authentik);
  users register their handsets and assign SIP accounts; an administrator interface for cells, users, settings,
  statistics, logs and the audit log; a policy for who may add base stations; an optional public map.
- **Cell link:** TLS 1.3 with mutual authentication for control (TCP 7300) and SRTP for the audio.
- **Feature framework:** DECT features beyond basic calls can be added **per handset model**; see
  [docs/dect-features.md](docs/dect-features.md).

## Documents

**To use it:** the [wiki](docs/wiki/Home.md): [quick start](docs/wiki/Quick-start.md),
[how it fits together](docs/wiki/How-it-fits-together.md), [hardware and handsets](docs/wiki/Hardware-and-handsets.md),
[installing the controller](docs/wiki/Controller-install.md), [preparing a box](docs/wiki/Box-install.md),
[cells](docs/wiki/Cells.md), [handsets](docs/wiki/Handsets.md), [SIP lines](docs/wiki/SIP-lines.md),
[roaming](docs/wiki/Roaming-and-many-cells.md), [operating](docs/wiki/Operating.md),
[troubleshooting](docs/wiki/Troubleshooting.md), [limits](docs/wiki/Limits.md).

**To build on it:**

| Doc | Content |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Components, data model, cell protocol, security |
| [docs/configuration.md](docs/configuration.md), [docs/deployment.md](docs/deployment.md) | Every setting and command |
| [docs/roaming.md](docs/roaming.md) | How handsets move between cells and how calls find them |
| [docs/cell-protocol.md](docs/cell-protocol.md) | Cell↔controller wire protocol: framing, enrollment, mTLS, heartbeat, mail tunnel, subscription slots, media |
| [docs/libopendectcell-integration.md](docs/libopendectcell-integration.md) | How the controller embeds `libopendectcell` through cgo: sessions, memory, timers, build |
| [docs/voip-requirements.md](docs/voip-requirements.md), [docs/sip-ua.md](docs/sip-ua.md) | The SIP side |
| [docs/threat-model.md](docs/threat-model.md) | What is protected against what |
| [docs/next-steps-2026-11-02.md](docs/next-steps-2026-11-02.md) | Where the project stands, what is missing, what comes next |
| the maintainers' notes | What was done each session, newest first |
| [docs/first-tasks.md](docs/first-tasks.md) | Start here if you are joining |
| [AGENTS.md](AGENTS.md) | Rules for AI agents working in this repository |

## License

AGPL-3.0-or-later (see [LICENSE](LICENSE)). Anyone running a modified controller as a network service must offer
its source to the users of that service.
