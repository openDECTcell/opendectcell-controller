# SIP lines

The controller connects handsets to **SIP**: a provider, a telephone system (Asterisk, FreeSWITCH, a FRITZ!Box
with an "IP phone" set up for it) or any registrar. It acts as a SIP **user agent** for every account; it is
not a registrar or a PBX. Voicemail, dial plans and call routing are done by the system you connect to.

## Adding an account (a VoIP profile)

**Phones > VoIP profiles > New**. The page has these groups:

| Group | Fields |
|---|---|
| **Account** | *Profile name* (how the handsets' Line menus name it), *Name on the handset* (at most 15 characters), *Display name* (what the called party sees), *User name* (the SIP user, often the number), *Login name* (when the provider wants a login other than the user name), *Password*, *Registrar* (the server, `host` or `host:port`), *SIP domain*, *Register* and the *registration interval*. |
| **Server and NAT** | *Transport* (UDP, TCP, TLS), *Outbound proxy* and a second one, *SIP outbound (RFC 5626)*, *NAT traversal for media* with a *STUN/TURN server*, *Keep media open*. |
| **Media** | *Try to autodetect*, or choose the codecs: **G.722 (HD)**, G.711 A-law, G.711 µ-law; *Packet time*; *Key tones (DTMF)* mode; *Reliable provisional responses (100rel)*; *Voicemail notification (MWI)*; *Allow call transfer*. |
| **Encryption** | *Try to autodetect*, or choose *Media encryption* (SRTP). |
| **Advanced** | *Further account parameters*, *Caller ID from*, *Keep-alive*. |

The defaults ("Try to autodetect" for media and encryption) suit most providers. If your provider supports TLS,
choose it under *Server and NAT*. Passwords are stored encrypted with the master key. When you leave the
password field empty on an existing profile it keeps the stored one; *Remove the stored password* deletes it.

After saving, the profile shows whether it is **registered** and why not if it is not. The profile is then
available as a line for handsets.

**Giving a handset the line.** Open the handset ([Handsets](Handsets.md)) and choose the profile as **Line 1**
(and optionally another as Line 2). Calls to the account ring every handset that has it as a line; the first to
answer takes the call and the others stop ringing. A handset's own calls go out on Line 1, or on Line 2 if the
handset picks it (CAT-iq handsets can).

## What works

| | |
|---|---|
| Registration, SRV and NAPTR lookups, keep-alive, TLS | built; interoperability tested against Asterisk and FreeSWITCH in the project's automated tests |
| Calls in and out, caller number and name, HD voice | seen on hardware with Gigaset handsets |
| Codecs | The phones do the coding. The controller negotiates so that both sides use a codec they share and passes **G.722** through untouched; it converts only when no common codec exists. |
| Encryption | The audio between a cell and the controller is SRTP. On the SIP side SRTP with SDES keys is used when the account is set to encrypt or when autodetect finds the peer offering it. |
| DTMF | RFC 4733 events, SIP INFO and in-band audio are supported; the handset's keys during a call go to the far end. |
| NAT | The controller learns its public address from the responses (`received`, `rport`); a STUN server can be set. |

## What does not work yet

- **Call transfer, conference and early media**: not built. The *Allow call transfer* box is stored but does
  nothing yet.
- **Voicemail indicator (MWI)**: the SIP notification is accepted but not shown on the handset yet; the box is
  stored.
- **Trunk-style registration** (for example a provider that wants several numbers on one registration) has not
  been tried against a real provider; only the test servers above.
- Calls over TLS have not been interoperability-tested.

More: [Limits](Limits.md) and [docs/sip-ua.md](../sip-ua.md).

## Network and ports

SIP uses UDP and TCP port 5060 on the controller (`OPENDECTCELL_SIP_UDP_LISTEN`, `OPENDECTCELL_SIP_TCP_LISTEN`), and the audio
uses the UDP range `OPENDECTCELL_SIP_RTP_PORTS` (two ports per call). If your SIP peer is behind NAT or on the
internet, the address in `OPENDECTCELL_SIP_MEDIA_IP` must be the one the peer can send audio to. A FRITZ!Box used as a
SIP registrar must have an "IP phone" set up for the controller to register as.
