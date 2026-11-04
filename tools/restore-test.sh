#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Proves that a backup restores (audit finding BUILD-06): a database dump plus the master key, loaded into a fresh
# PostgreSQL, gives a controller that knows its users and can open its encrypted secrets.
#
#   tools/restore-test.sh [IMAGE]        default image: opendectcell-controller:local
#
# It needs docker, the image (docker build -t opendectcell-controller:local . is enough: the test needs no DECT core) and the
# postgres image the compose file names. It makes its own network and volumes, publishes no port, and removes everything
# at the end (a failed run leaves nothing behind either). Exit status 0 only if every step held.
#
# Steps: make secrets; start a database and migrate it; create a user; start the controller, log in and set up two-factor
# login (that seals a secret under the data key); stop it; pg_dump; start a SECOND, empty database; restore the dump into it
# as the documented procedure says (the dump, then `migrate up`, with the same secrets); check the user's password,
# that every sealed value opens under the master key (rotate-dek re-encrypts them all, so it fails on one it cannot open),
# and that a controller started on the restored database lets the user log in.
set -eu

IMAGE=${1:-opendectcell-controller:local}
PG=$(sed -n 's/^    image: \(postgres:[^ ]*\)$/\1/p' "$(dirname "$0")/../docker-compose.yml" | head -1)
[ -n "$PG" ] || { echo "no postgres image in docker-compose.yml" >&2; exit 2; }
ID=restoretest$$
NET1=$ID-a
NET2=$ID-b
SECRETS=$ID-secrets
DBSECRETS=$ID-dbsecrets
fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "== $*"; }

cleanup() {
	docker rm -f "$ID-db1" "$ID-db2" "$ID-ctl1" "$ID-ctl2" >/dev/null 2>&1 || true
	docker volume rm -f "$SECRETS" "$DBSECRETS" "$ID-pg1" "$ID-pg2" >/dev/null 2>&1 || true
	docker network rm "$NET1" "$NET2" >/dev/null 2>&1 || true
	rm -rf "${WORK:-/nonexistent}"
}
trap cleanup EXIT INT TERM
WORK=$(mktemp -d)
chmod 777 "$WORK" # the curl container runs as another user and writes its cookie jar here

docker network create "$NET1" >/dev/null
docker network create "$NET2" >/dev/null

# the controller's commands, on a network whose database is called "db"
ctl() { # ctl NETWORK ARGS...   (stdin passes through)
	net=$1; shift
	docker run --rm -i --network "$net" \
		-e OPENDECTCELL_DATABASE_URL_FILE=/secrets/database_url -e OPENDECTCELL_DATABASE_ADMIN_URL_FILE=/dbsecrets/database_admin_url \
		-e OPENDECTCELL_MASTER_KEY_FILE=/secrets/master_key -e OPENDECTCELL_HTTP_BASE_URL=http://controller:8080 \
		-e OPENDECTCELL_CELLS_REQUIRE_DECT_CORE=false \
		-v "$SECRETS":/secrets:ro -v "$DBSECRETS":/dbsecrets:ro "$IMAGE" "$@"
}
start_db() { # start_db NAME NETWORK VOLUME
	docker run -d --name "$1" --network "$2" --network-alias db \
		-e POSTGRES_DB=opendectcell -e POSTGRES_USER=opendectcell -e POSTGRES_PASSWORD_FILE=/dbsecrets/db_password \
		-v "$DBSECRETS":/dbsecrets:ro -v "$3":/var/lib/postgresql "$PG" >/dev/null
	i=0
	until docker exec "$1" pg_isready -U opendectcell -d opendectcell >/dev/null 2>&1; do
		i=$((i + 1)); [ $i -lt 60 ] || fail "database $1 did not start"; sleep 1
	done
	sleep 2 # pg_isready answers during the entrypoint's temporary server; wait for the real one
	i=0
	until docker exec "$1" pg_isready -U opendectcell -d opendectcell >/dev/null 2>&1; do i=$((i + 1)); [ $i -lt 30 ] || fail "database $1 restarted badly"; sleep 1; done
}
start_ctl() { # start_ctl NAME NETWORK
	docker run -d --name "$1" --network "$2" --network-alias controller \
		-e OPENDECTCELL_DATABASE_URL_FILE=/secrets/database_url -e OPENDECTCELL_MASTER_KEY_FILE=/secrets/master_key \
		-e OPENDECTCELL_HTTP_BASE_URL=http://controller:8080 -e OPENDECTCELL_CELLS_REQUIRE_DECT_CORE=false \
		-v "$SECRETS":/secrets:ro "$IMAGE" serve --no-migrate >/dev/null
	i=0
	until web "$2" -sf http://controller:8080/readyz >/dev/null 2>&1; do
		i=$((i + 1)); [ $i -lt 60 ] || { docker logs "$1" 2>&1 | tail -5 >&2; fail "controller $1 did not become ready"; }; sleep 1
	done
}
web() { # web NETWORK curl-args...
	net=$1; shift
	docker run --rm --network "$net" -v "$WORK":/work -w /work curlimages/curl:8.10.1 "$@"
}
login() { # login NETWORK -> the status of the login and a session cookie in $WORK/jar
	rm -f "$WORK/jar"
	web "$1" -s -o /dev/null -w '%{http_code}' -c jar -H 'Origin: http://controller:8080' \
		--data-urlencode username=restore --data-urlencode 'password=a long test password' http://controller:8080/login
}

step "secrets and the first database"
docker volume create "$SECRETS" >/dev/null
docker volume create "$DBSECRETS" >/dev/null
# the image runs as 65532; a fresh named volume is root's until the image's own directory seeds it
docker run --rm -v "$SECRETS":/secrets -v "$DBSECRETS":/dbsecrets "$IMAGE" init-secrets --dir /secrets --admin-dir /dbsecrets >/dev/null ||
	fail "init-secrets"
start_db "$ID-db1" "$NET1" "$ID-pg1"
ctl "$NET1" migrate up >/dev/null || fail "migrate on the first database"

step "a user, a login and a sealed secret"
printf 'a long test password\n' | ctl "$NET1" user create --username restore --role user >/dev/null || fail "user create"
start_ctl "$ID-ctl1" "$NET1"
[ "$(login "$NET1")" = 303 ] || fail "login on the first controller"
code=$(web "$NET1" -s -o /dev/null -D h -w '%{http_code}' -b jar -H 'Origin: http://controller:8080' -X POST http://controller:8080/account/2fa/start)
[ "$code" = 200 ] || { cat "$WORK/h" >&2; cat "$WORK/jar" >&2; fail "two-factor setup answered $code"; }
docker rm -f "$ID-ctl1" >/dev/null

step "dump"
docker exec "$ID-db1" pg_dump -U opendectcell --no-owner opendectcell >"$WORK/dump.sql"
[ -s "$WORK/dump.sql" ] || fail "empty dump"

step "restore into a second, empty database"
start_db "$ID-db2" "$NET2" "$ID-pg2"
# the dump mentions the runtime role in its GRANTs; the role does not exist yet: those lines fail and `migrate up` makes the role and the grants
docker exec -i "$ID-db2" psql -q -U opendectcell -d opendectcell -v ON_ERROR_STOP=0 <"$WORK/dump.sql" >/dev/null 2>"$WORK/restore.err" || true
ctl "$NET2" migrate up >/dev/null || fail "migrate up on the restored database"
users=$(docker exec "$ID-db2" psql -U opendectcell -d opendectcell -Atc "select count(*) from app_user where username = 'restore'")
[ "$users" = 1 ] || fail "the user is not in the restored database ($users)"

step "every sealed value opens under the master key"
out=$(ctl "$NET2" rotate-dek) || fail "rotate-dek on the restored database: $out"
echo "$out"
case $out in "re-encrypted 0 "*) fail "nothing was sealed, so nothing was proved" ;; "re-encrypted "*) ;; *) fail "unexpected: $out" ;; esac

step "a controller on the restored database lets the user in"
start_ctl "$ID-ctl2" "$NET2"
[ "$(login "$NET2")" = 303 ] || fail "login on the restored controller"

echo "OK: the dump and the master key restored a working controller"
