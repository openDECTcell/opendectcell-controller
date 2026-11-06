-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +up

-- Legal texts an administrator switched on (Legal notice, privacy notice, emergency-call warning, experimental-software
-- notice; the texts themselves are in the setting "legal") and which version of each a user signed off. A user who has not
-- signed off on every enabled text at its current version has no access to the web interface or the API (internal/legal).
CREATE TABLE legal_acceptance (
    user_id     uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    doc         text NOT NULL CHECK (doc IN ('imprint', 'privacy', 'emergency', 'experimental')),
    version     integer NOT NULL CHECK (version > 0),
    accepted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, doc)
);

-- +down

DROP TABLE legal_acceptance;
