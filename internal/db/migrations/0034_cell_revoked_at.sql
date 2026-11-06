-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +up

-- A revoked base station stays in "All base stations" for three days and is then deleted (cell.Store.PurgeRevoked). The day it was
-- revoked is kept here; cells revoked before this column existed count from the day of the upgrade.
ALTER TABLE cell ADD COLUMN revoked_at timestamptz;
UPDATE cell SET revoked_at = now() WHERE state = 'revoked';

-- +down

ALTER TABLE cell DROP COLUMN revoked_at;
