#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Browser smoke test: a throwaway controller and database (tools/browser-smoke/up.sh), then headless Chrome through the
# two-factor setup, every page an administrator and a user can reach, disclosure, and account erasure. Needs docker, node,
# Chrome (CHROME=/path), and the image (docker build -t opendectcell-controller:local .). Exit status 0 only if every step held.
# It prints PASS and FAIL lines and every console error or CSP violation of the pages.
set -eu
cd "$(dirname "$0")"
[ -d node_modules ] || npm install --no-audit --no-fund >/dev/null
./up.sh "${1:-opendectcell-controller:local}" >/dev/null
trap 'docker rm -f clicktest-db clicktest-ctl >/dev/null 2>&1; docker volume rm -f clicktest-secrets clicktest-dbsecrets clicktest-pg >/dev/null 2>&1; docker network rm clicktest >/dev/null 2>&1; rm -f admin.secret admin.recovery' EXIT
node flow.js
docker exec -i clicktest-db psql -q -U opendectcell -d opendectcell <seed.sql
node crawl.js admin out/admin 0 | grep -E '^---|^[^2]0[0-9] |^[45]' || true
node crawl.js anna out/anna | grep -E '^---|^[45]' || true
node interact.js | tee out/interact.log
if grep -q '^FAIL' out/interact.log; then exit 1; fi
