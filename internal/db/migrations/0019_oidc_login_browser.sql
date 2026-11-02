-- SPDX-License-Identifier: AGPL-3.0-or-later
-- An in-flight OIDC login belongs to the browser that started it: the callback must come with the cookie
-- whose hash is stored here, so a callback URL handed to somebody else logs them in as nobody. Logins
-- under way at the upgrade are dropped (they last minutes).

-- +up

DELETE FROM oidc_login;
ALTER TABLE oidc_login ADD COLUMN browser_hash bytea NOT NULL CHECK (length(browser_hash) = 32);

-- +down

ALTER TABLE oidc_login DROP COLUMN browser_hash;
