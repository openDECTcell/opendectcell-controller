# Code and security review, 2026-11-03

Three read-only reviews of the working tree after the day's changes (handset privacy, disclosure, enrolment without approval,
two-factor on known devices): web, authentication and API; cell link, SIP, bridge and DECT core; configuration, database,
deployment and CI. Every finding was confirmed by reading the code; nothing was run by the reviewers. Written down first, then
fixed; the **Status** column says what happened. IDs: **W** web/auth, **C** cell/SIP/DECT, **R** configuration/deployment.

Status words: **fixed** (with a test unless it is a document or a script), **documented** (the behaviour stays, and is written
down where users and operators read it), **open** (needs a change in another repository or a decision).

## Medium

| ID | Finding | Status |
|---|---|---|
| W1 | An enrolment token outlived its creator's right to enroll (a disabled or demoted user, the setting turned off) and a cell enrolled with it is active at once | **fixed**: `Enroll` checks the creator now; disabling a user deletes their open tokens; at most 50 open tokens per user; a creator can delete their own token after losing the right |
| W2 | An admin could read, change and delete every user's personal contacts (list, API, `owner=`), and write into their phonebook | **fixed**: a personal phonebook is the owner's alone; shared contacts stay admin-managed |
| W3 | An admin sets a user's password, or opens a registration window for another owner, and thereby acts as that user (audited) | **documented** (ADR 0008): an administrator's power to reset a password is the way back into an account; with two-factor on, a reset alone is not enough. Users who mind should turn two-factor on |
| W4 | "Admins need two-factor" covered only admin routes, not the admin powers on user routes (shared SIP accounts, stations, registration windows, organizations) | **fixed**: an admin without two-factor is a plain user for the whole request until they set it up |
| W5 | An admin saw the exact position and statistics of an undisclosed base station, and an admin's save overwrote the operator's sharing switches | **fixed**: a non-operator admin sees and edits name, place and model only; position and statistics need disclosure; the sharing switches are the operator's |
| C1 | An unauthenticated LOCATE-REQUEST from a handset's IPUI makes a cell the handset's "last heard" cell (the stack stamps it before authentication), so the next call is offered to that cell first | **fixed 2026-11-04** in libopendectcell (API 157, openDECTcell bbc8c5a): a handset counts as heard only once its connection has proved its key; tested in the library, **confirmed on the radio 2026-11-04**: an AS690H registered on the Pi's lab cell located every 247 s (17:28, 17:34, 17:38) and its last-seen time followed each time |
| C2 = R1 | `rotate-dek` while the controller runs destroys data: the live process keeps the old keyring and anything sealed after the commit uses a deleted key | **fixed**: a running controller holds a database lock and `rotate-dek` refuses while it is held; documented |
| C3 | Registration ignores the owner's trust scope: a handset registered through a foreign cell has its key written to that cell's chip although the owner trusts only their own | **fixed**: a cell is offered only the PINs of owners who trust it (`regwin.PINsFor`, tested; the call from the DECT core is tested with two owners, only one of whom trusts the cell) |
| C4 | No cap on cells per operator, and every enrolled cell is active at once | **fixed**: at most 20 cells per non-admin operator; the cell is told only that the token is no good, the audit log says why |
| R2 | The audit log is append-only in name only: any session can `SET opendectcell.audit_purge = 'on'` and delete | **fixed**: purge goes through a function owned by the migration role; the app role has no DELETE on `audit_log` |
| R3 | Duplicate e-mail addresses allowed; reset and resend pick a row without order | **fixed**: unique among verified addresses; resend refuses an ambiguous address; verification cannot take an address another account verified |

## Low and medium-low

| ID | Finding | Status |
|---|---|---|
| W6 | A TOTP code can be replayed through a race (two parallel requests) | **fixed** |
| W7 | E-mail verification re-enables an account an administrator disabled | **fixed** (`admin_disabled`) |
| W8 | Incoming call lists leak between users that share a SIP account | **fixed**: a handset sees calls of its own owner |
| W9 | `handset.List` loads every user's lines | **fixed** |
| W10 | A database error skips the system-identity confirmation | **fixed** |
| W11 | The `otpauth:` link on the setup page is dead (`html/template` rewrites it) | **fixed** |
| W12 | The stations feed is cacheable by shared caches although login is required | **fixed** |
| W13 | Control characters in cell names silence alert mails | **fixed** |
| W14 | A reset does not forget trusted devices; a partial `PATCH` can report 400 although part applied; the failed-login audit entry stores what was typed in the user name field (maybe a password); `RememberDevice` keeps a client-supplied cookie token of the right shape | **fixed** (reset, audit entry); partial `PATCH` **documented**; the cookie token **documented**: it can be planted only over plain http (the cookie is `__Host-` over https) |
| W15 | Anyone can lock a named account out of password login for 15 minutes; `oidc/start` writes a row per anonymous request | **documented** (known trade-off of per-account limits; rows expire) |
| C5 | Cells of a disabled user stay active | **fixed**: they are disabled with the user |
| C6 | DECT workers are shared across cells; the seccomp filter is a deny-list without `io_uring` and others | **documented**: needs a memory-safety bug first; Docker's default profile blocks `io_uring` |
| C7 | `Serve` stops the whole controller on a non-timeout `Accept` error (for example out of file descriptors) | **fixed**: retried with back-off (`TestServeSurvivesFailingAccepts`) |
| C8 | No per-session rate limit for frames from a cell (a database update per STATS frame, a log line per unknown frame) | **fixed** for STATS writes (at most one per 10 s per session; `TestStatsWritesAreThrottled`); the rest **documented** |
| C9 | `Renew` is unthrottled and works for cells that are not active | **fixed** |
| C10 | A media-open refusal can be matched to the wrong call (the correlation is 16 bits, the media id 32) | **fixed**: media ids wrap within 1 to 65535 and skip ids in use |
| C11 | The SDP media destination is unrestricted (loopback, multicast) | **documented**: a small 1:1 stream; tests and lab setups use loopback |
| C12 | The SIP contact token is the only authenticator for INVITE/NOTIFY from any source | **documented**: known design (docs/sip-ua.md), not a new finding |
| C13 | IPUIs appear in logs although the threat model says they do not | **fixed** for the controller's own lines (dialled digits reduced to a length, the IPUI out of a debug line); the library's trace lines carry IPUIs by design, and the threat model now says so |
| R4 | `Stats.Days` labels bars one day early with `TZ` set | **fixed** |
| R5 | The public Caddy has no timeouts and forwards `/metrics` | **fixed** |
| R6 | The deployment guide says the web port is fixed to 127.0.0.1; in host network mode it is not | **fixed** (document) |
| R7 | Deleting a cell that enrolled with a token fails on a CHECK | **fixed** (migration) |
| R8 | The DynDNS unit fails on a fresh install (no state directory) | **fixed** |
| R9 | The firewall's "LAN" set follows router advertisements, so a rogue RA on the LAN widens it | **documented**: needs a host on the LAN already |
| R10 | The `/metrics` token has no minimum length | **fixed** (at least 32 characters) |
| R11 | The CSP blocks an inline `onclick`, so click-to-select does nothing | **fixed** |
| R12 | `deploy.sh` ships the whole working tree including keys | **fixed** |
| R13 | `migrate down` for 0017 re-adds a constraint the migration was written to lift | **documented**: down migrations are not supported in production |

## Not fixed, and why

- **C1** was fixed on 2026-11-04 in libopendectcell (API 157); the radio check was made the same evening (an AS690H on the lab cell stayed online across three locations; one handset, one cell).
- **W3** is a property of what an administrator is: a password reset is the way back into an account. Users who want more
  turn two-factor on, which a reset does not bypass.
- Findings marked **documented** are written into [threat-model.md](threat-model.md), [deployment.md](deployment.md) or the ADRs.

## What the reviews did not cover

Nothing was run: no fuzzing, no scanner (`govulncheck` and `gosec` ran in the review of 2026-11-02), no penetration test of the deployed
Raspberry Pi. The third-party code (sipgo, pion, go-oidc) was read only where the controller calls into it. The libopendectcell C code was
read at the Go boundary only.
