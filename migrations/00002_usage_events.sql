-- +goose Up
-- One row per chat request. Append-only and written in COPY batches.
-- No foreign keys: the ledger must accept rows even if a key or project is
-- later deleted, and FK checks would slow bulk inserts.
CREATE TABLE usage_events (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts              timestamptz NOT NULL,
    request_id      text NOT NULL,
    project_id      uuid NOT NULL,
    key_id          uuid NOT NULL,
    model           text NOT NULL,
    provider        text NOT NULL,
    tokens_in       integer NOT NULL,
    tokens_out      integer NOT NULL,
    usage_estimated boolean NOT NULL,
    latency_ms      integer NOT NULL,
    upstream_ms     integer NOT NULL,
    ttft_ms         integer,                 -- streams only
    cost_usd        numeric(12, 6) NOT NULL,
    cost_saved_usd  numeric(12, 6) NOT NULL, -- cache hits
    cache           text NOT NULL,           -- HIT | MISS | BYPASS
    status          smallint NOT NULL,
    error_class     text,
    attempts        smallint NOT NULL,
    stream          boolean NOT NULL
);

-- Spend per project over time is the dominant query.
CREATE INDEX usage_events_project_ts_idx ON usage_events (project_id, ts);
-- BRIN: rows arrive in ts order, so a tiny block-range index serves time scans.
CREATE INDEX usage_events_ts_brin ON usage_events USING brin (ts);

-- +goose Down
DROP TABLE usage_events;
