-- +up

-- The history of a VoIP line's registration (ideas B3): one row when the line goes up, goes down, or goes down for a
-- different reason, not one per attempt. A line's page shows it, with the share of time it was registered.
CREATE TABLE sip_reg_event (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES sip_account ON DELETE CASCADE,
    at         timestamptz NOT NULL,
    state      text NOT NULL CHECK (state IN ('registered', 'failed', 'backoff', 'disabled')),
    error      text NOT NULL DEFAULT '' CHECK (length(error) <= 300),
    took_ms    integer CHECK (took_ms >= 0)
);
CREATE INDEX sip_reg_event_account_idx ON sip_reg_event (account_id, at DESC);
CREATE INDEX sip_reg_event_at_idx ON sip_reg_event (at);

-- +down

DROP TABLE sip_reg_event;
