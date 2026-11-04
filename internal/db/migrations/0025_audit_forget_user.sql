-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Erasing a user (audit finding PRIV-05): the audit log keeps what happened, but not who: entries the user made lose their
-- name, address and id, and entries about the user's own account lose the user name. Like the purge, it runs as the table's
-- owner through a function; the runtime role cannot update the log in any other way.

-- +up

CREATE OR REPLACE FUNCTION audit_log_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'UPDATE') AND current_setting('opendectcell.audit_purge', true) = 'on'
       AND current_user = (SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'audit_log'::regclass) THEN
        RETURN CASE TG_OP WHEN 'DELETE' THEN OLD ELSE NEW END;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$;

CREATE FUNCTION audit_forget_user(uid uuid) RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE n bigint; m bigint;
BEGIN
    PERFORM set_config('opendectcell.audit_purge', 'on', true);
    UPDATE audit_log SET actor_user_id = NULL, actor_name = '(deleted user)', remote_addr = NULL WHERE actor_user_id = uid;
    GET DIAGNOSTICS n = ROW_COUNT;
    UPDATE audit_log SET details = details - 'username' - 'email' - 'display_name', remote_addr = NULL
        WHERE object_type = 'user' AND object_id = uid::text;
    GET DIAGNOSTICS m = ROW_COUNT;
    RETURN n + m;
END;
$$;
REVOKE ALL ON FUNCTION audit_forget_user(uuid) FROM PUBLIC;

-- +down

DROP FUNCTION audit_forget_user(uuid);
CREATE OR REPLACE FUNCTION audit_log_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('opendectcell.audit_purge', true) = 'on'
       AND current_user = (SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'audit_log'::regclass) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$;
