-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Fixes from the code review of 2026-11-03.

-- +up

-- Two accounts may not both have verified the same address: a reset or a verification link must find one.
-- (An address not yet verified may be claimed twice; only the first to follow its link keeps it.)
CREATE UNIQUE INDEX app_user_email_verified_uq ON app_user (lower(email)) WHERE email_verified AND email IS NOT NULL;

-- An administrator who disabled an account that had not followed its e-mail link yet: the link must not open it.
ALTER TABLE app_user ADD COLUMN admin_disabled boolean NOT NULL DEFAULT false;

-- Deleting a cell that enrolled with a token set cell_id to NULL and broke the CHECK that used_at and cell_id go together.
ALTER TABLE cell_enrollment_token DROP CONSTRAINT cell_enrollment_token_cell_id_fkey;
ALTER TABLE cell_enrollment_token ADD CONSTRAINT cell_enrollment_token_cell_id_fkey FOREIGN KEY (cell_id) REFERENCES cell ON DELETE CASCADE;

-- The audit log is append-only for the runtime role. Until now any session could `SET opendectcell.audit_purge = 'on'` and delete;
-- now the delete passes only when the trigger runs as the table's owner, which only audit_purge() (owned by the migrating
-- role, SECURITY DEFINER) does. The runtime role may call it (retention) and nothing else.
CREATE OR REPLACE FUNCTION audit_log_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('opendectcell.audit_purge', true) = 'on'
       AND current_user = (SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'audit_log'::regclass) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$;

CREATE FUNCTION audit_purge(before timestamptz) RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE n bigint;
BEGIN
    PERFORM set_config('opendectcell.audit_purge', 'on', true);
    DELETE FROM audit_log WHERE at < before;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;
REVOKE ALL ON FUNCTION audit_purge(timestamptz) FROM PUBLIC;

-- +down

DROP FUNCTION audit_purge(timestamptz);
ALTER TABLE cell_enrollment_token DROP CONSTRAINT cell_enrollment_token_cell_id_fkey;
ALTER TABLE cell_enrollment_token ADD CONSTRAINT cell_enrollment_token_cell_id_fkey FOREIGN KEY (cell_id) REFERENCES cell ON DELETE SET NULL;
ALTER TABLE app_user DROP COLUMN admin_disabled;
DROP INDEX app_user_email_verified_uq;
