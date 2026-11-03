#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Keeps the AAAA record of the test ground's name pointing at this host's IPv6 address (ipv64.net DynDNS), and the firewall's
# set of LAN prefixes in step with the prefix the provider delegates.
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

# The firewall's idea of "the LAN" for IPv6 follows the /64 prefixes the router advertises: the provider changes its
# delegated prefix now and then, and a prefix written into the ruleset would then lock the administrator out. One
# atomic replacement, and only when a prefix is missing from the set.
prefixes=$(ip -6 route show dev "$IFACE" proto ra 2>/dev/null | awk '$1 ~ /\/64$/ && $1 !~ /^fe80/ { print $1 }' | sort -u | paste -sd, -)
if [ -n "$prefixes" ] && have=$(nft list set inet opendectcell_filter lan6 2>/dev/null); then
  missing=0
  for p in $(printf '%s' "$prefixes" | tr ',' ' '); do
    printf '%s' "$have" | grep -q "$p" || missing=1
  done
  if [ "$missing" = 1 ]; then
    printf 'flush set inet opendectcell_filter lan6\nadd element inet opendectcell_filter lan6 { %s }\n' "$prefixes" | nft -f - &&
      logger -t opendectcell-ddns "firewall: the LAN's IPv6 prefixes are now $prefixes"
  fi
fi

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
