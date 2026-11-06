-- +goose Up
-- The dashboard scaling token (#617): it reads only the transcode metrics
-- and the scaling signals. token_hash is SHA-256 of the token; the server
-- never stores the token.
CREATE TABLE transcode_worker_scaling_token (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    token_hash BLOB NOT NULL,
    created INTEGER NOT NULL
);

-- +goose Down
DROP TABLE transcode_worker_scaling_token;
