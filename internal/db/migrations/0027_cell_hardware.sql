-- SPDX-License-Identifier: AGPL-3.0-or-later
-- What a base station says it is (the hardware model it reports in its hello), kept so the pages and the statistics can
-- show it without the owner typing it in; and whether the owner chose the name, so that a name left at the OpenWrt
-- default can be replaced by one made of the model and the base station's identity.

-- +up

ALTER TABLE cell ADD COLUMN hw_model text NOT NULL DEFAULT '' CHECK (length(hw_model) <= 64);
ALTER TABLE cell ADD COLUMN name_custom boolean NOT NULL DEFAULT false;

-- the model each existing cell reported when it enrolled (a cell that reconnects tells it again)
UPDATE cell c SET hw_model = left(a.model, 64)
  FROM (SELECT DISTINCT ON (object_id) object_id, details->>'hw_model' AS model
          FROM audit_log WHERE action = 'cell.enroll' AND object_type = 'cell'
         ORDER BY object_id, at DESC) a
 WHERE a.object_id = c.id::text AND coalesce(a.model, '') <> '';

-- +down

ALTER TABLE cell DROP COLUMN name_custom;
ALTER TABLE cell DROP COLUMN hw_model;
