-- SPDX-License-Identifier: AGPL-3.0-or-later
-- CardDAV address books (ideas D5): a user links a book on their own server, it is copied into contact rows that
-- belong to the book, and each handset shows the books the user ticked for it, the way it has VoIP lines.

-- +up

CREATE TABLE addressbook (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    label         text NOT NULL CHECK (label <> '' AND length(label) <= 64),
    url           text NOT NULL CHECK (length(url) <= 512),
    username      text NOT NULL DEFAULT '' CHECK (length(username) <= 128),
    password_enc  bytea,
    enabled       boolean NOT NULL DEFAULT true,
    interval_min  integer NOT NULL DEFAULT 60 CHECK (interval_min BETWEEN 5 AND 1440),
    last_sync_at  timestamptz,
    last_ok_at    timestamptz,
    last_error    text NOT NULL DEFAULT '' CHECK (length(last_error) <= 300),
    last_note     text NOT NULL DEFAULT '' CHECK (length(last_note) <= 300),
    contact_count integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_user_id, label)
);

CREATE TABLE handset_addressbook (
    handset_id     uuid NOT NULL REFERENCES handset ON DELETE CASCADE,
    addressbook_id uuid NOT NULL REFERENCES addressbook ON DELETE CASCADE,
    PRIMARY KEY (handset_id, addressbook_id)
);

-- a contact that belongs to a book is the book's: the sync makes, changes and removes it
ALTER TABLE contact ADD COLUMN addressbook_id uuid REFERENCES addressbook ON DELETE CASCADE;
ALTER TABLE contact ADD COLUMN remote_href text;
ALTER TABLE contact ADD COLUMN remote_etag text;
CREATE UNIQUE INDEX contact_remote_idx ON contact (addressbook_id, remote_href) WHERE addressbook_id IS NOT NULL;

-- the last seven digits of a number without the zeros in front of it: what a call's number is looked up by to find
-- the contact it belongs to (ideas D5, step 2). "+49 30 1234567" and "030 1234567" end in the same seven.
ALTER TABLE contact_number ADD COLUMN match_tail text GENERATED ALWAYS AS
    (right(regexp_replace(regexp_replace(number, '[^0-9]', '', 'g'), '^0+', ''), 7)) STORED;
CREATE INDEX contact_number_tail_idx ON contact_number (match_tail);

-- +down

DROP INDEX contact_number_tail_idx;
ALTER TABLE contact_number DROP COLUMN match_tail;
DROP INDEX contact_remote_idx;
ALTER TABLE contact DROP COLUMN remote_etag;
ALTER TABLE contact DROP COLUMN remote_href;
ALTER TABLE contact DROP COLUMN addressbook_id;
DROP TABLE handset_addressbook;
DROP TABLE addressbook;
