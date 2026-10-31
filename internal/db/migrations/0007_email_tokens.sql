-- SPDX-License-Identifier: AGPL-3.0-or-later
-- E-mail verification for self-registration and password reset by e-mail
-- (maintainer, 2026-11-01: verification is mandatory once SMTP is configured).
-- Existing users count as verified.

-- +up

ALTER TABLE app_user ADD COLUMN email_verified boolean NOT NULL DEFAULT true;

CREATE TABLE email_token (
    token_hash  bytea PRIMARY KEY CHECK (length(token_hash) = 32),
    user_id     uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    purpose     text NOT NULL CHECK (purpose IN ('verify', 'reset')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz
);
CREATE INDEX email_token_user_idx ON email_token (user_id);

-- +down

DROP TABLE email_token;
ALTER TABLE app_user DROP COLUMN email_verified;
