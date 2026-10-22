-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Initial schema. See docs/schema.md for the reasoning behind each table.
-- Columns ending in _enc hold secret.Keyring ciphertexts bound to
-- (table, column, primary key); see docs/threat-model.md §4.2.

-- +up

CREATE TABLE app_user (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username       text NOT NULL CHECK (username <> '' AND length(username) <= 128),
    display_name   text NOT NULL DEFAULT '',
    email          text,
    role           text NOT NULL CHECK (role IN ('admin', 'user')),
    may_add_cells  boolean NOT NULL DEFAULT false,
    auth_source    text NOT NULL CHECK (auth_source IN ('local', 'oidc')),
    password_hash  text,
    oidc_issuer    text,
    oidc_subject   text,
    disabled       boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_login_at  timestamptz,
    CHECK (auth_source = 'local' OR password_hash IS NULL),
    CHECK ((auth_source = 'oidc') = (oidc_issuer IS NOT NULL AND oidc_subject IS NOT NULL))
);
CREATE UNIQUE INDEX app_user_username_key ON app_user (lower(username));
CREATE UNIQUE INDEX app_user_oidc_key ON app_user (oidc_issuer, oidc_subject);

CREATE TABLE session (
    token_hash    bytea PRIMARY KEY CHECK (length(token_hash) = 32),
    user_id       uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    auth_method   text NOT NULL CHECK (auth_method IN ('local', 'oidc')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL
);
CREATE INDEX session_user_idx ON session (user_id);
CREATE INDEX session_expires_idx ON session (expires_at);

-- Failed logins, for rate limiting across controller instances.
CREATE TABLE login_failure (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    key  text NOT NULL,
    at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX login_failure_key_at_idx ON login_failure (key, at);

-- In-flight OIDC logins: state, nonce and PKCE verifier, valid for minutes.
CREATE TABLE oidc_login (
    state_hash     bytea PRIMARY KEY CHECK (length(state_hash) = 32),
    nonce          text NOT NULL,
    code_verifier  text NOT NULL,
    return_to      text NOT NULL DEFAULT '/',
    expires_at     timestamptz NOT NULL
);

-- Data keys, wrapped by the master key (docs/threat-model.md §4.2).
CREATE TABLE data_key (
    id          integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kek_id      text NOT NULL,
    wrapped     bytea NOT NULL,
    active      boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX data_key_one_active ON data_key (active) WHERE active;

-- Installation-wide settings (policies such as slot_policy), as JSON values.
CREATE TABLE setting (
    key         text PRIMARY KEY,
    value       jsonb NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Cells (docs/cell-protocol.md §6.2 for the states).
CREATE TABLE cell (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name              text NOT NULL CHECK (name <> '' AND length(name) <= 64),
    location          text NOT NULL DEFAULT '',
    state             text NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending', 'active', 'disabled', 'revoked')),
    enrolled_by       uuid REFERENCES app_user ON DELETE SET NULL,
    enrolled_at       timestamptz,
    approved_by       uuid REFERENCES app_user ON DELETE SET NULL,
    approved_at       timestamptz,
    rfpi              bytea CHECK (length(rfpi) = 5),
    rpn               smallint CHECK (rpn BETWEEN 0 AND 255),
    software_version  text,
    stub_version      text,
    cap_sub_records   smallint CHECK (cap_sub_records >= 0),
    cap_audio_nb      smallint CHECK (cap_audio_nb >= 0),
    cap_audio_wb      smallint CHECK (cap_audio_wb >= 0),
    max_calls         smallint NOT NULL DEFAULT 1 CHECK (max_calls >= 0),
    allow_plain_rtp   boolean NOT NULL DEFAULT false,
    last_seen_at      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX cell_name_key ON cell (lower(name));

-- The private cell CA (docs/cell-protocol.md §4.1). Old CAs stay during rotation.
CREATE TABLE cell_ca (
    id          integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    cert_der    bytea NOT NULL,
    key_enc     bytea NOT NULL,
    active      boolean NOT NULL DEFAULT false,
    not_after   timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX cell_ca_one_active ON cell_ca (active) WHERE active;

-- Per-cell client certificates; a cell's current set is those not revoked
-- and not expired (docs/cell-protocol.md §4.2).
CREATE TABLE cell_certificate (
    fingerprint  bytea PRIMARY KEY CHECK (length(fingerprint) = 32),
    cell_id      uuid NOT NULL REFERENCES cell ON DELETE CASCADE,
    serial       text NOT NULL,
    not_before   timestamptz NOT NULL,
    not_after    timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);
CREATE INDEX cell_certificate_cell_idx ON cell_certificate (cell_id);

CREATE TABLE cell_enrollment_token (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash    bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_by    uuid REFERENCES app_user ON DELETE SET NULL,
    note          text NOT NULL DEFAULT '',
    auto_approve  boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz,
    cell_id       uuid REFERENCES cell ON DELETE SET NULL,
    CHECK ((used_at IS NULL) = (cell_id IS NULL))
);

-- Handsets. owner_user_id NULL means a shared, admin-managed handset.
-- The TPUI comes from an installation-wide pool (docs/cell-protocol.md §7.4);
-- its low 16 bits are the link signature, which must be unique too.
CREATE TABLE handset (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id      uuid REFERENCES app_user ON DELETE RESTRICT,
    ipui               bytea NOT NULL UNIQUE CHECK (length(ipui) BETWEEN 1 AND 16),
    tpui               integer NOT NULL UNIQUE CHECK (tpui BETWEEN 0 AND 1048575),
    uak_enc            bytea NOT NULL,
    display_name       text NOT NULL DEFAULT '' CHECK (length(display_name) <= 64),
    manufacturer_emc   integer CHECK (manufacturer_emc BETWEEN 0 AND 65535),
    model              text,
    capabilities       jsonb NOT NULL DEFAULT '{}',
    default_line       smallint CHECK (default_line IN (1, 2)),
    dual_ring_policy   text NOT NULL DEFAULT 'call_waiting'
                       CHECK (dual_ring_policy IN ('call_waiting', 'busy')),
    last_seen_cell_id  uuid REFERENCES cell ON DELETE SET NULL,
    last_seen_at       timestamptz,
    registered_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX handset_lsig_key ON handset ((tpui & 65535));
CREATE INDEX handset_owner_idx ON handset (owner_user_id);

-- A handset's home set: the cells that always hold its record (§7.4 rule 1).
CREATE TABLE handset_home_cell (
    handset_id  uuid NOT NULL REFERENCES handset ON DELETE CASCADE,
    cell_id     uuid NOT NULL REFERENCES cell ON DELETE CASCADE,
    PRIMARY KEY (handset_id, cell_id)
);
CREATE INDEX handset_home_cell_cell_idx ON handset_home_cell (cell_id);

-- The controller's view of each chip's subscription table (§7.4). The chip
-- is a write-only cache of this.
CREATE TABLE cell_slot (
    cell_id       uuid NOT NULL REFERENCES cell ON DELETE CASCADE,
    slot          smallint NOT NULL CHECK (slot BETWEEN 1 AND 64),
    handset_id    uuid REFERENCES handset ON DELETE SET NULL,
    state         text NOT NULL DEFAULT 'unknown'
                  CHECK (state IN ('empty', 'writing', 'present', 'unknown')),
    written_at    timestamptz,
    last_seen_at  timestamptz,
    pinned        boolean NOT NULL DEFAULT false,
    PRIMARY KEY (cell_id, slot)
);
CREATE UNIQUE INDEX cell_slot_handset_key ON cell_slot (cell_id, handset_id) WHERE handset_id IS NOT NULL;

-- SIP accounts (docs/voip-requirements.md §2). The "must" parameters are
-- columns; the "should"/"later" ones are in advanced, validated by the
-- application. owner_user_id NULL means a shared, admin-managed account.
CREATE TABLE sip_account (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id     uuid REFERENCES app_user ON DELETE RESTRICT,
    label             text NOT NULL CHECK (label <> '' AND length(label) <= 64),
    display_name      text NOT NULL DEFAULT '',
    username          text NOT NULL CHECK (username <> ''),
    domain            text NOT NULL CHECK (domain <> ''),
    auth_username     text,
    password_enc      bytea,
    registrar         text,
    outbound_proxy    text,
    transport         text NOT NULL DEFAULT 'tls' CHECK (transport IN ('udp', 'tcp', 'tls')),
    register          boolean NOT NULL DEFAULT true,
    register_expiry   integer NOT NULL DEFAULT 600 CHECK (register_expiry BETWEEN 60 AND 86400),
    keepalive         text NOT NULL DEFAULT 'auto' CHECK (keepalive IN ('auto', 'none', 'crlf', 'options')),
    media_encryption  text NOT NULL DEFAULT 'optional'
                      CHECK (media_encryption IN ('off', 'optional', 'mandatory')),
    codecs            text[] NOT NULL DEFAULT '{G722,PCMA,PCMU}',
    dtmf_mode         text NOT NULL DEFAULT 'rfc4733' CHECK (dtmf_mode IN ('rfc4733', 'info', 'inband')),
    caller_id_source  text NOT NULL DEFAULT 'auto' CHECK (caller_id_source IN ('auto', 'from', 'pai', 'rpid')),
    tls_verify        boolean NOT NULL DEFAULT true,
    tls_ca_pem        text,
    advanced          jsonb NOT NULL DEFAULT '{}',
    enabled           boolean NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sip_account_owner_idx ON sip_account (owner_user_id);

-- 0-2 lines per handset. The account's owner must match the handset's
-- (or the account is shared); the application enforces that.
CREATE TABLE handset_line (
    handset_id      uuid NOT NULL REFERENCES handset ON DELETE CASCADE,
    line            smallint NOT NULL CHECK (line IN (1, 2)),
    sip_account_id  uuid NOT NULL REFERENCES sip_account ON DELETE RESTRICT,
    enabled         boolean NOT NULL DEFAULT true,
    display_name    text NOT NULL DEFAULT '',
    PRIMARY KEY (handset_id, line)
);
CREATE INDEX handset_line_account_idx ON handset_line (sip_account_id);

-- Registration windows (architecture.md §5). One open window per user.
CREATE TABLE registration_window (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    opened_by       uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    owner_user_id   uuid REFERENCES app_user ON DELETE CASCADE,
    pin_hash        bytea NOT NULL CHECK (length(pin_hash) = 32),
    state           text NOT NULL DEFAULT 'open'
                    CHECK (state IN ('open', 'completed', 'expired', 'cancelled')),
    opened_at       timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    closed_at       timestamptz,
    handset_id      uuid REFERENCES handset ON DELETE SET NULL
);
CREATE UNIQUE INDEX registration_window_one_open ON registration_window (opened_by) WHERE state = 'open';

CREATE TABLE registration_window_cell (
    window_id  uuid NOT NULL REFERENCES registration_window ON DELETE CASCADE,
    cell_id    uuid NOT NULL REFERENCES cell ON DELETE CASCADE,
    PRIMARY KEY (window_id, cell_id)
);

-- Call history. owner_user_id is a snapshot of the handset's owner, so a
-- user keeps their history after deleting a handset.
CREATE TABLE call_log (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    handset_id       uuid REFERENCES handset ON DELETE SET NULL,
    owner_user_id    uuid REFERENCES app_user ON DELETE SET NULL,
    line             smallint CHECK (line IN (1, 2)),
    sip_account_id   uuid REFERENCES sip_account ON DELETE SET NULL,
    direction        text NOT NULL CHECK (direction IN ('in', 'out')),
    remote_party     text NOT NULL DEFAULT '',
    started_at       timestamptz NOT NULL,
    answered_at      timestamptz,
    ended_at         timestamptz,
    end_reason       text,
    cell_ids         uuid[] NOT NULL DEFAULT '{}',
    sip_codec        text,
    cell_codec       text,
    sip_tls          boolean,
    srtp             boolean,
    quality          jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX call_log_started_idx ON call_log (started_at DESC);
CREATE INDEX call_log_owner_idx ON call_log (owner_user_id, started_at DESC);
CREATE INDEX call_log_handset_idx ON call_log (handset_id, started_at DESC);

-- Per-handset feature module settings (docs/dect-features.md).
CREATE TABLE feature_setting (
    handset_id  uuid NOT NULL REFERENCES handset ON DELETE CASCADE,
    feature     text NOT NULL,
    enabled     boolean NOT NULL DEFAULT false,
    params      jsonb NOT NULL DEFAULT '{}',
    PRIMARY KEY (handset_id, feature)
);

-- Audit log. Append-only: updates are refused, and deletes only pass inside
-- a transaction that has set opendectcell.audit_purge = 'on' (retention).
-- actor_user_id has no foreign key, so entries survive the user's deletion;
-- actor_name is a snapshot.
CREATE TABLE audit_log (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at             timestamptz NOT NULL DEFAULT now(),
    actor_user_id  uuid,
    actor_name     text NOT NULL,
    action         text NOT NULL,
    object_type    text NOT NULL DEFAULT '',
    object_id      text NOT NULL DEFAULT '',
    details        jsonb NOT NULL DEFAULT '{}',
    remote_addr    inet
);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);
CREATE INDEX audit_log_object_idx ON audit_log (object_type, object_id, at DESC);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_user_id, at DESC);

CREATE FUNCTION audit_log_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('opendectcell.audit_purge', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$;
CREATE TRIGGER audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_guard();
CREATE TRIGGER audit_log_no_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_guard();

-- +down

DROP TABLE audit_log;
DROP FUNCTION audit_log_guard();
DROP TABLE feature_setting;
DROP TABLE call_log;
DROP TABLE registration_window_cell;
DROP TABLE registration_window;
DROP TABLE handset_line;
DROP TABLE sip_account;
DROP TABLE cell_slot;
DROP TABLE handset_home_cell;
DROP TABLE handset;
DROP TABLE cell_enrollment_token;
DROP TABLE cell_certificate;
DROP TABLE cell_ca;
DROP TABLE cell;
DROP TABLE setting;
DROP TABLE data_key;
DROP TABLE oidc_login;
DROP TABLE login_failure;
DROP TABLE session;
DROP TABLE app_user;
