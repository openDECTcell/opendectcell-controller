-- SPDX-License-Identifier: AGPL-3.0-or-later
-- A cell's call limit is what the cell announces (its voice channels), and the controller never grants
-- unencrypted RTP, so neither setting is kept per cell any more. Nothing read either column.

-- +up

ALTER TABLE cell DROP COLUMN max_calls, DROP COLUMN allow_plain_rtp;

-- +down

ALTER TABLE cell ADD COLUMN max_calls smallint NOT NULL DEFAULT 1 CHECK (max_calls >= 0),
                 ADD COLUMN allow_plain_rtp boolean NOT NULL DEFAULT false;
