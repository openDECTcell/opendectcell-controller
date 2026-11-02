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
| `init` | One-shot: `init-secrets` creates `master_key`, `db_password` and `database_url` in the `secrets` volume if they are missing. It never overwrites |
| `db` | PostgreSQL 18; data in the `pgdata` volume |
| `controller` | Applies migrations, then serves on `127.0.0.1:8080` (web), `:7300` (cells), `:5060` UDP/TCP (SIP), `41000-41099/udp` (SIP RTP) and `41100-41199/udp` (the cells' speech). Non-root, read-only root filesystem, no capabilities |

**Back up the master key now**, separately from the database. Without it,
every UAK and SIP password in a database backup is unrecoverable
(`threat-model.md` §4.1):

```sh
docker compose cp controller:/secrets/master_key ./master_key.backup
```

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
