# Configuration

The controller reads an optional YAML file (`--config FILE` or
`OPENDECTCELL_CONFIG`) and then the environment, which wins. Unknown keys in the
file are an error, and every problem is reported at once on start-up.

**Secrets** (marked 🔒) can also be read from a file: `OPENDECTCELL_<NAME>_FILE`,
e.g. a Docker secret. Setting both forms is an error. Secret values are never
echoed in error messages or logs.

| Variable | YAML | Default | Meaning |
|---|---|---|---|
| `OPENDECTCELL_HTTP_LISTEN` | `http.listen` | `:8080` | Web UI and API address |
| `OPENDECTCELL_HTTP_BASE_URL` | `http.base_url` | **required** | External URL, e.g. `https://dect.example.org`. `https://` turns on Secure, `__Host-` cookies; it also sets the OIDC redirect URL `<base>/auth/oidc/callback` |
| `OPENDECTCELL_HTTP_TRUSTED_PROXIES` | `http.trusted_proxies` | none | Comma-separated CIDRs of reverse proxies whose `X-Forwarded-For` is believed |
| `OPENDECTCELL_HTTP_METRICS_TOKEN` 🔒 | `http.metrics_token` | none | Turns on `/metrics` (Prometheus text) for clients that send this as `Authorization: Bearer`. At least 32 characters. Empty: no endpoint. The metrics name your cells, so do not publish the token |
| `OPENDECTCELL_DATABASE_URL` 🔒 | `database.url` | **required** | PostgreSQL URL |
| `OPENDECTCELL_DATABASE_ADMIN_URL` 🔒 | `database.admin_url` | none | The role that owns the schema, **for `migrate` only**: it creates the role in `DATABASE_URL`, applies the migrations and grants that role the table rights. Do not give it to the running controller (`deployment.md`, "Database roles") |
| `OPENDECTCELL_DATABASE_MAX_CONNS` | `database.max_conns` | 10 | Pool size |
| `OPENDECTCELL_MASTER_KEY` 🔒 | `secrets.master_key` | **required** | Base64 master key; `opendectcell-controller keygen` makes one (`threat-model.md` §4.1) |
| `OPENDECTCELL_MASTER_KEY_OLD` 🔒 | `secrets.old_master_key` | — | Only while rotating the master key (`rekey`) |
| `OPENDECTCELL_LOG_LEVEL` | `log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `OPENDECTCELL_LOG_FORMAT` | `log.format` | `text` | `text` or `json` |
| `OPENDECTCELL_AUTH_LOCAL_ENABLED` | `auth.local.enabled` | `true` | Local username/password login |
| `OPENDECTCELL_AUTH_ARGON_MEMORY_KIB` | `auth.local.argon_memory_kib` | 65536 | Argon2id memory (min 19456) |
| `OPENDECTCELL_AUTH_ARGON_TIME` | `auth.local.argon_time` | 3 | Argon2id passes |
| `OPENDECTCELL_AUTH_ARGON_THREADS` | `auth.local.argon_threads` | 2 | Argon2id parallelism |
| `OPENDECTCELL_AUTH_SESSION_IDLE` | `auth.session_idle` | `12h` | Session ends after this long without a request |
| `OPENDECTCELL_AUTH_SESSION_MAX_AGE` | `auth.session_max_age` | `168h` | Session ends after this long regardless |
| `OPENDECTCELL_AUTH_LOGIN_RATE_LIMIT` | `auth.login_rate_limit` | 10 | Failed logins per username and per client address per 15 minutes |
| `OPENDECTCELL_OIDC_ISSUER` | `auth.oidc.issuer` | — | Turns OIDC on. `https` (or loopback `http` for testing) |
| `OPENDECTCELL_OIDC_CLIENT_ID` | `auth.oidc.client_id` | — | Required with an issuer |
| `OPENDECTCELL_OIDC_CLIENT_SECRET` 🔒 | `auth.oidc.client_secret` | — | Confidential client secret |
| `OPENDECTCELL_OIDC_SCOPES` | `auth.oidc.scopes` | `openid,profile,email` | Must include `openid`. Authentik needs no extra scope for `groups` |
| `OPENDECTCELL_OIDC_GROUPS_CLAIM` | `auth.oidc.groups_claim` | `groups` | ID-token claim listing the user's groups |
| `OPENDECTCELL_OIDC_ADMIN_GROUPS` | `auth.oidc.admin_groups` | none | Groups that grant `admin` |
| `OPENDECTCELL_OIDC_USER_GROUPS` | `auth.oidc.user_groups` | none | Groups that grant `user`. **Empty: every user the IdP authenticates for this client may log in** — restrict access at the IdP, or set this |
| `OPENDECTCELL_OIDC_PROVIDER_NAME` | `auth.oidc.provider_name` | `Single sign-on` | Login button label |
| `OPENDECTCELL_CELLS_LISTEN` | `cells.listen` | `:7300` | Cell control port (`cell-protocol.md` §1) |
| `OPENDECTCELL_SIP_UDP_LISTEN` | `sip.udp_listen` | `:5060` | The UDP socket all SIP accounts share (`sip-ua.md` §2) |
| `OPENDECTCELL_SIP_TCP_LISTEN` | `sip.tcp_listen` | `:5060` | Incoming SIP over TCP; empty turns it off. Outgoing TCP/TLS connections are made as needed |
| `OPENDECTCELL_SIP_RTP_PORTS` | `sip.rtp_ports` | `41000-41999` | RTP/RTCP port pairs for the SIP side, two ports per call. Separate from the cell media ports (`cell-protocol.md` §1) |
| `OPENDECTCELL_SIP_MEDIA_IP` | `sip.media_ip` | local address | Advertise this IP in SDP, for a controller behind a static NAT |
| `OPENDECTCELL_CELLS_PUBLIC_ADDRESS` | `cells.public_address` | base URL host + listen port | `host:port` as cells reach the control port; written into enrollment strings. Set it when cells connect through NAT or another name |
| `OPENDECTCELL_CELLS_REQUIRE_DECT_CORE` | `cells.require_dect_core` | `false` | `true`: `serve` refuses to start if the binary was built without libopendectcell (no DECT core: every cell's mails would be dropped). The compose file sets it |
| `OPENDECTCELL_CELLS_HANDSET_POOL` | `cells.handset_pool` | `true` | A handset that enrols is given a TPUI from a pool (90 values the 7412 chip hears), unique among the handsets in one cell's table, not across the controller: a cell that hears a handset whose TPUI another handset in its table holds gives it a new one (`roaming.md` §4.2); `false`: it gets the TPUI derived from its record number, as before. Handsets enrolled earlier keep theirs either way. Every handset can be heard on every cell in both cases |
| `OPENDECTCELL_CELLS_STACK_WORKERS` | `cells.stack_workers` | `0` | How many worker processes run the cells' DECT stacks (ADR 0003, `adr/0003-stack-isolation.md`). `0`: inside the controller process, where a crash in the C library ends the controller. `N`: N processes of this binary (`cellworker`, no database, no keys of its own, no network); a crash or hang ends one worker and the cells in it restart, the controller and the other workers carry on. A good number is the CPU count. |
| `OPENDECTCELL_CELLS_MEDIA_IP` | `cells.media_ip` | host of `cells.public_address`, if that is an IP | The address cells send a call's speech to (MEDIA_OPEN `ctl_addr`, `cell-protocol.md` §8). Behind Docker's port mapping: the host's LAN address the cells reach. **`auto`**: each cell is sent to the address it connected to (the server end of its link), which is right with no address translation in between (the host network) and when cells reach the controller by different addresses, e.g. a LAN address and a public IPv6 address. Without a valid address calls have no audio |
| `OPENDECTCELL_CELLS_MEDIA_PORTS` | `cells.media_ports` | `41100-41199` | UDP ports the controller listens on for the cells' speech, one per call; publish the range like the SIP RTP range |
| `OPENDECTCELL_CARDDAV_ALLOW_PRIVATE` | `carddav.allow_private` | `false` | `true`: the controller may fetch the address books users link from private network addresses (10/8, 172.16/12, 192.168/16, fc00::/7), for a Nextcloud on your own network. Off, a user could make the controller probe the network around it. Loopback and link-local addresses (the cloud metadata service) are never allowed |
| `OPENDECTCELL_SMTP_HOST` | `smtp.host` | none | Outgoing mail server. Without it the controller sends no mail (log-level change notices, sign-up mails) and the settings page says so |
| `OPENDECTCELL_SMTP_PORT` | `smtp.port` | `587` | |
| `OPENDECTCELL_SMTP_USER` | `smtp.user` | none | Empty: no authentication |
| `OPENDECTCELL_SMTP_PASSWORD` 🔒 | `smtp.password` | none | |
| `OPENDECTCELL_SMTP_FROM` | `smtp.from` | **required with a host** | Sender address, e.g. `dect@example.org` |
| `OPENDECTCELL_SMTP_SECURITY` | `smtp.security` | `starttls` | `starttls`, `tls` (implicit TLS, port 465) or `none` (a trusted local relay only) |

For OIDC users, the role is recomputed from the groups at every login, so
removing someone from the admin group takes effect at their next login.
Their existing sessions keep their role until they end (`session_idle`).

Example file:

```yaml
http:
  base_url: https://dect.example.org
  trusted_proxies: [172.16.0.0/12]
auth:
  oidc:
    issuer: https://auth.example.org/application/o/opendectcell/
    client_id: opendectcell
    admin_groups: [dect-admins]
    user_groups: [staff]
```
