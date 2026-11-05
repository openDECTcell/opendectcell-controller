#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# The backup service of an openDECTcell controller (docs/wiki/Operating.md, "Backups").
#
#   opendectcell-backup            run for ever: one backup a day at BACKUP_AT
#   opendectcell-backup once       make one backup now, then exit (exit status 0 only if it, and its check, held)
#   opendectcell-backup decrypt F  write the plain dump of the encrypted backup F to standard output
#   opendectcell-backup check F    restore the backup F into a scratch database and say whether it holds
#
# One backup is: pg_dump -Fc of the controller's database, encrypted with a passphrase (gpg, AES-256, authenticated), written to
# BACKUP_DIR; checked by restoring it into a scratch PostgreSQL that lives for a minute inside this container; copied to an SFTP
# server if BACKUP_SFTP_TARGET is set; the oldest ones beyond BACKUP_KEEP removed (here and there). Every run ends by writing
# BACKUP_DIR/status.json, which the controller reads to show the state and to tell the administrators when it is wrong.
#
# The master key of the controller is NOT in the dump and not here: keep it apart (a dump with its key opens everything).
set -u

DB_HOST=${BACKUP_DB_HOST:-db}
DB_NAME=${BACKUP_DB_NAME:-opendectcell}
DB_USER=${BACKUP_DB_USER:-opendectcell}
DB_PASSWORD_FILE=${BACKUP_DB_PASSWORD_FILE:-/dbsecrets/db_password}
DIR=${BACKUP_DIR:-/backups}
AT=${BACKUP_AT:-03:15}
KEEP=${BACKUP_KEEP:-14}
PASS_FILE=${BACKUP_PASSPHRASE_FILE:-/backup-secrets/passphrase}
CHECK=${BACKUP_CHECK:-1}
SFTP_TARGET=${BACKUP_SFTP_TARGET:-}
SFTP_PORT=${BACKUP_SFTP_PORT:-22}
SFTP_KEY=${BACKUP_SFTP_KEY_FILE:-/backup-secrets/sftp_key}
SFTP_HOSTS=${BACKUP_SFTP_KNOWN_HOSTS_FILE:-/backup-secrets/sftp_known_hosts}
STATUS=$DIR/status.json

log() { echo "backup: $*" >&2; }

# json_str: a string as JSON (backslash, quote and control characters escaped; at most 300 characters)
json_str() {
	printf '%s' "$1" | head -c 300 | tr '\n\r\t' '   ' | sed 's/\\/\\\\/g; s/"/\\"/g' | tr -d '\000-\037' | { printf '"'; cat; printf '"'; }
}

# the first start gives the service its own root helpers: it runs as root only to own its volumes, then as the postgres user
if [ "$(id -u)" = 0 ] && [ "${1:-}" != decrypt ]; then # (decrypt only reads: it needs no volume of its own)
	mkdir -p "$DIR" "$(dirname "$PASS_FILE")"
	chown postgres:postgres "$DIR" "$(dirname "$PASS_FILE")"
	chmod 700 "$(dirname "$PASS_FILE")"
	exec gosu postgres "$0" "$@"
fi

# gpg keeps a small state in a directory of its own: on the container's temporary space, which is the one place that is writable
GNUPGHOME=${GNUPGHOME:-/tmp/gnupg}
export GNUPGHOME
mkdir -p "$GNUPGHOME" && chmod 700 "$GNUPGHOME"

case "${KEEP}" in '' | *[!0-9]*) log "BACKUP_KEEP must be a number"; exit 2 ;; esac
[ "$KEEP" -ge 1 ] || { log "BACKUP_KEEP must be at least 1"; exit 2; }

passphrase() {
	if [ ! -s "$PASS_FILE" ]; then
		umask 077
		# 32 random characters; shown once in the log so that it can be copied away, and never again
		head -c 48 /dev/urandom | base64 | tr -d '/+=\n' | head -c 32 > "$PASS_FILE"
		log "made a new passphrase in $PASS_FILE: copy it somewhere safe and apart (without it no backup can be read):"
		log "    $(cat "$PASS_FILE")"
	fi
}

decrypt() { gpg --batch --quiet --pinentry-mode loopback --passphrase-file "$PASS_FILE" --decrypt "$1"; }

if [ "${1:-}" = decrypt ]; then
	[ -n "${2:-}" ] || { log "usage: opendectcell-backup decrypt FILE"; exit 2; }
	decrypt "$2"
	exit $?
fi

export PGHOST="$DB_HOST" PGUSER="$DB_USER" PGDATABASE="$DB_NAME" PGCONNECT_TIMEOUT=10
PGPASSWORD=$(cat "$DB_PASSWORD_FILE" 2>/dev/null || true)
export PGPASSWORD

# the SFTP calls: a key, the server's key pinned from a file the administrator gave (no trust on first use), no prompts
sftp_cmd() {
	sftp -q -b - -P "$SFTP_PORT" -i "$SFTP_KEY" -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$SFTP_HOSTS" \
		-o ConnectTimeout=20 -o IdentitiesOnly=yes "${SFTP_TARGET%%:*}"
}
sftp_dir() { printf '%s' "${SFTP_TARGET#*:}"; }

# the restore check: the backup is read back and loaded into a PostgreSQL made for this purpose, never into the live one
restore_check() { # restore_check FILE -> sets CHECK_OK, CHECK_USERS, CHECK_TABLES, CHECK_ERR
	CHECK_OK=false CHECK_USERS=0 CHECK_TABLES=0 CHECK_ERR=""
	work=$(mktemp -d /tmp/check.XXXXXX) || { CHECK_ERR="no scratch space"; return 1; }
	sock=$work/sock; mkdir -p "$sock"
	if ! decrypt "$1" > "$work/plain.dump" 2> "$work/err"; then CHECK_ERR="the file does not decrypt"; rm -rf "$work"; return 1; fi
	if ! initdb -D "$work/data" -U check --auth=trust > "$work/initdb.log" 2>&1; then CHECK_ERR="no scratch database"; rm -rf "$work"; return 1; fi
	if ! pg_ctl -D "$work/data" -o "-c listen_addresses= -c unix_socket_directories=$sock -c fsync=off" -l "$work/pg.log" -w start > /dev/null 2>&1; then
		CHECK_ERR="the scratch database did not start"; rm -rf "$work"; return 1
	fi
	export_h=$PGHOST
	if createdb -h "$sock" -U check chk > /dev/null 2>&1 &&
		pg_restore -h "$sock" -U check -d chk --no-owner --no-acl --exit-on-error "$work/plain.dump" > "$work/restore.log" 2>&1; then
		CHECK_USERS=$(psql -h "$sock" -U check -d chk -Atc "SELECT count(*) FROM app_user" 2>/dev/null || echo 0)
		CHECK_TABLES=$(psql -h "$sock" -U check -d chk -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'" 2>/dev/null || echo 0)
		live_tables=$(psql -h "$export_h" -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'" 2>/dev/null || echo -1)
		live_users=$(psql -h "$export_h" -Atc "SELECT count(*) FROM app_user" 2>/dev/null || echo 0)
		if [ "$CHECK_TABLES" != "$live_tables" ]; then
			CHECK_ERR="the restored database has $CHECK_TABLES tables, the live one $live_tables"
		elif [ "$live_users" -gt 0 ] && [ "$CHECK_USERS" -lt 1 ]; then
			CHECK_ERR="the restored database has no users"
		else
			CHECK_OK=true
		fi
	else
		CHECK_ERR="the restore failed: $(tail -n 1 "$work/restore.log" 2>/dev/null)"
	fi
	pg_ctl -D "$work/data" -m immediate stop > /dev/null 2>&1
	rm -rf "$work"
	[ "$CHECK_OK" = true ]
}

# prune: keep the KEEP newest files of the names we make, here
prune_local() {
	ls -1 "$DIR"/opendectcell-*.dump.gpg 2> /dev/null | sort -r | tail -n +"$((KEEP + 1))" | while read -r f; do
		log "removing the old backup $(basename "$f")"
		rm -f -- "$f"
	done
}

# prune_remote: the same on the SFTP server
prune_remote() {
	d=$(sftp_dir)
	names=$(printf 'ls -1 %s\n' "$d" | sftp_cmd 2> /dev/null | sed 's|.*/||' | grep '^opendectcell-[0-9]*-[0-9]*\.dump\.gpg$' | sort -r | tail -n +"$((KEEP + 1))")
	[ -n "$names" ] || return 0
	for n in $names; do
		log "removing the old backup $n from the SFTP server"
		printf 'rm %s/%s\n' "$d" "$n" | sftp_cmd > /dev/null 2>&1
	done
}

write_status() { # write_status ok file size sha started finished error sftp_ok sftp_err
	next=$(next_run | tr -d '\n')
	{
		printf '{"version":1,"ok":%s,"started":%s,"finished":%s,"file":%s,"size":%s,"sha256":%s,"error":%s,' \
			"$1" "$(json_str "$5")" "$(json_str "$6")" "$(json_str "$2")" "${3:-0}" "$(json_str "$4")" "$(json_str "$7")"
		printf '"check":{"ran":%s,"ok":%s,"users":%s,"tables":%s,"error":%s},' "$([ "$CHECK" = 1 ] && echo true || echo false)" "${CHECK_OK:-false}" "${CHECK_USERS:-0}" "${CHECK_TABLES:-0}" "$(json_str "${CHECK_ERR:-}")"
		printf '"sftp":{"configured":%s,"ok":%s,"error":%s},"keep":%s,"next":%s,"at":%s}\n' "$([ -n "$SFTP_TARGET" ] && echo true || echo false)" "$8" "$(json_str "$9")" "$KEEP" "$(json_str "$next")" "$(json_str "$(date -u +%Y-%m-%dT%H:%M:%SZ)")"
	} > "$STATUS.tmp" && mv "$STATUS.tmp" "$STATUS"
}

backup_once() {
	started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	CHECK_OK=false CHECK_USERS=0 CHECK_TABLES=0 CHECK_ERR=""
	passphrase
	name=opendectcell-$(date -u +%Y%m%d-%H%M%S).dump.gpg
	file=$DIR/$name
	log "dumping $DB_NAME on $DB_HOST to $name"
	err=$(mktemp) rc=$(mktemp)
	# the status of pg_dump is what counts, not that of the pipe: it is written to a file
	{ pg_dump -Fc "$DB_NAME" 2> "$err"; echo $? > "$rc"; } |
		gpg --batch --quiet --pinentry-mode loopback --passphrase-file "$PASS_FILE" --symmetric --cipher-algo AES256 --compress-algo none -o "$file.part"
	gpg_rc=$?
	if [ "$(cat "$rc")" != 0 ] || [ "$gpg_rc" != 0 ] || [ ! -s "$file.part" ]; then
		e="the dump failed: $(grep -i 'error\|fatal' "$err" | head -n 1)"
		[ "$gpg_rc" = 0 ] || e="the encryption failed"
		rm -f "$file.part" "$err" "$rc"
		log "$e"
		write_status false "" 0 "" "$started" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$e" false ""
		return 1
	fi
	rm -f "$rc"
	rm -f "$err"
	mv "$file.part" "$file"
	size=$(wc -c < "$file")
	sha=$(sha256sum "$file" | cut -d' ' -f1)
	ok=true error=""
	if [ "$CHECK" = 1 ]; then
		log "checking that it restores"
		if ! restore_check "$file"; then ok=false; error="the restore check failed: $CHECK_ERR"; log "$error"; fi
	fi
	sftp_ok=false sftp_err=""
	if [ -n "$SFTP_TARGET" ]; then
		if [ ! -s "$SFTP_KEY" ] || [ ! -s "$SFTP_HOSTS" ]; then
			sftp_err="the SFTP key or the known-hosts file is missing"
		elif printf 'put %s %s/%s.part\nrename %s/%s.part %s/%s\n' "$file" "$(sftp_dir)" "$name" "$(sftp_dir)" "$name" "$(sftp_dir)" "$name" | sftp_cmd > /dev/null 2> "$DIR/.sftp.err"; then
			sftp_ok=true
			prune_remote
		else
			sftp_err="the upload failed: $(grep -v '^$' "$DIR/.sftp.err" | tail -n 1)"
		fi
		rm -f "$DIR/.sftp.err"
		[ "$sftp_ok" = true ] || { ok=false; error="${error:+$error; }$sftp_err"; log "$sftp_err"; }
	fi
	prune_local
	write_status "$ok" "$name" "$size" "$sha" "$started" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$error" "$sftp_ok" "$sftp_err"
	[ "$ok" = true ] && log "done: $name ($size bytes)"
	[ "$ok" = true ]
}

# next_run: when the daily run comes next, in UTC
next_run() {
	secs=$(seconds_until)
	date -u -d "@$(($(date +%s) + secs))" +%Y-%m-%dT%H:%M:%SZ
}

seconds_until() {
	now=$(date +%s)
	t=$(date -d "today $AT" +%s 2> /dev/null) || { echo 86400; return; }
	[ "$t" -gt "$now" ] || t=$(date -d "tomorrow $AT" +%s)
	echo $((t - now))
}

case "$AT" in [0-2][0-9]:[0-5][0-9]) ;; *) log "BACKUP_AT must be HH:MM"; exit 2 ;; esac

if [ "${1:-}" = check ]; then
	[ -n "${2:-}" ] || { log "usage: opendectcell-backup check FILE"; exit 2; }
	if restore_check "$2"; then
		echo "backup: $2 restores: $CHECK_USERS users, $CHECK_TABLES tables"
		exit 0
	fi
	echo "backup: $2 does not hold: $CHECK_ERR" >&2
	exit 1
fi

if [ "${1:-}" = once ]; then
	backup_once
	exit $?
fi

trap 'exit 0' TERM INT
log "a backup every day at $AT (the zone of this container), the $KEEP newest kept${SFTP_TARGET:+, copied to $SFTP_TARGET}"
passphrase
[ -s "$STATUS" ] || write_status false "" 0 "" "" "" "no backup has run yet" false ""
while :; do
	s=$(seconds_until)
	sleep "$s" &
	wait $!
	backup_once || true
done
