-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +up

-- The notification centre (ideas B4): what a user is told in the interface, and by mail when they asked for it.
ALTER TABLE app_user ADD COLUMN alert_prefs jsonb NOT NULL DEFAULT '{}';

CREATE TABLE notification (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    at          timestamptz NOT NULL DEFAULT now(),
    kind        text NOT NULL CHECK (length(kind) BETWEEN 1 AND 32),
    level       text NOT NULL CHECK (level IN ('ok', 'info', 'warning', 'danger')),
    subject     text NOT NULL CHECK (length(subject) <= 200),
    body        text NOT NULL DEFAULT '' CHECK (length(body) <= 2000),
    href        text NOT NULL DEFAULT '' CHECK (length(href) <= 200),
    read_at     timestamptz,
    mail_wanted boolean NOT NULL DEFAULT false,
    mailed_at   timestamptz
);
CREATE INDEX notification_user_idx ON notification (user_id, at DESC);
CREATE INDEX notification_unread_idx ON notification (user_id) WHERE read_at IS NULL;
CREATE INDEX notification_mail_idx ON notification (user_id) WHERE mail_wanted AND mailed_at IS NULL;
CREATE INDEX notification_at_idx ON notification (at);

-- A condition that is bad now and what was said about it, so that a restart does not say it again, and so that
-- "it is back" can be said when it is gone. info holds what is needed to say that (who, the words, the link).
CREATE TABLE alert_state (
    kind     text NOT NULL,
    object   text NOT NULL,
    since    timestamptz NOT NULL,
    fired_at timestamptz,
    info     jsonb NOT NULL DEFAULT '{}',
    PRIMARY KEY (kind, object)
);

-- +down

DROP TABLE alert_state;
DROP TABLE notification;
ALTER TABLE app_user DROP COLUMN alert_prefs;
