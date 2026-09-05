CREATE TABLE creation_events_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at INTEGER NOT NULL
);

INSERT INTO creation_events_new(created_at)
SELECT created_at FROM creation_events;

DROP TABLE creation_events;
ALTER TABLE creation_events_new RENAME TO creation_events;

CREATE INDEX idx_creation_events_time ON creation_events(created_at);
