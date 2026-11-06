-- +goose Up
-- Transcode placement policy (#616, docs/spikes/transcode-workers.md §3.8).
-- One row holds the policy as JSON. No row means the default policy: every
-- job runs locally.
CREATE TABLE transcode_worker_policy (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    policy TEXT NOT NULL,
    updated INTEGER NOT NULL
);

-- +goose Down
DROP TABLE transcode_worker_policy;
