-- SPDX-License-Identifier: AGPL-3.0-or-later
-- The TURN password of a VoIP profile (LuCI: stunpass), sealed like the SIP
-- password. It gets its own column, not a field of `advanced`, so that
-- rotate-dek (db.EncryptedColumns) finds it and the API never returns it.

-- +up

ALTER TABLE sip_account ADD COLUMN stunpass_enc bytea;

-- +down

ALTER TABLE sip_account DROP COLUMN stunpass_enc;
