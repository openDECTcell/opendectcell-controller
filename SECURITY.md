# Security policy

The controller terminates the TLS links of the DECT cells, bridges calls between DECT handsets and SIP, and stores
accounts, SIP credentials and handset keys, so security reports are welcome and taken seriously.

## Reporting a vulnerability

**Please do not open a public issue for a security problem.** Report it privately through GitHub: on this repository's
*Security* tab choose *Report a vulnerability* (private vulnerability reporting). Say:

- what you found and where (file, function, endpoint, setting);
- which version (the footer of every page, or `opendectcell-controller version`) and how it is deployed;
- how to reproduce it, or why you believe it is exploitable;
- what an attacker needs: a web account (which role), a cell certificate, a SIP peer, network access.

Keep real phone numbers, SIP credentials, handset identities, keys and tokens out of the report; mask them.

You will get an answer, and a fix or a decision, as soon as the maintainers can; this is a small project, so please
allow some days. A fix is released with a note in the release text, and the reporter is named if they wish.

## What is in scope

The web interface and API (authentication, authorisation between users and organisations, API tokens, webhooks), the cell
enrolment and the mutual-TLS cell link, the SIP and RTP/SRTP handling, secrets at rest, the backup service, the Docker image
and the compose files in `deploy/`. Weaknesses in the radio standard itself (DECT's cipher and authentication) are known and
described in the [opendectcell repository's policy](https://github.com/openDECTcell/opendectcell/blob/main/SECURITY.md); problems in the
box software, the kernel modules or the packages belong there.

## What we already know

Our own reviews, what was fixed and what is still open: [docs/security-review-2026-11-02.md](docs/security-review-2026-11-02.md)
and the [threat model](docs/threat-model.md). What the controller stores about its users, and how it is erased:
[docs/wiki/Operating.md](docs/wiki/Operating.md).
