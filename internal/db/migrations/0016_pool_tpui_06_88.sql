-- SPDX-License-Identifier: AGPL-3.0-or-later
-- A pool TPUI {00, 11, N} must have a last byte the 06.88 chip hears: low three
-- bits 1 to 6, bit 3 clear, N from 0x10 (bench 2026-11-02; the maintainers' notes).
-- A pooled handset that holds another gets the next free one; it takes it at
-- its next location registration.

-- +up

WITH free AS (
	SELECT n, row_number() OVER (ORDER BY n) AS rn
	FROM generate_series(16, 255) AS n
	WHERE n % 16 BETWEEN 1 AND 6
	  AND NOT EXISTS (SELECT 1 FROM handset h WHERE h.tpui & 65535 = 4352 + n)
), bad AS (
	SELECT id, row_number() OVER (ORDER BY registered_at, id) AS rn
	FROM handset
	WHERE pooled AND tpui & 65280 = 4352
	  AND NOT ((tpui & 255) >= 16 AND (tpui & 255) % 16 BETWEEN 1 AND 6)
)
UPDATE handset h SET tpui = 4352 + free.n
FROM bad JOIN free ON free.rn = bad.rn
WHERE h.id = bad.id;

-- +down

-- The TPUIs stay as they are: both sets are valid for the 08.25 chip.
SELECT 1;
