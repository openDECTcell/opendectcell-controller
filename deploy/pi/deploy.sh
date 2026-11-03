#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Puts the working tree on the test ground and (re)builds and starts the stack there (docs/test-ground.md).
# usage: deploy/pi/deploy.sh [user@host]       default: pi@opendectcell-pi.fritz.box
# The image is built on the Pi (arm64, with libopendectcell), so the submodule must be checked out here:
#   git submodule update --init
set -eu
HOST=${1:-pi@opendectcell-pi.fritz.box}
DIR=opendectcell-controller
cd "$(dirname "$0")/../.."
[ -f third_party/opendectcell/src/libopendectcell/dect_base.c ] || { echo "third_party/opendectcell is not checked out: git submodule update --init" >&2; exit 1; }

rsync -a --delete --exclude .git --exclude .env --exclude docker-compose.override.yml \
  --exclude 'deploy/pi/.env' --exclude '*.log' ./ "$HOST:$DIR/"
ssh "$HOST" "cd $DIR && [ -f deploy/pi/.env ] || { cp deploy/pi/env.example deploy/pi/.env; echo 'created deploy/pi/.env from the example'; }
  C='sudo docker compose --env-file deploy/pi/.env -f docker-compose.yml -f deploy/pi/docker-compose.pi.yml'
  \$C build --build-arg VERSION=\$(date +%Y%m%d)-pi
  \$C up -d --remove-orphans
  \$C ps"
