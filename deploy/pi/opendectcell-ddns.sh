#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Keeps the AAAA record of the test ground's name pointing at this host's IPv6 address (ipv64.net DynDNS).
# Run by opendectcell-ddns.timer every five minutes. It asks the provider only when the address changed (the account is limited
# to 48 updates a day), never sets an A record (the line has no public IPv4), and keeps the key off the command line.
#
# /etc/opendectcell/ddns.env (root, 0600):  DDNS_KEY=<update token>  DDNS_DOMAIN=<name>  [DDNS_IFACE=eth0]
set -eu
. /etc/opendectcell/ddns.env
: "${DDNS_KEY:?}" "${DDNS_DOMAIN:?}"
IFACE=${DDNS_IFACE:-eth0}
STATE=/var/lib/opendectcell-ddns
mkdir -p "$STATE"

# The stable global address: not temporary (privacy), not deprecated, in 2000::/3 (so not the ULA fd00::/8).
addr=$(ip -6 -o addr show dev "$IFACE" scope global 2>/dev/null |
  awk '!/temporary/ && !/deprecated/ { split($4, a, "/"); if (a[1] ~ /^[23]/) { print a[1]; exit } }')
if [ -z "$addr" ]; then
  logger -t opendectcell-ddns "no global IPv6 address on $IFACE: nothing to update"
  exit 0
fi

last=$(cat "$STATE/last" 2>/dev/null || true)
now=$(date +%s)
when=$(cat "$STATE/when" 2>/dev/null || echo 0)
# Ask the provider when the address changed since the last good answer, or when the name does not resolve to it
# (at most every 30 minutes, so a slow resolver cannot make this loop).
if [ "$addr" = "$last" ]; then
  seen=$(getent ahostsv6 "$DDNS_DOMAIN" 2>/dev/null | awk '{ print $1; exit }' || true)
  if [ "$seen" = "$addr" ] || [ $((now - when)) -lt 1800 ]; then
    exit 0
  fi
fi

answer=$(curl -fsS --max-time 20 -G --config - <<EOT
url = "https://ipv64.net/nic/update"
data-urlencode = "key=$DDNS_KEY"
data-urlencode = "domain=$DDNS_DOMAIN"
data-urlencode = "ip6=$addr"
data-urlencode = "output=min"
EOT
) || { logger -t opendectcell-ddns "update request failed"; exit 1; }

case "$answer" in
  *good*|*nochg*)
    printf '%s\n' "$addr" > "$STATE/last"
    printf '%s\n' "$now" > "$STATE/when"
    logger -t opendectcell-ddns "$DDNS_DOMAIN -> $addr ($answer)"
    ;;
  *)
    logger -t opendectcell-ddns "the provider did not accept the update: $answer"
    exit 1
    ;;
esac
