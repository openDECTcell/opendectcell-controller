-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Base station names need not be unique (maintainer, 2026-11-01): the public
-- map tells stations apart by their random id. Enrolment still avoids a clash
-- when it picks the first name.

-- +up

DROP INDEX cell_name_key;

-- +down

CREATE UNIQUE INDEX cell_name_key ON cell (lower(name));
