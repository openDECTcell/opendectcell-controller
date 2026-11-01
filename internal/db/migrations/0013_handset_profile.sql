-- SPDX-License-Identifier: AGPL-3.0-or-later
-- The device profile of a handset (the id of a libopendectcell profile file such as
-- gigaset-a540, or a manufacturer's such as gigaset): what the DECT stack knows
-- about the model's habits. NULL: the standard behaviour. handset.model keeps
-- the name the UI shows.

-- +up

ALTER TABLE handset ADD COLUMN profile text CHECK (profile IS NULL OR length(profile) BETWEEN 1 AND 64);

-- +down

ALTER TABLE handset DROP COLUMN profile;
