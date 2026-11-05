# SPDX-License-Identifier: AGPL-3.0-or-later
# The backup service: pg_dump and a scratch PostgreSQL (from the same image as the database, so the versions match), gpg for the
# encryption, an SFTP client for the copy. See backup.sh.
FROM postgres:18@sha256:979bccfde35445db618c65107d27d08c6c21339d8dc3ad4c830c6633947be0e4
RUN apt-get update \
 && apt-get install -y --no-install-recommends gnupg openssh-client \
 && rm -rf /var/lib/apt/lists/*
COPY backup.sh /usr/local/bin/opendectcell-backup
ENTRYPOINT ["/usr/local/bin/opendectcell-backup"]
