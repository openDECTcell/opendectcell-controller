-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +up

-- Personal API tokens (ideas F1): a script or a widget acts as the user with the scopes the user gave it, until it expires or is
-- revoked. Only a hash of the secret is kept; the token is shown once, when it is made.
CREATE TABLE api_token (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    name         text NOT NULL CHECK (name <> '' AND length(name) <= 64),
    -- what the holder recognises it by: "odc_" and the 16 hexadecimal characters of the lookup key; not a secret
    lookup       text NOT NULL UNIQUE CHECK (lookup ~ '^[0-9a-f]{16}$'),
    secret_hash  bytea NOT NULL CHECK (length(secret_hash) = 32),
    scopes       text[] NOT NULL CHECK (cardinality(scopes) BETWEEN 1 AND 32),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_used_at timestamptz,
    last_used_ip text,
    revoked_at   timestamptz
);
CREATE INDEX api_token_user_idx ON api_token (user_id, created_at DESC);

-- +down

DROP TABLE api_token;
