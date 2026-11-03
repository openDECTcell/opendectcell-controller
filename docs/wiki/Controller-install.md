# Install the controller

The controller is one Docker Compose project: a PostgreSQL database and the controller itself. It needs a
server that stays on, and that the cells (the FRITZ!Boxes) can reach over the network.

## 1. Get it and start it

```sh
git clone --recurse-submodules https://github.com/openDECTcell/opendectcell-controller
cd opendectcell-controller
docker compose up -d --build
```

The build links the DECT library `libopendectcell` from the `opendectcell` submodule into the controller. If the
submodule is missing the build stops and says so. (`LIBOPENDECTCELL=0` builds a controller without the DECT
core: cells connect, but all their messages are dropped. It is for development only.)

| Service | What it does |
|---|---|
| `init` | Runs once and creates the secrets if they are missing: `master_key` and the controller's `database_url` in the `secrets` volume, the database's own administrative password in the `dbsecrets` volume. It never overwrites. |
| `migrate` | Runs once before the controller: sets up the database role the controller uses (it can read and write the tables, nothing more) and applies the schema changes. |
| `db` | PostgreSQL 18; data in the `pgdata` volume. |
| `controller` | Waits for `migrate`, then serves, connected to the database with its limited role. Runs as a non-root user with a read-only filesystem and no capabilities. |

## 2. The first administrator

```sh
docker compose exec controller opendectcell-controller user create --username admin --role admin
```

The password is read from standard input. More users: the same command with `--role user` (add
`--may-add-cells` to let that user enrol cells), or **Administration > Users** in the web interface.
`user set-password --username N` sets a new local password and ends that user's sessions.

Open the web interface on <http://localhost:8080>. By default the web port is published on `127.0.0.1` only.

**Back up the master key now**, separately from the database. Without it every stored key and SIP password in
a database backup is unrecoverable:

```sh
docker compose cp controller:/secrets/master_key ./master_key.backup
```

## 3. Ports and addresses

| Port | Protocol | For |
|---|---|---|
| 8080 | TCP | Web interface and API (published on `127.0.0.1` by default) |
| 7300 | TCP | Cells connect here (TLS 1.3, mutual authentication) |
| 5060 | UDP and TCP | SIP |
| 41000–41099 | UDP | The audio of SIP calls (two ports per call) |
| 41100–41199 | UDP | The audio between cells and the controller (one port per call) |

**A call needs two addresses to be right.** The speech crosses the controller twice: cell to controller, and
controller to the SIP peer. Set them when the controller does not run with `network_mode: host`, which is
the usual case with Docker's port mapping:

| Setting | Meaning |
|---|---|
| `OPENDECTCELL_CELLS_PUBLIC_ADDRESS` | `host:port` as the cells reach the control port. It is written into enrollment strings. |
| `OPENDECTCELL_CELLS_MEDIA_IP` | The address the cells send a call's audio to: this server's LAN address. |
| `OPENDECTCELL_SIP_MEDIA_IP` | The address advertised to the SIP peer for audio: this server's address as the peer reaches it. |
| `OPENDECTCELL_HTTP_BASE_URL` | The external URL of the web interface, for example `https://dect.example.org`. |

Put them in `docker-compose.override.yml` (`services: controller: environment:`) and a `.env` file next to
`docker-compose.yml`. Without a valid cells media address calls connect but have **no audio**; the log says so
at start.

For more than a few calls at once, widen both UDP ranges, or run the controller with `network_mode: host`
and set `OPENDECTCELL_SIP_RTP_PORTS` to the range you open in the firewall. Docker's userland proxy adds
latency and rewrites source addresses.

## 4. Production notes

- **TLS.** Put a TLS-terminating reverse proxy in front of port 8080, set `OPENDECTCELL_HTTP_BASE_URL=https://…`
  and `OPENDECTCELL_HTTP_TRUSTED_PROXIES` to the proxy's address range. With `https://` the cookies become Secure.
- **Your own secrets.** Replace the generated ones with files outside the database backups
  (`OPENDECTCELL_MASTER_KEY_FILE`, `OPENDECTCELL_DATABASE_URL_FILE`).
- **Migrations.** `serve` applies pending migrations at start. To control that step run
  `opendectcell-controller migrate up` yourself and start with `serve --no-migrate`, which refuses to start on a
  pending migration.
- **Mail.** Set `OPENDECTCELL_SMTP_HOST` (and `OPENDECTCELL_SMTP_FROM`) to let the controller send sign-up mails,
  password resets and the alert when a cell goes offline. Without it no mail is sent.
- **Single sign-on.** Set `OPENDECTCELL_OIDC_ISSUER`, `OPENDECTCELL_OIDC_CLIENT_ID` and the client secret, and map
  groups to roles with `OPENDECTCELL_OIDC_ADMIN_GROUPS` and `OPENDECTCELL_OIDC_USER_GROUPS`. With the user groups empty,
  **every** user the identity provider authenticates for this client may sign in: restrict access at the
  provider. A worked example for Authentik: [authentik.md](../authentik.md).
- **Sign-up.** **Administration > Settings** decides whether visitors may create accounts and whether an
  administrator approves them first.
- **Two-factor sign-in** for local accounts can be required of administrators in the same place. A locked-out
  administrator is reset on the server with `opendectcell-controller user reset-2fa`.

Every setting and its default: [configuration.md](../configuration.md). Every command:
[deployment.md](../deployment.md).

## 5. Checking that it runs

```sh
docker compose ps
curl -s http://127.0.0.1:8080/healthz      # the process is alive
curl -s http://127.0.0.1:8080/readyz       # and the database answers
docker compose logs -f controller
```

The web interface's **Administration > Dashboard** shows what is online. See also [Operating a controller](Operating.md).
