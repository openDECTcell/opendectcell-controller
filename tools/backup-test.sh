#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Proves the backup service (deploy/backup) on throwaway containers: a database with the controller's schema, a backup that is
# encrypted, checked by restoring it, copied to an SFTP server and pruned; and that a broken database, a wrong SFTP host key and
# a missing key each make the run fail and say so in status.json.
#
#   tools/backup-test.sh [CONTROLLER_IMAGE]      default opendectcell-controller:local
#
# Needs docker and the images (the controller image: docker build -t opendectcell-controller:local .; atmoz/sftp is pulled).
# Makes its own network and volumes and removes everything at the end. Exit status 0 only if every step held.
set -eu

IMAGE=${1:-opendectcell-controller:local}
HERE=$(cd "$(dirname "$0")/.." && pwd)
PG=$(sed -n 's/^    image: \(postgres:[^ ]*\)$/\1/p' "$HERE/docker-compose.yml" | head -1)
ID=backuptest$$
NET=$ID-net
fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "== $*"; }
cleanup() {
	docker rm -f "$ID-db" "$ID-sftp" >/dev/null 2>&1 || true
	docker volume rm -f "$ID-secrets" "$ID-dbsecrets" "$ID-pg" "$ID-backups" "$ID-bsecrets" "$ID-upload" "$ID-keys" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

step "images"
docker build -q -t opendectcell-backup:test "$HERE/deploy/backup" >/dev/null || fail "the backup image does not build"
docker network create "$NET" >/dev/null

step "a database with the schema"
docker run --rm -v "$ID-secrets":/secrets -v "$ID-dbsecrets":/dbsecrets "$IMAGE" init-secrets --dir /secrets --admin-dir /dbsecrets >/dev/null
docker run -d --name "$ID-db" --network "$NET" --network-alias db -e POSTGRES_DB=opendectcell -e POSTGRES_USER=opendectcell \
	-e POSTGRES_PASSWORD_FILE=/dbsecrets/db_password -v "$ID-dbsecrets":/dbsecrets:ro -v "$ID-pg":/var/lib/postgresql "$PG" >/dev/null
i=0; until docker exec "$ID-db" pg_isready -U opendectcell -d opendectcell >/dev/null 2>&1; do i=$((i + 1)); [ $i -lt 60 ] || fail "the database did not start"; sleep 1; done
docker run --rm --network "$NET" -e OPENDECTCELL_DATABASE_URL_FILE=/secrets/database_url -e OPENDECTCELL_DATABASE_ADMIN_URL_FILE=/dbsecrets/database_admin_url \
	-e OPENDECTCELL_MASTER_KEY_FILE=/secrets/master_key -e OPENDECTCELL_HTTP_BASE_URL=http://controller:8080 \
	-v "$ID-secrets":/secrets:ro -v "$ID-dbsecrets":/dbsecrets:ro "$IMAGE" migrate up >/dev/null || fail "migrate up"
docker exec "$ID-db" psql -U opendectcell -d opendectcell -qc "INSERT INTO app_user (username, role, auth_source) VALUES ('backup-test', 'admin', 'local')" || fail "no test user"

step "an SFTP server with a key of ours"
docker run --rm -v "$ID-bsecrets":/backup-secrets --entrypoint sh opendectcell-backup:test -c \
	'ssh-keygen -q -t ed25519 -N "" -f /backup-secrets/sftp_key && chown -R postgres /backup-secrets && chmod 700 /backup-secrets' || fail "ssh-keygen"
docker run --rm -v "$ID-bsecrets":/backup-secrets -v "$ID-keys":/keys --entrypoint sh opendectcell-backup:test -c 'cp /backup-secrets/sftp_key.pub /keys/id.pub'
docker run --rm -v "$ID-upload":/u --entrypoint sh opendectcell-backup:test -c "chown 1001:100 /u" # the volume is made for the SFTP user
docker run -d --name "$ID-sftp" --network "$NET" -v "$ID-keys":/home/bk/.ssh/keys:ro -v "$ID-upload":/home/bk/upload atmoz/sftp:alpine bk::1001:100:upload >/dev/null
i=0; until docker run --rm --network "$NET" --entrypoint sh opendectcell-backup:test -c "ssh-keyscan -t ed25519 $ID-sftp 2>/dev/null | grep -q ssh-ed25519"; do
	i=$((i + 1)); [ $i -lt 30 ] || fail "the SFTP server did not start"; sleep 1
done
docker run --rm --network "$NET" -v "$ID-bsecrets":/backup-secrets --entrypoint sh opendectcell-backup:test -c \
	"ssh-keyscan -t ed25519 $ID-sftp 2>/dev/null > /backup-secrets/sftp_known_hosts && chown postgres /backup-secrets/sftp_known_hosts"

run() { # run [ENV=VALUE...] -- ARGS   (the backup service, once)
	envs=""
	while [ "$1" != -- ]; do envs="$envs -e $1"; shift; done; shift
	# shellcheck disable=SC2086
	docker run --rm --network "$NET" --read-only --tmpfs /tmp:size=512m --cap-drop ALL --cap-add CHOWN --cap-add SETUID --cap-add SETGID --cap-add DAC_OVERRIDE --cap-add FOWNER --security-opt no-new-privileges:true \
		-e BACKUP_SFTP_TARGET="bk@$ID-sftp:upload" -e BACKUP_KEEP=2 $envs \
		-v "$ID-dbsecrets":/dbsecrets:ro -v "$ID-backups":/backups -v "$ID-bsecrets":/backup-secrets opendectcell-backup:test "$@"
}
status() { docker run --rm -v "$ID-backups":/backups --entrypoint cat opendectcell-backup:test /backups/status.json; }
files() { docker run --rm -v "$ID-backups":/backups --entrypoint sh opendectcell-backup:test -c 'ls -1 /backups | grep "^opendectcell-.*dump.gpg$" | wc -l'; }
remote() { docker run --rm -v "$ID-upload":/u --entrypoint sh opendectcell-backup:test -c 'ls -1 /u | grep "^opendectcell-.*dump.gpg$" | wc -l'; }
has() { status | grep -q "$1" || { status >&2; fail "status.json lacks: $1"; }; }

step "one backup: encrypted, checked, copied"
run -- once || fail "the backup failed"
has '"ok":true'; has '"check":{"ran":true,"ok":true'; has '"sftp":{"configured":true,"ok":true'
[ "$(files)" = 1 ] || fail "no backup file"
[ "$(remote)" = 1 ] || fail "nothing on the SFTP server"
docker run --rm -v "$ID-backups":/backups --entrypoint sh opendectcell-backup:test -c 'head -c 5 /backups/opendectcell-*.dump.gpg | grep -q PGDMP' && fail "the backup is not encrypted"
docker run --rm -v "$ID-backups":/backups -v "$ID-bsecrets":/backup-secrets --entrypoint sh opendectcell-backup:test -c \
	'f=$(ls /backups/opendectcell-*.dump.gpg | head -n 1); /usr/local/bin/opendectcell-backup decrypt "$f" | pg_restore --list | grep -q app_user' || fail "the backup does not decrypt to a dump of the schema"
docker run --rm -v "$ID-backups":/backups --entrypoint sh opendectcell-backup:test -c 'grep -q backup-test /backups/opendectcell-*.dump.gpg' && fail "the user name is readable in the file"

step "the check can fail"
docker run --rm --network "$NET" -v "$ID-dbsecrets":/dbsecrets:ro -v "$ID-backups":/backups -v "$ID-bsecrets":/backup-secrets --entrypoint sh opendectcell-backup:test -c '
	cd /backups; f=$(ls opendectcell-*.dump.gpg | head -n 1)
	/usr/local/bin/opendectcell-backup check "$f" || exit 10
	# one flipped byte: the encryption is authenticated, it does not open
	cp "$f" /tmp/flipped.dump.gpg; printf "\\377" | dd of=/tmp/flipped.dump.gpg bs=1 seek=200 conv=notrunc 2>/dev/null
	if /usr/local/bin/opendectcell-backup check /tmp/flipped.dump.gpg; then exit 11; fi
	# a file that is encrypted fine but is not a dump of this database
	echo "not a dump" | gpg --batch --quiet --pinentry-mode loopback --passphrase-file /backup-secrets/passphrase --symmetric -o /tmp/junk.dump.gpg
	if /usr/local/bin/opendectcell-backup check /tmp/junk.dump.gpg; then exit 12; fi
	# a dump of another schema: fewer tables than the live database
	PGHOST=db PGUSER=opendectcell PGPASSWORD=$(cat /dbsecrets/db_password) pg_dump -Fc -t app_user opendectcell | gpg --batch --quiet --pinentry-mode loopback --passphrase-file /backup-secrets/passphrase --symmetric -o /tmp/part.dump.gpg
	if /usr/local/bin/opendectcell-backup check /tmp/part.dump.gpg; then exit 13; fi
' || fail "the check did not behave (exit $?: 10 good file, 11 flipped, 12 junk, 13 partial)"

step "retention: two kept, here and there"
sleep 1; run -- once || fail "second"; sleep 1; run -- once || fail "third"
[ "$(files)" = 2 ] || fail "$(files) files here, want 2"
[ "$(remote)" = 2 ] || fail "$(remote) files on the SFTP server, want 2"

step "failures are reported"
run BACKUP_DB_PASSWORD_FILE=/nonexistent -- once && fail "a backup with a wrong database password passed"
has '"ok":false'
run BACKUP_SFTP_KNOWN_HOSTS_FILE=/backup-secrets/nothing -- once && fail "a backup without the host key passed"
has '"ok":false'; has 'SFTP'
run BACKUP_SFTP_TARGET= -- once || fail "a local backup alone failed"
has '"ok":true'; has '"sftp":{"configured":false'
run BACKUP_AT=25:99 -- && fail "a bad time was accepted"

step "ok"
