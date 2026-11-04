-- SPDX-License-Identifier: AGPL-3.0-or-later
-- A short description of the browser a session was started in ("Firefox on Linux"), so a user can tell their sessions
-- apart on the account page (UI backlog B6). No address and no full user-agent string is kept.

-- +up

ALTER TABLE session ADD COLUMN device text NOT NULL DEFAULT '' CHECK (length(device) <= 64);

-- +down

ALTER TABLE session DROP COLUMN device;
