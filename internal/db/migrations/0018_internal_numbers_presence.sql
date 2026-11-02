-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Internal numbers and presence (docs/internal-calls.md, the maintainers' notes 2026-11-02).
--
-- Every handset has an internal number, unique among its owner's handsets: what a
-- handset dials after INT to reach another handset of the same account. 200..999,
-- the lowest free one; a handset beyond the 800th of one account has none (NULL)
-- and cannot be called internally. A trigger gives the number, so every way a
-- handset gets into the table gets one.
--
-- detached_at and page_failed_at are what the DECT core saw of a handset besides
-- its contacts (last_seen_at): it sent {DETACH} (switched off), or nobody
-- answered a page for it. The presence shown (online, unresponsive, offline) is
-- worked out from these three times and the roaming interval; nothing is stored
-- as "state".

-- +up

ALTER TABLE handset
    ADD COLUMN internal_number smallint CHECK (internal_number BETWEEN 200 AND 999),
    ADD COLUMN detached_at     timestamptz,
    ADD COLUMN page_failed_at  timestamptz;

UPDATE handset h SET internal_number = x.n
  FROM (SELECT id, (199 + row_number() OVER (PARTITION BY owner_user_id ORDER BY registered_at, id))::int AS n FROM handset) x
 WHERE h.id = x.id AND x.n <= 999;

CREATE UNIQUE INDEX handset_internal_number_key ON handset (owner_user_id, internal_number);

CREATE FUNCTION handset_assign_internal_number() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.internal_number IS NULL
       OR (TG_OP = 'UPDATE' AND NEW.owner_user_id <> OLD.owner_user_id) THEN
        -- one owner's numbers are given one at a time
        PERFORM pg_advisory_xact_lock(hashtextextended(NEW.owner_user_id::text, 18));
        SELECT n INTO NEW.internal_number
          FROM generate_series(200, 999) n
         WHERE NOT EXISTS (SELECT 1 FROM handset h
                            WHERE h.owner_user_id = NEW.owner_user_id AND h.internal_number = n AND h.id <> NEW.id)
         ORDER BY n LIMIT 1;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER handset_internal_number BEFORE INSERT OR UPDATE OF owner_user_id ON handset
    FOR EACH ROW EXECUTE FUNCTION handset_assign_internal_number();

-- +down

DROP TRIGGER handset_internal_number ON handset;
DROP FUNCTION handset_assign_internal_number();
DROP INDEX handset_internal_number_key;
ALTER TABLE handset DROP COLUMN page_failed_at, DROP COLUMN detached_at, DROP COLUMN internal_number;
