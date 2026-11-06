-- +goose Up
-- Remote transcode job records (#618). A worker keeps running its jobs
-- through a server restart; the new server process adopts a job when the
-- worker reconnects and presents it before lease_expires. token_hash is
-- SHA-256 of the job's data-plane token. playlist and segment_pattern are
-- canonical paths in the transcode directory. input_mode is the worker's
-- input mode at placement; source_path and source_size let the next server
-- process serve a streamed source again. worker_id has no foreign key:
-- a static-token worker has no identity row.
CREATE TABLE transcode_worker_jobs (
    id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL,
    playlist TEXT NOT NULL,
    segment_pattern TEXT NOT NULL,
    play_session_id TEXT NOT NULL DEFAULT '',
    device_id TEXT NOT NULL DEFAULT '',
    token_hash BLOB NOT NULL,
    segment_length INTEGER NOT NULL,
    start_number INTEGER NOT NULL DEFAULT 0,
    input_mode TEXT NOT NULL DEFAULT 'auto',
    source_path TEXT NOT NULL DEFAULT '',
    source_size INTEGER NOT NULL DEFAULT 0,
    created INTEGER NOT NULL,
    lease_expires INTEGER NOT NULL
);

CREATE INDEX transcode_worker_jobs_worker ON transcode_worker_jobs (worker_id);

-- +goose Down
DROP TABLE transcode_worker_jobs;
