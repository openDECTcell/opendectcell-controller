-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Where a handset went: one row each time it was heard on another cell than
-- last time (docs/roaming.md). Location data: the retention job erases it with
-- the call details.

-- +up

CREATE TABLE handset_move (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    handset_id   uuid NOT NULL REFERENCES handset ON DELETE CASCADE,
    from_cell_id uuid REFERENCES cell ON DELETE SET NULL,
    to_cell_id   uuid REFERENCES cell ON DELETE SET NULL,
    at           timestamptz NOT NULL
);
CREATE INDEX handset_move_handset ON handset_move (handset_id, at DESC);
CREATE INDEX handset_move_at ON handset_move (at);

-- +down

DROP TABLE handset_move;
