-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Organizations, and whose base stations a user's handsets may use (docs/adr/0007-organizations-and-trust.md).
-- A base station's operator is the user who enrolled it (cell.enrolled_by). An organization only groups users
-- so that others can decide to trust them; it gives its members no rights over each other's things.

-- +up

CREATE TABLE organization (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text NOT NULL CHECK (name <> '' AND length(name) <= 64),
    -- NULL when the owner's account was deleted: a controller admin then looks after it.
    owner_user_id  uuid REFERENCES app_user ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE organization_member (
    org_id        uuid NOT NULL REFERENCES organization ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    role          text NOT NULL CHECK (role IN ('admin', 'member')),
    -- what this member calls the organization; NULL: the organization's own name
    display_name  text CHECK (display_name IS NULL OR (display_name <> '' AND length(display_name) <= 64)),
    joined_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX organization_member_user_idx ON organization_member (user_id);

-- Single-use invitation tokens; only the hash is kept.
CREATE TABLE organization_invite (
    token_hash  bytea PRIMARY KEY CHECK (length(token_hash) = 32),
    org_id      uuid NOT NULL REFERENCES organization ON DELETE CASCADE,
    created_by  uuid REFERENCES app_user ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    used_by     uuid REFERENCES app_user ON DELETE SET NULL
);
CREATE INDEX organization_invite_org_idx ON organization_invite (org_id);

-- 'all': every base station; 'own': the user's own; 'orgs': the user's own and those of the members of trusted_org.
ALTER TABLE app_user ADD COLUMN trust_scope text NOT NULL DEFAULT 'all' CHECK (trust_scope IN ('all', 'own', 'orgs'));

-- An organization a user trusts. It must be one the user belongs to: leaving it ends the trust with it.
CREATE TABLE user_trusted_org (
    user_id  uuid NOT NULL,
    org_id   uuid NOT NULL,
    PRIMARY KEY (user_id, org_id),
    FOREIGN KEY (org_id, user_id) REFERENCES organization_member (org_id, user_id) ON DELETE CASCADE
);

-- May a handset of owner be put on cell? The one place the rule lives.
CREATE FUNCTION handset_may_use_cell(owner uuid, cell uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT CASE (SELECT trust_scope FROM app_user WHERE id = owner)
        WHEN 'all' THEN true
        WHEN 'own' THEN EXISTS (SELECT 1 FROM cell c WHERE c.id = cell AND c.enrolled_by = owner)
        WHEN 'orgs' THEN EXISTS (SELECT 1 FROM cell c WHERE c.id = cell AND (c.enrolled_by = owner OR EXISTS (
            SELECT 1 FROM user_trusted_org t JOIN organization_member m ON m.org_id = t.org_id
            WHERE t.user_id = owner AND m.user_id = c.enrolled_by)))
        ELSE false
    END
$$;

-- +down

DROP FUNCTION handset_may_use_cell(uuid, uuid);
DROP TABLE user_trusted_org;
ALTER TABLE app_user DROP COLUMN trust_scope;
DROP TABLE organization_invite;
DROP TABLE organization_member;
DROP TABLE organization;
