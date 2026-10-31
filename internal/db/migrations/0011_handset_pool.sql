-- SPDX-License-Identifier: AGPL-3.0-or-later
-- A pooled handset is held by every cell, host-only (record numbers above the
-- chip's six slots), under a TPUI from the shared pool {00, 11, N}, N from 7:
-- the same page identity on every cell (docs/roaming.md). Handsets enrolled
-- before stay chip-slot handsets of their home cells.

-- +up

ALTER TABLE handset ADD COLUMN pooled boolean NOT NULL DEFAULT false;

-- +down

ALTER TABLE handset DROP COLUMN pooled;
