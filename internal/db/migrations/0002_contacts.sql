-- SPDX-License-Identifier: AGPL-3.0-or-later
-- CAT-iq list access, controller side (docs/list-access.md, #49):
-- contacts, and the shared read mark of missed calls.

-- +up

-- owner_user_id NULL means a shared, admin-managed contact.
CREATE TABLE contact (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_user_id  uuid REFERENCES app_user ON DELETE CASCADE,
    last_name      text NOT NULL DEFAULT '' CHECK (length(last_name) <= 64),
    first_name     text NOT NULL DEFAULT '' CHECK (length(first_name) <= 64),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (last_name <> '' OR first_name <> '')
);
CREATE INDEX contact_owner_idx ON contact (owner_user_id, lower(last_name), lower(first_name));

CREATE TABLE contact_number (
    contact_id  bigint NOT NULL REFERENCES contact ON DELETE CASCADE,
    position    smallint NOT NULL CHECK (position BETWEEN 0 AND 4),
    number      text NOT NULL CHECK (number ~ '^\+?[0-9*#]{1,32}$'),
    kind        text NOT NULL DEFAULT 'other' CHECK (kind IN ('fixed', 'mobile', 'work', 'other')),
    is_default  boolean NOT NULL DEFAULT false,
    PRIMARY KEY (contact_id, position)
);
CREATE UNIQUE INDEX contact_number_one_default ON contact_number (contact_id) WHERE is_default;
CREATE INDEX contact_number_number_idx ON contact_number (number);

-- One read mark per missed call, shared by every handset on the line.
ALTER TABLE call_log ADD COLUMN missed_read_at timestamptz;
CREATE INDEX call_log_account_idx ON call_log (sip_account_id, started_at DESC);

-- +down

DROP INDEX call_log_account_idx;
ALTER TABLE call_log DROP COLUMN missed_read_at;
DROP TABLE contact_number;
DROP TABLE contact;
