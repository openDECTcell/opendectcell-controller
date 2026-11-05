# Operating a controller

## A user's data

**Account > My activity** shows a user their own sign-ins and the failed attempts on their user name, and the changes they made or that were made to their account (by an administrator, with the name), from the audit log. **Account > Your data**: a user downloads everything the controller holds about them (JSON, no secrets) and can **delete their account and data**:
handsets (the base stations forget them), VoIP lines, the base stations they enrolled (revoked), contacts, call history and sessions go. The audit
log keeps what happened but forgets who did it. The last administrator cannot delete themselves. Administrators remove other users in **Administration > Users**
once their handsets and VoIP lines are gone, and release a handset registered under someone else's name in **Administration > Release a handset**.

## Backups

A controller has two things that cannot be rebuilt, and **they must be stored apart**:

1. **The database**: users, handsets and their keys, VoIP lines and their passwords, cells, settings, call log, audit log. The keys and passwords in it are
   **encrypted** with the data keys; the rest is not secret from whoever holds a dump.
2. **The master key** (`/secrets/master_key` in the `secrets` volume), which protects the data keys:
   `docker compose cp controller:/secrets/master_key ./master_key.backup`. It is **not** in any backup. Without it a restored database cannot open its secrets;
   with it next to the dump, anyone holding the dump can.

### The backup service (nightly)

The compose file has a **`backup`** service. Every night (at `OPENDECTCELL_BACKUP_AT`, default 03:15, in the zone `OPENDECTCELL_TZ`) it:

1. dumps the database (`pg_dump -Fc`) and **encrypts** the dump with a **passphrase** of its own (gpg, AES-256, with an integrity check): the files hold personal data
   and must not be readable by whoever finds them;
2. **checks that it restores**: it reads the file back, loads it into a scratch PostgreSQL that lives for a minute inside the service, and compares the tables and the
   users with the live database. A backup that does not restore is reported as failed;
3. copies it to an **SFTP server** if you set one up (below);
4. removes the oldest beyond `OPENDECTCELL_BACKUP_KEEP` (default 14), here and on the SFTP server;
5. writes `status.json`, which the controller shows under **Administration > Backup** (last run, restore check, SFTP, the files, a download of any of them) and uses to
   tell the administrators when the last run failed or the last good backup is more than 36 hours old ([Notifications](Notifications.md)).

**The passphrase.** The first start makes one in the `backup-secrets` volume and prints it **once** in the log (`docker compose logs backup`). **Copy it away now**, apart from
the backups and from the master key: without it no backup can be read, and nobody can recover it for you. The controller does not have it either, so a downloaded backup is useless
to whoever steals it from the controller.

```sh
docker compose up -d backup                 # start it (it is part of the stack)
docker compose logs backup                  # the passphrase, once, on its first start
docker compose exec backup opendectcell-backup once    # make one backup now; exit status 0 only if it held
docker compose exec backup opendectcell-backup check /backups/opendectcell-....dump.gpg   # does this file restore?
```

**SFTP.** Set `OPENDECTCELL_BACKUP_SFTP_TARGET=user@host:directory` (and `OPENDECTCELL_BACKUP_SFTP_PORT` if it is not 22) in the environment of `docker compose`, and put two files in the
`backup-secrets` volume: **`sftp_key`**, a private key without a passphrase (the public key goes into the server's `authorized_keys`; use a key that may only write there), and
**`sftp_known_hosts`**, the server's host key (`ssh-keyscan -t ed25519 host`, checked by you). The service never trusts a host key on first use: a wrong or changed key fails the backup
and says so. The upload goes to a `.part` name and is renamed when complete.

```sh
docker run --rm -v opendectcell_backup-secrets:/s alpine sh -c 'ls -l /s'    # what is there
docker cp ./sftp_key $(docker compose ps -q backup):/backup-secrets/sftp_key   # or copy them into the volume any way you like
```

**Restoring** (try it once before you need it):

```sh
docker compose exec backup opendectcell-backup decrypt /backups/opendectcell-20261105-031500.dump.gpg > opendectcell.dump   # or gpg --decrypt with the passphrase, anywhere
# a fresh stack: start db and migrate, stop the controller, then
pg_restore --clean --if-exists --no-owner -d opendectcell opendectcell.dump
# put the same master key back in the `secrets` volume, then: docker compose run --rm migrate && docker compose up -d
```

The nightly check proves the file restores into PostgreSQL; `tools/restore-test.sh [IMAGE]` goes further and proves the restored database lets a user log in and opens every
encrypted value under the master key; `tools/backup-test.sh` tests the service itself on throwaway containers. Run them after changing anything about backups.

The backup service sees the database's administrative credentials (read only), like the `migrate` service. If you make backups some other way, leave the service out
(`docker compose up -d` without `backup`, or `docker compose rm backup`): the Administration > Backup page then says no service is running, and nothing is warned.

## The base stations' certificate authority

Every controller has one **certificate authority (CA)** that its base stations trust: it signs the controller's server certificate (1 year) and each base station's certificate (90 days, renewed by the
base station over its link, long before it ends). The CA is made on the first start, valid for **10 years**, and its key is stored sealed with the master key. Every base station **pins** the CA's public
key when it is enrolled: that pin is in the enrolment string, and a controller whose CA has another pin is not the controller the base station was enrolled with.

**What to know**

* A certificate is never valid longer than the CA: in the last months the base stations' certificates get shorter, and **when the CA ends no base station can connect any more**. The notification centre
  tells administrators 90 days before (see [Notifications](Notifications.md)); the date is in the table `cell_ca` (`docker compose exec db psql -U opendectcell -c "select id, active, not_after from cell_ca"`).
* The CA is the **same across restarts and upgrades**, and across a restore of the database with its master key. It cannot be opened without the master key that sealed it: a controller started with another
  master key **fails to start** instead of making a new CA (a test pins this down).
* A new CA is made **only when none is active**. That is what happens, with another pin, if the row is deleted or retired (`update cell_ca set active = false`) and the controller started.
* Rotating the CA, with the old and the new one trusted side by side so that nothing has to be done at the boxes, is **not built** (a time bomb for 2036, not an emergency).

**If the CA is lost or about to end: enrol every base station again.** The only way today:

1. Pick a quiet time: calls end and base stations are offline until they are enrolled again. Take a [database backup](#backups) first, and keep the master key.
2. Stop the controller, retire the CA (`update cell_ca set active = false`), start the controller. It makes a new CA and logs its pin (`cell_ca_pin`). The base stations now refuse it: they show as offline.
3. For each base station, enrol it again as the first time ([Quick start](Quick-start.md)): make an enrolment under **Base stations**, and give the string to the box.
4. Handsets keep their registration: their keys are in the database and do not depend on the CA.

**Not tested end to end:** whether a base station that is enrolled again keeps its name and settings or comes as a new entry, and how the old entry is best removed. Try it with one spare box first, and write down what you
see in the maintainers' notes. Until the CA's end is near, none of this is needed.

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
| Is it all right | **Administration > Health**: one list of checks, green or red with the cause: the database and its schema, the clock, outgoing mail (with a button that sends you a test mail), mail that waits too long, base stations connected, the certificate authority and the base stations' certificates, VoIP lines registered, the last backup and the space left for backups |
| Is it alive, is the database up | `GET /healthz` and `GET /readyz`; the container has a health check |
| A page for the users of a hosted instance | `GET /status` answers anybody with `ok`, `degraded` (a check of Health fails) or `down` (the database does not answer, HTTP 503) and the time, and nothing of what is inside, when you set `OPENDECTCELL_HTTP_PUBLIC_STATUS=true`; otherwise it is a 404 |
| Metrics | `GET /metrics` in Prometheus text format, when you set `OPENDECTCELL_HTTP_METRICS_TOKEN`; send it as `Authorization: Bearer …`. About ten series so far (cells, calls, registrations). |
| Alerts | The bell and, if you want, mail: a VoIP line down, a base station offline, certificates ending, a backup that failed or is old ([Notifications](Notifications.md)) |
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
| A VoIP line's registration history (up, down, why) | 90 days, fixed | deleted |
| Notifications (the bell) | 90 days, fixed | deleted |
| The log of what webhooks posted | 7 days, fixed | deleted |

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
