-- SPDX-License-Identifier: AGPL-3.0-or-later
-- A TPUI must be unique among the handsets in one cell's table, not across the
-- controller: the chip pages and hears by it, and a cell holds 32 records. A
-- 06.88 chip hears only 90 values of the pool, so a global rule capped the
-- controller at 90 handsets. The store keeps them unique per cell when a
-- handset is placed in a cell's table (the maintainers' notes, 2026-11-02).

-- +up

ALTER TABLE handset DROP CONSTRAINT handset_tpui_key;
DROP INDEX handset_lsig_key;
CREATE INDEX handset_tpui_idx ON handset (tpui);

-- +down

DROP INDEX handset_tpui_idx;
CREATE UNIQUE INDEX handset_lsig_key ON handset ((tpui & 65535));
ALTER TABLE handset ADD CONSTRAINT handset_tpui_key UNIQUE (tpui);
