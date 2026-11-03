-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Two-factor login asks again on a device it has not seen, and on a known one every three weeks. A browser that
-- passed the second step holds a random token in a cookie; only its hash is kept here.
-- A user may also put off being asked to set two-factor login up (never forced, except for administrators).

-- +up

CREATE TABLE trusted_device (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    token_hash   bytea NOT NULL CHECK (length(token_hash) = 32),
    label        text NOT NULL DEFAULT '' CHECK (length(label) <= 200),
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- the last time a code was entered on this device
    last_2fa_at  timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, token_hash)
);

ALTER TABLE app_user ADD COLUMN twofa_prompt_dismissed_at timestamptz;

-- +down

ALTER TABLE app_user DROP COLUMN twofa_prompt_dismissed_at;
DROP TABLE trusted_device;
