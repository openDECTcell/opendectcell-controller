# Security review before publication, 2026-11-02

An adversarial review of this repository and of openDECTcell (`docs/security-review-2026-11-02.md` there) before going
public: code reading of every handler, the cell link, the SIP side and the DECT core, `go vet`, `govulncheck`
(no reachable vulnerability) and `gosec`. The earlier audit of 2026-10-29 is not described in the worklog, so this one
could not be diffed against it. Severity is the reviewer's; "fixed" says what was changed and where it is tested.

## Fixed

| Finding | What was wrong | What changed |
|---|---|---|
| **F-01 (high)** SIP INVITE/NOTIFY from any source | A request addressed by extension and domain (`To:`) was accepted from any address: ringing handsets, forged caller IDs and missed calls, RTP port exhaustion | A request addressed by the account's secret Contact token is taken from anywhere (that is the registrar's side); one addressed by extension and domain only from the addresses the account's servers resolve to (`sip.Manager.accept`, `line.fromPeer`). At most 8 incoming calls per account and 4 per source at once (`takeIncoming`). Tests: `sip/guard_test.go`. **Still open:** an INVITE with the token from a stranger who learned it (it is a 64-bit random value in the account's id); compose still publishes 5060 on all interfaces: firewall it |
| **F-02 (medium)** SIP account fields | A user chose the registrar, proxy and domain: the controller connected to loopback, link-local, Docker-internal services; control characters and quotes in the display name or user agent reached the SIP headers; no quota | Targets that are loopback, link-local, multicast or a port of another service (22, 443, 5432, 6379 ...) are refused (`sip/target.go`, `Config.AllowLocal` for tests); control characters, quotes and backslashes are refused in every text that goes into a header, and every text is at most 255 characters; 20 accounts per user (`sipaccount.MaxAccountsPerUser`). Tests: `sip/target_test.go`, `sipaccount` `TestHeaderInjectionAndQuota`. **Still open:** a user can still probe other *private* addresses on a SIP port and read "refused" or "timeout" on the account's status page |
| **F-03 (medium)** worker isolation | A worker is the controller's own binary under the same uid: a worker taken over through the C library could read `/secrets` and connect to the database | A seccomp filter on every thread (`dect/worker/seccomp_linux.go`) denies opening files, sockets, `connect`, `execve`, `ptrace`, `process_vm_*`, `pidfd_getfd`, mounts; ADR 0003 corrected. Test: `TestConfinedWorkerCannotReachFilesOrNetwork`. **Still open:** cells in one worker share an address space |
| **F-04 (medium)** media of another cell | Any cell could acknowledge or close another cell's media flow | The bridge keeps the cell each MEDIA_OPEN went to and drops answers and closes from any other; the cell link refuses both from a cell that is not active. Test: `TestMediaOfAnotherCellIsNotTouched` |
| **F-06 (medium)** admin API without 2FA | The REST admin API skipped the "admins need 2FA" gate | `apiAdmin` applies it. Test: `TestAdminsNeedTwoFactorWhenRequired` |
| **F-08 (medium)** OIDC login CSRF | The state was not tied to the browser | A cookie bound to the login (migration 0019); the callback needs it. Tests: `auth` and `web` |
| **F-09 (medium)** call-list names | A private contact's name could reach another user's handset on a shared account | One entry per call *and name*. Test: `TestCallsFileKeepsPrivateNamesApart` |
| **F-11 (low)** BYE | A BYE with wrong dialog tags ended the call | The dialog check is honoured. **Still open:** INFO and re-INVITE are matched by Call-ID only |
| **F-14 (low)** unbounded resources | contacts per user, accounts per user | 500 contacts per owner; accounts: see F-02. **Still open:** pre-authentication cell connections, `oidc_login` rows, SIP TCP connections, the worker queue |
| **F-16 (low)** admin's handset | A handset owned by an admin could edit any user's contact | Handsets edit only their owner's own contacts. Test: `TestHandsetOfAnAdminCannotChangeAnotherUsersContact` |
| **F-20 (low)** HSTS, bodies | no HSTS; some POST handlers without a body limit | HSTS when the site is served over https; every request body is capped at 1 MiB |
| I-01 | `golang.org/x/net` v0.60.0 | bumped (see go.mod) |

## Open (not fixed yet)

| Finding | Summary | Suggested fix |
|---|---|---|
| **F-05 (medium)** | SIP TLS verification expectations are shared between accounts: a name union per host, and the SRV target name is accepted | key the expectation by account and domain; drop the SRV target for accounts with verification on |
| **F-07 (medium)** | the controller connects to PostgreSQL as the bootstrap superuser (the threat model says least privilege) | a second role that owns the schema but is not a superuser; the bootstrap role only for migrations; `cap_drop` on the `db` service |
| F-10 (low) | name and location of every active cell are visible to every user, ignoring the sharing switches | non-admins see only their own cells, or `Cell.Public()` |
| F-12 (low) | the login rate limit is check-then-act; Argon2id has no concurrency cap; behind a proxy without `trusted_proxies` all clients share one key | atomic limit; a semaphore around the hashing; a startup warning |
| F-13 (low) | revoking a cell is a best-effort message to its session | close the connection directly and re-check the database each minute |
| F-15 (low) | debug logging holds IPUIs, pages and dialled digits (the threat model says never) | hash identities and mask numbers in debug logs, or say so in B7 |
| F-17 (low) | an OIDC role change applies at the next login (sessions last 7 days) | re-login every 12 h |
| F-18 (low, unconfirmed) | a passive capture of a registration may allow an offline PIN search (the PIN is the authentication code) | verify against EN 300 175-7; then say so in the threat model |
| F-19 (low) | IPUI squatting: the first account to register an IPUI owns it | an admin action to release one; show the conflict |
| I-02..I-06 | Leaflet attribution HTML (admin only), unpinned CI actions and base images, radio-triggered registration lock-out (by design), the SDP peer address decides where RTP goes, `otpauth:` link rewritten by `html/template` | see the report |

The full reports (file and line for every finding, what was checked and found fine) are kept by the maintainer outside
the repository while findings are open.
