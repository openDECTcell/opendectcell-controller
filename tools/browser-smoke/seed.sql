-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Test data for the browser smoke test: a base station and a handset each for anna and ben (ben's are disclosed).
INSERT INTO cell (name, location, state, enrolled_by, enrolled_at, last_seen_at, box_model, latitude, longitude, rfpi, software_version)
 SELECT 'Annas lab', 'Kiel', 'active', id, now(), now(), 'FRITZ!Box 7520', 54.32, 10.14, '\x0369ca0e24'::bytea, '0.3.4' FROM app_user WHERE username='anna';
INSERT INTO cell (name, location, state, enrolled_by, enrolled_at, last_seen_at, box_model, disclosed_to_admins)
 SELECT 'Bens flat', 'Hamburg', 'active', id, now(), now() - interval '2 hours', 'FRITZ!Box 7412', true FROM app_user WHERE username='ben';
INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name)
 SELECT id, '\x0012345678'::bytea, 1, '\x00'::bytea, 'Annas A540' FROM app_user WHERE username='anna';
INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name, disclosed_to_admins)
 SELECT id, '\x0087654321'::bytea, 2, '\x00'::bytea, 'Bens C430', true FROM app_user WHERE username='ben';
