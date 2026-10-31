-- SPDX-License-Identifier: AGPL-3.0-or-later
-- C3: the rest of a handset's DECT record (libopendectcell's struct dect_hs_rec).
-- The two identity IEs an FP-originated {CC-SETUP} carries, as the handset
-- itself put them on the air; NULL until learned. Neither is secret.

-- +up

ALTER TABLE handset
    ADD COLUMN cc_port_id   bytea CHECK (length(cc_port_id) <= 16),
    ADD COLUMN cc_fixed_id  bytea CHECK (length(cc_fixed_id) <= 16);

-- +down

ALTER TABLE handset DROP COLUMN cc_fixed_id, DROP COLUMN cc_port_id;
