-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +up

-- Webhooks (ideas F2): a user has the controller post events (a call comes in, is missed, ends; a handset goes online or offline; a
-- notification) to an address of their own, signed with a secret, retried when the receiver fails. The secret and the optional
-- Authorization header value are sealed with the keyring (internal/db/keys.go).
CREATE TABLE webhook (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    name            text NOT NULL CHECK (name <> '' AND length(name) <= 64),
    url             text NOT NULL CHECK (length(url) <= 500),
    format          text NOT NULL DEFAULT 'json' CHECK (format IN ('json', 'ntfy')),
    events          text[] NOT NULL CHECK (cardinality(events) BETWEEN 1 AND 16),
    enabled         boolean NOT NULL DEFAULT true,
    secret_enc      bytea NOT NULL,
    auth_enc        bytea,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- deliveries in a row that ended as failed; at the limit the webhook is switched off, with the reason
    failures        integer NOT NULL DEFAULT 0,
    disabled_reason text NOT NULL DEFAULT '',
    last_at         timestamptz,
    last_status     text NOT NULL DEFAULT ''
);
CREATE INDEX webhook_user_idx ON webhook (user_id);

CREATE TABLE webhook_delivery (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    webhook_id uuid NOT NULL REFERENCES webhook ON DELETE CASCADE,
    uid        uuid NOT NULL DEFAULT gen_random_uuid(),
    event      text NOT NULL,
    payload    jsonb NOT NULL,
    state      text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done', 'failed')),
    attempts   integer NOT NULL DEFAULT 0,
    next_at    timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    done_at    timestamptz,
    last_error text NOT NULL DEFAULT '' CHECK (length(last_error) <= 300),
    last_code  integer
);
CREATE INDEX webhook_delivery_due_idx ON webhook_delivery (next_at) WHERE state = 'pending';
CREATE INDEX webhook_delivery_hook_idx ON webhook_delivery (webhook_id, id DESC);
CREATE INDEX webhook_delivery_done_idx ON webhook_delivery (done_at) WHERE state <> 'pending';

-- +down

DROP TABLE webhook_delivery;
DROP TABLE webhook;
