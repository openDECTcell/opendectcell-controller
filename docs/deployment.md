# Deployment

## Local bring-up

```sh
docker compose up -d
docker compose exec controller opendectcell-controller user create --username admin --role admin
# type the password and press Enter (it is read from standard input)
```

Then open <http://localhost:8080>.

**The DECT core needs libopendectcell.** A plain `docker build` has no DECT core
(`CGO_ENABLED=0`): cells connect and are managed, but their mails are
dropped. The compose file builds with it, which needs the openDECTcell submodule:

```sh
git submodule update --init
docker compose build
```

That links libopendectcell statically with cgo (`-tags libopendectcell`); the log says
`built without libopendectcell` when it is missing. The compose file builds with it by
default (`LIBOPENDECTCELL=1`; `LIBOPENDECTCELL=0 docker compose build` makes the version without).

| Service | Does |
|---|---|
| `init` | One-shot: `init-secrets` creates the master key and the database credentials if they are missing. The controller's files (`master_key`, `database_url`, the runtime role) go in the `secrets` volume, the administrative database credentials (`db_password`, `database_admin_url`) in the `dbsecrets` volume. It never overwrites |
| `db` | PostgreSQL 18; data in the `pgdata` volume; sees only `dbsecrets`. No capabilities beyond those its entrypoint needs |
| `migrate` | One-shot: creates the runtime role, applies the migrations as the administrative role and grants the runtime role its rights. The only service (besides `db`) that sees the administrative credentials |
| `controller` | Waits for `migrate`, connects as the runtime role and serves on `127.0.0.1:8080` (web), `:7300` (cells), `:5060` UDP/TCP (SIP), `41000-41099/udp` (SIP RTP) and `41100-41199/udp` (the cells' speech). Non-root, read-only root filesystem, no capabilities |

**Back up the master key now**, separately from the database. Without it,
every UAK and SIP password in a database backup is unrecoverable
(`threat-model.md` §4.1):

```sh
docker compose cp controller:/secrets/master_key ./master_key.backup
```

## Database roles

The controller connects to PostgreSQL as **`opendectcell_app`**, a role that can read and write the tables and nothing else: it is
not a superuser, cannot change the schema, create roles or databases, and cannot reach other databases. A flaw in the
controller that lets someone run their own SQL therefore cannot, for example, read files of the database host or drop tables.
The role that PostgreSQL was initialised with (`opendectcell`) owns the schema; only `migrate` uses it. The controller container
does not mount its credentials.

- **New installs** get this layout from `init-secrets` (the `init` service).
- **Existing installs** keep working: `init` finds the old `database_url` (which names the bootstrap role), leaves it, copies the
  old `db_password` to the new place, and the log of `serve` warns *"the controller is connected to PostgreSQL as a superuser"*.
  To switch, once: `docker compose run --rm init init-secrets --dir /secrets --admin-dir /dbsecrets --limited-role`, then
  `docker compose up -d` (`init` has made the runtime URL; `migrate` creates the role and grants it what it needs; the
  controller restarts as that role).
- **Own setup** (no compose): set `OPENDECTCELL_DATABASE_URL` to the runtime role and `OPENDECTCELL_DATABASE_ADMIN_URL` to the owner of
  the schema **only for `opendectcell-controller migrate up`**; `serve --no-migrate` needs only the runtime URL. `migrate up` creates the
  runtime role from the user and password in `OPENDECTCELL_DATABASE_URL` if it is missing, brings its password and rights in line, and
  never gives it more than the table rights above. The role that owns the schema must be allowed to create roles.

## Production notes

- **SIP and RTP through Docker's port mapping** work for a few calls, but
  Docker's userland proxy and NAT add latency and rewrite source addresses.
  For production, run the controller with `network_mode: host` and set
  `OPENDECTCELL_SIP_RTP_PORTS` to the range you open in the firewall.

- **Calls need two addresses.** The speech of a call crosses the controller
  twice: cell to controller (UDP `OPENDECTCELL_CELLS_MEDIA_PORTS`, to the address in
  `OPENDECTCELL_CELLS_MEDIA_IP`) and controller to the SIP peer (UDP
  `OPENDECTCELL_SIP_RTP_PORTS`, advertised as `OPENDECTCELL_SIP_MEDIA_IP`). Behind
  Docker's port mapping both must be this host's LAN address, because the
  container's own address is unreachable from outside; with
  `network_mode: host` the SIP one defaults right and the cells' one is the host
  of `OPENDECTCELL_CELLS_PUBLIC_ADDRESS`. Without a valid cells address calls have
  no audio (the log says so at start).

- **Network exposure.** Docker publishes these ports on the host. Only what the use needs should be reachable:

  | Port | Who must reach it | Set the address with | Firewall rule |
  |---|---|---|---|
  | 8080/tcp (web) | the reverse proxy | fixed to 127.0.0.1 | none: it is not published beyond the host |
  | 7300/tcp (cells) | the cells (their boxes) | `OPENDECTCELL_CELL_BIND` | allow the cells' addresses; the port takes enrolment (a one-time token, rate limited) and mutual-TLS sessions |
  | 5060/udp+tcp (SIP) | your SIP provider or telephone system | `OPENDECTCELL_SIP_BIND` | allow the provider's addresses only; a registered account takes calls only from its registrar, and a PBX without registration only from its addresses, but an open SIP port is still attack surface |
  | 41000-41099/udp (RTP) | the SIP peers | `OPENDECTCELL_MEDIA_BIND` | allow the same addresses as SIP |
  | 41100-41199/udp (cells' speech) | the cells | `OPENDECTCELL_MEDIA_BIND` | allow the cells' addresses |

  A SIP port that the whole internet can reach gets scanned within hours; with a provider that registers you
  (outbound), the provider's replies come back through your own connection and need no open port on a NAT router.
  One residual: a request to an account's secret contact token (part of the Contact address the controller gave the
  provider) is accepted from any address, so do not publish contact addresses.

- Put a TLS-terminating reverse proxy in front of port 8080, set
  `OPENDECTCELL_HTTP_BASE_URL=https://…` and `OPENDECTCELL_HTTP_TRUSTED_PROXIES` to the
  proxy's address range.
- Replace the generated secrets with your own secret files
  (`OPENDECTCELL_MASTER_KEY_FILE`, `OPENDECTCELL_DATABASE_URL_FILE`), kept outside the
  database backups.
- `serve` applies pending migrations at start-up. To control that step, run
  `opendectcell-controller migrate up` yourself and start with `serve --no-migrate`,
  which refuses to start on a pending migration.
- Rotation: `rekey` (master key) and `rotate-dek` (data key), as in
  `threat-model.md` §4.3.

## Commands

| Command | Needs config | Does |
|---|---|---|
| `serve [--no-migrate]` | yes | Runs the controller |
| `migrate up \| down [n] \| status` | yes | Schema migrations (`schema.md` §1) |
| `user create --username N --role admin\|user [--display-name] [--email] [--may-add-cells]` | yes | Creates a local user; password on stdin |
| `user set-password --username N` | yes | Sets a local password and ends that user's sessions |
| `rekey` | yes | Re-wraps data keys under a new master key |
| `rotate-dek` | yes | Re-encrypts every secret under a new data key |
| `keygen` | no | Prints a new master key |
| `init-secrets --dir D` | no | As the compose `init` service |
| `healthcheck [--url U]` | no | Exit 0 if `/healthz` answers (container health check) |
| `version` | no | Prints the version |

Endpoints for monitoring: `GET /healthz` (process alive) and `GET /readyz`
(database reachable).
