CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS invites (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    recipient_token_hash BLOB NOT NULL UNIQUE CHECK(length(recipient_token_hash) = 32),
    status_token_hash BLOB NOT NULL UNIQUE CHECK(length(status_token_hash) = 32),
    asker_name TEXT NOT NULL,
    recipient_name TEXT NOT NULL,
    pronoun TEXT NOT NULL,
    offered_ideas TEXT NOT NULL,
    proposed_slots TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    accepted_at INTEGER,
    expires_at INTEGER NOT NULL,
    selected_ideas TEXT,
    custom_idea TEXT,
    selected_slot_index INTEGER
);

CREATE INDEX IF NOT EXISTS idx_invites_expires_at ON invites(expires_at);

CREATE TABLE IF NOT EXISTS creation_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ip_hash BLOB NOT NULL CHECK(length(ip_hash) = 32),
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_creation_events_ip_time ON creation_events(ip_hash, created_at);
CREATE INDEX IF NOT EXISTS idx_creation_events_time ON creation_events(created_at);
