# Operating a controller

## Backups

Two things make a backup, and **they must be stored apart**:

1. **The database.** `docker compose exec db pg_dump -U opendectcell opendectcell > opendectcell.sql` (the `pgdata` volume holds
   the live data). It contains handsets' keys, SIP passwords and TOTP secrets, **encrypted** with the data keys.
2. **The master key** (`/secrets/master_key` in the `secrets` volume), which protects the data keys:
   `docker compose cp controller:/secrets/master_key ./master_key.backup`.

A database backup without the master key is useless, and the master key next to the backup defeats its
purpose. **Administration > Backup** explains the same. There is no scheduled or tested restore tool yet: restore
by loading the dump into a fresh PostgreSQL and putting the master key back in the `secrets` volume. Try a
restore once before you need it.

## Upgrading

```sh
cd opendectcell-controller
git pull --recurse-submodules
docker compose up -d --build
```

The controller applies pending database migrations when it starts. To control that step, run
`docker compose exec controller opendectcell-controller migrate status` and `migrate up` yourself and start with
`serve --no-migrate`, which refuses to start while a migration is pending. Take a database backup first.
Migrations are checksummed and take a lock, so two controllers cannot migrate at once. `migrate down` exists but
is for developers.

A controller restart ends running calls and the cells reconnect within seconds. Handsets stay registered.

## Monitoring

| What | How |
|---|---|
| Is it alive, is the database up | `GET /healthz` and `GET /readyz`; the container has a health check |
| Metrics | `GET /metrics` in Prometheus text format, when you set `OPENDECTCELL_HTTP_METRICS_TOKEN`; send it as `Authorization: Bearer …`. About ten series so far (cells, calls, registrations). |
| Cell offline | The controller sends an alert mail when a cell goes offline, if mail is set up |
| Cells and calls | **Administration > Dashboard**, **Cells**, **All calls**, **Statistics** |
| Certificates | The expiry of each cell's certificate is a metric; cells renew themselves at two thirds of its life |

## Logs

`docker compose logs controller`. Format and level: `OPENDECTCELL_LOG_FORMAT` (`text`, `json`) and
`OPENDECTCELL_LOG_LEVEL`. For a problem, raise the level **temporarily** in the web interface: **Administration >
Logging** changes the level at run time and puts an expiry on the change, so debug logging does not stay on. Users
see a restricted **Logging** page for their own handsets and lines.

## Data kept, and for how long

**Administration > Settings > Retention**:

| Data | Default | After that |
|---|---|---|
| A call's number and quality details | 90 days | erased; the call itself (when, how long, how it ended) stays so statistics still add up |
| Location history of handsets (which cell, when) | 90 days | deleted |
| Audit log (who changed what) | 365 days | deleted |

The audit log is append-only: it cannot be edited or deleted except by the retention job. There is no export
or deletion of one user's data yet; see [Limits](Limits.md).

## Secrets and keys

- **Master key**: `rekey` re-wraps the data keys under a new master key (set the new one as `OPENDECTCELL_MASTER_KEY`
  and the old as `OPENDECTCELL_MASTER_KEY_OLD` while you run it, then remove the old one). `rotate-dek` re-encrypts every
  secret under a new data key; **stop the controller first** (it refuses to run beside a live one: the controller keeps the old
  keys in memory). Both are in [deployment.md](../deployment.md) and [threat-model.md](../threat-model.md).
- **Cell certificates** renew themselves (90 days). A cell whose certificate ran out cannot renew and must enrol
  again. **Revoke** a cell on its page to stop it connecting.
- **Users**: **Administration > Users** to approve, disable, reset passwords and see what a user disclosed to the administrators. Local passwords use Argon2id and a
  12-character minimum; failed sign-ins are rate-limited per user name and per address.

## Scaling

The project's aim is hundreds of cells and about 500 handsets. What has been **tested**: the database layer with 500
handsets over 100 cells, and the call routing with 40 simulated cells. What has **not** been tested: 100 real cells
connected at once, CPU and memory of one controller carrying that many DECT stacks, and many simultaneous calls
(the compose file opens 100 audio ports, about 50 calls). By default all cells' DECT stacks run inside the one controller
process, so a crash in the DECT library stops every cell and call until it restarts (Docker's restart policy brings it
back, the compose file sets it). **Worker processes** avoid that: set `OPENDECTCELL_CELLS_STACK_WORKERS` to the number of
CPUs (for example `OPENDECTCELL_CELLS_STACK_WORKERS=4 docker compose up -d controller`): the stacks then run in that many
separate processes, a crash or hang ends one of them, the cells in it restart within seconds and the controller and
the other cells carry on. It has run on the lab (a crash was provoked on purpose) but has not had a long soak, so it is off
by default.
