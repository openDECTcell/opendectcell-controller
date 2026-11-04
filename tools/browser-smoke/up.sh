#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Throwaway controller for a browser click-through: http://127.0.0.1:18080, users admin / anna / ben, password "a long test password"
set -eu
IMAGE=${1:-opendectcell-controller:local}
PG=postgres:18
ID=clicktest
docker rm -f $ID-db $ID-ctl >/dev/null 2>&1 || true
docker volume rm -f $ID-secrets $ID-dbsecrets $ID-pg >/dev/null 2>&1 || true
docker network rm $ID >/dev/null 2>&1 || true
docker network create $ID >/dev/null
docker volume create $ID-secrets >/dev/null; docker volume create $ID-dbsecrets >/dev/null
docker run --rm -v $ID-secrets:/secrets -v $ID-dbsecrets:/dbsecrets $IMAGE init-secrets --dir /secrets --admin-dir /dbsecrets >/dev/null
docker run -d --name $ID-db --network $ID --network-alias db -e POSTGRES_DB=opendectcell -e POSTGRES_USER=opendectcell -e POSTGRES_PASSWORD_FILE=/dbsecrets/db_password -v $ID-dbsecrets:/dbsecrets:ro -v $ID-pg:/var/lib/postgresql $PG >/dev/null
sleep 6
until docker exec $ID-db pg_isready -U opendectcell -d opendectcell >/dev/null 2>&1; do sleep 1; done
sleep 2
ctl() { docker run --rm -i --network $ID -e OPENDECTCELL_DATABASE_URL_FILE=/secrets/database_url -e OPENDECTCELL_DATABASE_ADMIN_URL_FILE=/dbsecrets/database_admin_url -e OPENDECTCELL_MASTER_KEY_FILE=/secrets/master_key -e OPENDECTCELL_HTTP_BASE_URL=http://127.0.0.1:18080 -e OPENDECTCELL_CELLS_REQUIRE_DECT_CORE=false -v $ID-secrets:/secrets:ro -v $ID-dbsecrets:/dbsecrets:ro $IMAGE "$@"; }
ctl migrate up >/dev/null
for u in "admin admin" "anna user" "ben user"; do set -- $u; printf 'a long test password\n' | ctl user create --username $1 --role $2 >/dev/null; done
docker run -d --name $ID-ctl --network $ID -p 127.0.0.1:18080:8080 -e OPENDECTCELL_DATABASE_URL_FILE=/secrets/database_url -e OPENDECTCELL_MASTER_KEY_FILE=/secrets/master_key -e OPENDECTCELL_HTTP_BASE_URL=http://127.0.0.1:18080 -e OPENDECTCELL_CELLS_REQUIRE_DECT_CORE=false -v $ID-secrets:/secrets:ro $IMAGE serve --no-migrate >/dev/null
i=0; until curl -sf http://127.0.0.1:18080/readyz >/dev/null 2>&1; do i=$((i+1)); [ $i -lt 60 ] || { docker logs $ID-ctl | tail; exit 1; }; sleep 1; done
echo up
