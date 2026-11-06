-- +goose Up
-- Server replicas (#652). With DEADFIN_REPLICA_URL each server process has a
-- random replica ID and a row here; a heartbeat moves lease_expires. A
-- replica is live while its lease is not over. url is where the other
-- replicas forward the requests that this replica owns. draining is 1 from
-- SIGTERM until the replica deletes its row.
CREATE TABLE server_replicas (
    id TEXT PRIMARY KEY,
    url TEXT NOT NULL,
    draining INTEGER NOT NULL DEFAULT 0,
    lease_expires INTEGER NOT NULL
);

-- The one key that signs a forward between replicas. The first replica
-- creates it.
CREATE TABLE server_replica_secret (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    secret BLOB NOT NULL
);

-- owner is the replica that serves the row; '' is no owner. A row whose
-- owner is not live can be claimed. Without replica mode every owner is ''.
ALTER TABLE play_sessions ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE play_session_grants ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE transcode_worker_jobs ADD COLUMN owner TEXT NOT NULL DEFAULT '';

CREATE INDEX play_sessions_owner ON play_sessions (owner);
CREATE INDEX play_session_grants_owner ON play_session_grants (owner);
CREATE INDEX transcode_worker_jobs_owner ON transcode_worker_jobs (owner);

-- +goose Down
DROP INDEX transcode_worker_jobs_owner;
DROP INDEX play_session_grants_owner;
DROP INDEX play_sessions_owner;
ALTER TABLE transcode_worker_jobs DROP COLUMN owner;
ALTER TABLE play_session_grants DROP COLUMN owner;
ALTER TABLE play_sessions DROP COLUMN owner;
DROP TABLE server_replica_secret;
DROP TABLE server_replicas;
