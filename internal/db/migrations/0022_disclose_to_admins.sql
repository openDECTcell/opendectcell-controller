-- SPDX-License-Identifier: AGPL-3.0-or-later
-- A user may disclose a handset or a base station to the administrators, one entry at a time. An administrator then
-- sees it under that user in Administration > Users. Off for everything: handsets are private to their owner.

-- +up

ALTER TABLE handset ADD COLUMN disclosed_to_admins boolean NOT NULL DEFAULT false;
ALTER TABLE cell ADD COLUMN disclosed_to_admins boolean NOT NULL DEFAULT false;

-- +down

ALTER TABLE cell DROP COLUMN disclosed_to_admins;
ALTER TABLE handset DROP COLUMN disclosed_to_admins;
