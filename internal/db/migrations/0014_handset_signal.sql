-- SPDX-License-Identifier: AGPL-3.0-or-later
-- The last field strength a cell measured on a link to the handset (dBm, from the
-- chip's per-carrier level), and when. Shown on the handset's page.

-- +up

ALTER TABLE handset ADD COLUMN signal_dbm smallint, ADD COLUMN signal_at timestamptz;

-- +down

ALTER TABLE handset DROP COLUMN signal_dbm, DROP COLUMN signal_at;
