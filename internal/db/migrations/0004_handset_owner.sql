-- SPDX-License-Identifier: AGPL-3.0-or-later
-- A handset always has an owner (the maintainer, the maintainers' notes point 2): it
-- enrols only through a registration window, and the window's user owns it.
-- A team's handset belongs to an organisation or admin account, never NULL.
-- Fails on a database that holds a handset without an owner: give it one.

-- +up

ALTER TABLE handset ALTER COLUMN owner_user_id SET NOT NULL;

-- +down

ALTER TABLE handset ALTER COLUMN owner_user_id DROP NOT NULL;
