-- +goose Up
-- Live play sessions and PlaySessionId grants (#620). The server writes them
-- behind each change and reloads them at startup, so playback continues
-- across a server restart. state is the session's JSON snapshot, owned by
-- internal/session. user_id has no foreign key: the write-behind must not
-- fail when a user is deleted; startup skips the rows of a missing user.
CREATE TABLE play_sessions (
    id TEXT PRIMARY KEY,
    user_id BLOB NOT NULL,
    last_activity INTEGER NOT NULL,
    state BLOB NOT NULL
);

CREATE INDEX play_sessions_last_activity ON play_sessions (last_activity);

-- media_source_ids is a JSON array of strings.
CREATE TABLE play_session_grants (
    play_session_id TEXT PRIMARY KEY,
    user_id BLOB NOT NULL,
    item_id BLOB NOT NULL,
    media_source_ids TEXT NOT NULL,
    last_used INTEGER NOT NULL
);

CREATE INDEX play_session_grants_last_used ON play_session_grants (last_used);

-- +goose Down
DROP TABLE play_session_grants;
DROP TABLE play_sessions;
