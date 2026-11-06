-- +goose Up
-- Transcode worker credentials (#615). A join token enrolls workers; each
-- enrolled worker then proves its own key. derived_key is HMAC(secret,
-- label): the server never stores a secret, but a derived key is
-- password-equivalent (the handshake needs it).

-- uses_left NULL means no use limit. expires and revoked are NULL when unset.
CREATE TABLE transcode_worker_join_tokens (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    derived_key BLOB NOT NULL,
    fingerprint TEXT NOT NULL DEFAULT '',
    expires INTEGER,
    uses_left INTEGER,
    created INTEGER NOT NULL,
    revoked INTEGER
);

-- join_token_id names the token that enrolled the worker; revoking that
-- token does not revoke the worker. input_mode is shared, stream or auto.
CREATE TABLE transcode_workers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    join_token_id TEXT NOT NULL,
    derived_key BLOB NOT NULL,
    input_mode TEXT NOT NULL DEFAULT 'auto',
    created INTEGER NOT NULL,
    last_seen INTEGER NOT NULL,
    revoked INTEGER
);

-- Pool labels of a worker (#616 places jobs by pool).
CREATE TABLE transcode_worker_pools (
    worker_id TEXT NOT NULL REFERENCES transcode_workers(id) ON DELETE CASCADE,
    pool TEXT NOT NULL,
    PRIMARY KEY (worker_id, pool)
);

-- Server certificate key fingerprints that pinned workers accept, beyond
-- the pin in their join token: the next key before a rotation.
CREATE TABLE transcode_worker_pins (
    fingerprint TEXT PRIMARY KEY,
    created INTEGER NOT NULL
);

-- +goose Down
DROP TABLE transcode_worker_pins;
DROP TABLE transcode_worker_pools;
DROP TABLE transcode_workers;
DROP TABLE transcode_worker_join_tokens;
