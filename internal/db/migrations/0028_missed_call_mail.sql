-- SPDX-License-Identifier: AGPL-3.0-or-later
-- E-mail for a missed call, for the users who ask for it (ideas A4): the opt-in on the account, and a mark on the
-- call so that each missed call is mailed once.

-- +up

ALTER TABLE app_user ADD COLUMN notify_missed_calls boolean NOT NULL DEFAULT false;
ALTER TABLE call_log ADD COLUMN missed_notified_at timestamptz;
CREATE INDEX call_log_missed_pending_idx ON call_log (owner_user_id, started_at)
  WHERE direction = 'in' AND answered_at IS NULL AND missed_notified_at IS NULL;

-- +down

DROP INDEX call_log_missed_pending_idx;
ALTER TABLE call_log DROP COLUMN missed_notified_at;
ALTER TABLE app_user DROP COLUMN notify_missed_calls;
