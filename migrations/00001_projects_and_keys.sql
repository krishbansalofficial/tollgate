-- +goose Up
CREATE TABLE projects (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text NOT NULL UNIQUE,
    monthly_budget_usd numeric(12, 2),            -- NULL = no budget (enforced in a later phase)
    rpm_limit          integer NOT NULL DEFAULT 60,
    tpm_limit          integer NOT NULL DEFAULT 100000,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    key_hash   bytea NOT NULL UNIQUE,              -- sha256(raw key); the raw key is never stored
    key_prefix text NOT NULL,                       -- first chars, for display only
    name       text NOT NULL DEFAULT '',
    rpm_limit  integer,                             -- NULL = inherit from project
    tpm_limit  integer,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX api_keys_project_id_idx ON api_keys (project_id);

-- +goose Down
DROP TABLE api_keys;
DROP TABLE projects;
