-- SPDX-License-Identifier: AGPL-3.0-or-later
-- The instance's own logo (assets), self-registration with admin approval,
-- the opt-in to e-mail about log-level changes, and a base station's profile
-- with one sharing switch per field (name, position, specs, statistics), all
-- off by default: nothing leaves the controller until its owner says so.

-- +up

CREATE TABLE asset (
    key           text PRIMARY KEY CHECK (key <> ''),
    content_type  text NOT NULL,
    data          bytea NOT NULL CHECK (length(data) <= 524288),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE app_user
    ADD COLUMN pending_approval   boolean NOT NULL DEFAULT false,
    ADD COLUMN notify_log_changes boolean NOT NULL DEFAULT false;

ALTER TABLE cell
    ADD COLUMN box_model          text NOT NULL DEFAULT '' CHECK (length(box_model) <= 64),
    ADD COLUMN latitude           double precision CHECK (latitude BETWEEN -90 AND 90),
    ADD COLUMN longitude          double precision CHECK (longitude BETWEEN -180 AND 180),
    ADD COLUMN position_precision text NOT NULL DEFAULT 'approx'
                                  CHECK (position_precision IN ('exact', 'approx', 'region')),
    ADD COLUMN share_name         boolean NOT NULL DEFAULT false,
    ADD COLUMN share_position     boolean NOT NULL DEFAULT false,
    ADD COLUMN share_specs        boolean NOT NULL DEFAULT false,
    ADD COLUMN share_stats        boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT cell_position_pair CHECK ((latitude IS NULL) = (longitude IS NULL));

-- +down

ALTER TABLE cell
    DROP CONSTRAINT cell_position_pair,
    DROP COLUMN share_stats, DROP COLUMN share_specs, DROP COLUMN share_position,
    DROP COLUMN share_name, DROP COLUMN position_precision, DROP COLUMN longitude,
    DROP COLUMN latitude, DROP COLUMN box_model;
ALTER TABLE app_user DROP COLUMN notify_log_changes, DROP COLUMN pending_approval;
DROP TABLE asset;
