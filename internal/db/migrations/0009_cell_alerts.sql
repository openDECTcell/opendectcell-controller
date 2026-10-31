-- SPDX-License-Identifier: AGPL-3.0-or-later
-- E-mail alerts when a cell goes offline or comes back, opt-in per user (admins).

-- +up

ALTER TABLE app_user ADD COLUMN notify_cell_alerts boolean NOT NULL DEFAULT false;

-- +down

ALTER TABLE app_user DROP COLUMN notify_cell_alerts;
