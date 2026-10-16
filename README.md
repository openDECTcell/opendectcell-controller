# opendectcell-controller

Central controller for [openDECTcell](https://github.com/openDECTcell/opendectcell): it connects DECT cells (FRITZ!Boxes running OpenWrt and openDECTcell) to VoIP.

> **The controller translates between DECT and SIP. It is not a PBX.** Dial plans, voicemail, call routing, conferences and the like stay in the PBX or with the provider. Every registered handset appears there as one or two ordinary SIP user agents.

Status: **design phase.** Implementation starts after openDECTcell's hardware gate G7 (a handset rings and audio passes via the on-box test tool).

## Scope

- **DECT side:** registration database, DLC/NWK sessions per handset (via the shared C protocol library `libopendectcell` from the openDECTcell repo), cell management, idle roaming across cells.
- **VoIP side:** per handset **up to two SIP accounts** (lines), with registration, calls, audio bridging (RTP/SRTP) and the usual interoperability features; see [docs/voip-requirements.md](docs/voip-requirements.md).
- **Web frontend:**
  - per-user authentication (local accounts and OIDC, e.g. Authentik)
  - users register their handsets and assign SIP accounts
  - an admin interface for statistics, logs and cell status
  - a policy for who may add base stations
- **Cell link:** TCP for control (optional TLS) and UDP for audio (optional SRTP/DTLS), with mutual authentication when cells are reached over the internet.
- **Feature framework:** DECT features beyond basic calls (date/time, missed calls, message waiting, …) can be added later **per handset model**; see [docs/dect-features.md](docs/dect-features.md).

## Documents

| Doc | Content |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Components, data model, cell protocol, security |
| [docs/voip-requirements.md](docs/voip-requirements.md) | SIP/RTP parameters to support, including encryption |
| [docs/dect-features.md](docs/dect-features.md) | DECT features beyond calls and the per-model feature framework |

## License

GPL-2.0 (see [LICENSE](LICENSE)).
