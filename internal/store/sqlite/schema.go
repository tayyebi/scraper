package sqlite

// migrations are applied in order and recorded, so an existing database is
// upgraded rather than recreated. Never edit a migration that has shipped --
// append a new one. A migration that changes underneath a deployed hub leaves
// two databases claiming the same version with different shapes.
var migrations = []string{
	// 1: initial schema.
	`
CREATE TABLE agents (
    id              TEXT PRIMARY KEY,
    name            TEXT    NOT NULL DEFAULT '',
    labels          TEXT    NOT NULL DEFAULT '{}',
    capabilities    TEXT    NOT NULL DEFAULT '{}',
    status          TEXT    NOT NULL DEFAULT 'offline',
    browser         TEXT    NOT NULL DEFAULT '',
    browser_version TEXT    NOT NULL DEFAULT '',
    platform        TEXT    NOT NULL DEFAULT '',
    user_agent      TEXT    NOT NULL DEFAULT '',
    agent_version   TEXT    NOT NULL DEFAULT '',
    enrolled_at     INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL
);

-- Credentials live in their own table so an agent row can be read and shown
-- without the secret hash ever being loaded into a struct that gets serialized.
CREATE TABLE agent_credentials (
    agent_id   TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    hash       TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);

CREATE TABLE enrollment_tokens (
    id         TEXT PRIMARY KEY,
    hash       TEXT    NOT NULL UNIQUE,
    labels     TEXT    NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER,
    used_by    TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    name         TEXT    NOT NULL DEFAULT '',
    hash         TEXT    NOT NULL UNIQUE,
    scope        TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER
);

CREATE TABLE console_sessions (
    id         TEXT PRIMARY KEY,
    hash       TEXT    NOT NULL UNIQUE,
    username   TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    agent_id   TEXT    NOT NULL,
    tab_id     INTEGER NOT NULL DEFAULT 0,
    url        TEXT    NOT NULL DEFAULT '',
    title      TEXT    NOT NULL DEFAULT '',
    origin     TEXT    NOT NULL,
    state      TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    closed_at  INTEGER
);
CREATE INDEX sessions_by_agent ON sessions(agent_id, state);
CREATE INDEX sessions_by_state ON sessions(state, created_at DESC);

CREATE TABLE commands (
    id          TEXT PRIMARY KEY,
    session_id  TEXT    NOT NULL DEFAULT '',
    agent_id    TEXT    NOT NULL DEFAULT '',
    op          TEXT    NOT NULL,
    params      TEXT,
    state       TEXT    NOT NULL,
    result      TEXT,
    error       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    deadline_at INTEGER NOT NULL DEFAULT 0,
    done_at     INTEGER
);
CREATE INDEX commands_by_session ON commands(session_id, created_at DESC);

CREATE TABLE events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id   TEXT    NOT NULL DEFAULT '',
    session_id TEXT    NOT NULL DEFAULT '',
    type       TEXT    NOT NULL,
    seq        INTEGER NOT NULL DEFAULT 0,
    body       TEXT,
    at         INTEGER NOT NULL
);
CREATE INDEX events_by_session ON events(session_id, id);
CREATE INDEX events_by_time ON events(at);

-- Bodies are digests, never bytes: see internal/store/blob.
CREATE TABLE exchanges (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT    NOT NULL,
    agent_id        TEXT    NOT NULL DEFAULT '',
    request_id      TEXT    NOT NULL DEFAULT '',
    method          TEXT    NOT NULL DEFAULT 'GET',
    url             TEXT    NOT NULL DEFAULT '',
    status          INTEGER NOT NULL DEFAULT 0,
    status_text     TEXT    NOT NULL DEFAULT '',
    mime_type       TEXT    NOT NULL DEFAULT '',
    resource_type   TEXT    NOT NULL DEFAULT '',
    initiator       TEXT    NOT NULL DEFAULT '',
    req_headers     TEXT,
    res_headers     TEXT,
    req_body_digest TEXT    NOT NULL DEFAULT '',
    res_body_digest TEXT    NOT NULL DEFAULT '',
    req_body_size   INTEGER NOT NULL DEFAULT 0,
    res_body_size   INTEGER NOT NULL DEFAULT 0,
    from_cache      INTEGER NOT NULL DEFAULT 0,
    truncated       INTEGER NOT NULL DEFAULT 0,
    started_at      INTEGER NOT NULL,
    duration_ms     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX exchanges_by_session ON exchanges(session_id, id);
CREATE INDEX exchanges_by_time ON exchanges(started_at);
CREATE INDEX exchanges_by_res_digest ON exchanges(res_body_digest);
CREATE INDEX exchanges_by_req_digest ON exchanges(req_body_digest);
`,
}
