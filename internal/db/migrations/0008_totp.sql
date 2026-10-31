-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Two-factor login with a time-based one-time password (RFC 6238) and
-- recovery codes. The secret is encrypted under the data key.

-- +up

CREATE TABLE user_totp (
    user_id       uuid PRIMARY KEY REFERENCES app_user ON DELETE CASCADE,
    secret_enc    bytea NOT NULL,
    confirmed_at  timestamptz,
    last_step     bigint NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_recovery_code (
    user_id    uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    code_hash  bytea NOT NULL CHECK (length(code_hash) = 32),
    used_at    timestamptz,
    PRIMARY KEY (user_id, code_hash)
);

-- Between the password and the second factor: valid for minutes, bound to a user.
CREATE TABLE mfa_challenge (
    token_hash  bytea PRIMARY KEY CHECK (length(token_hash) = 32),
    user_id     uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    return_to   text NOT NULL DEFAULT '/',
    expires_at  timestamptz NOT NULL
);

-- +down

DROP TABLE mfa_challenge;
DROP TABLE user_recovery_code;
DROP TABLE user_totp;
