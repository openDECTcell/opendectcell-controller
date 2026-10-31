-- SPDX-License-Identifier: AGPL-3.0-or-later
-- The RFPI the controller last asked a cell to broadcast. NULL: the cell has
-- its own factory identity as far as the controller knows. Set when the
-- controller sends the system identity, cleared when it sends the way back, so
-- a cell is never left with an identity nobody remembers giving it.

-- +up

ALTER TABLE cell ADD COLUMN rfpi_pushed bytea CHECK (length(rfpi_pushed) = 5);

-- +down

ALTER TABLE cell DROP COLUMN rfpi_pushed;
