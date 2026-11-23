-- Schema v1. Snapshots are not copies: each entry records the interval of runs
-- in which it was true. A snapshot at run R is every entry with
--   valid_from_run <= R AND (valid_to_run IS NULL OR valid_to_run > R).

CREATE TABLE runs (
    id           INTEGER PRIMARY KEY,
    started_at   TEXT NOT NULL,
    finished_at  TEXT,
    status       TEXT NOT NULL CHECK (status IN ('running', 'complete', 'partial', 'failed')),
    capabilities TEXT NOT NULL DEFAULT ''
);

-- id is the id of the run that produced it (complete or partial runs only).
CREATE TABLE snapshots (
    id         INTEGER PRIMARY KEY REFERENCES runs(id),
    created_at TEXT NOT NULL,
    pinned     INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0, 1)),
    flagged    INTEGER NOT NULL DEFAULT 0 CHECK (flagged IN (0, 1)),
    forgotten  INTEGER NOT NULL DEFAULT 0 CHECK (forgotten IN (0, 1)),
    note       TEXT NOT NULL DEFAULT '',
    folders    INTEGER NOT NULL DEFAULT 0,
    messages   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE folders (
    id             INTEGER PRIMARY KEY,
    name           TEXT NOT NULL,
    delimiter      TEXT NOT NULL DEFAULT '',
    special_use    TEXT NOT NULL DEFAULT '',
    uidvalidity    INTEGER NOT NULL DEFAULT 0,
    last_uid       INTEGER NOT NULL DEFAULT 0,
    highest_modseq INTEGER NOT NULL DEFAULT 0,
    first_seen_run INTEGER NOT NULL REFERENCES runs(id),
    gone_run       INTEGER REFERENCES runs(id)
);
CREATE UNIQUE INDEX folders_live_name ON folders(name) WHERE gone_run IS NULL;

-- One row per unique raw message. sha256 is the hash of the exact server bytes.
-- emailid (RFC 8474) is only a download-avoidance hint, never the key.
CREATE TABLE blobs (
    sha256         BLOB PRIMARY KEY CHECK (length(sha256) = 32),
    size           INTEGER NOT NULL,
    internal_date  TEXT,
    message_id     TEXT,
    emailid        TEXT,
    first_seen_run INTEGER NOT NULL REFERENCES runs(id)
) WITHOUT ROWID;
CREATE INDEX blobs_emailid ON blobs(emailid) WHERE emailid IS NOT NULL;

CREATE TABLE entries (
    id             INTEGER PRIMARY KEY,
    folder_id      INTEGER NOT NULL REFERENCES folders(id),
    uidvalidity    INTEGER NOT NULL,
    uid            INTEGER NOT NULL,
    blob           BLOB NOT NULL REFERENCES blobs(sha256),
    flags          TEXT NOT NULL DEFAULT '',
    valid_from_run INTEGER NOT NULL REFERENCES runs(id),
    valid_to_run   INTEGER REFERENCES runs(id),
    CHECK (valid_to_run IS NULL OR valid_to_run > valid_from_run)
);
CREATE UNIQUE INDEX entries_open_position ON entries(folder_id, uidvalidity, uid) WHERE valid_to_run IS NULL;
CREATE INDEX entries_folder_valid_to ON entries(folder_id, valid_to_run);
CREATE INDEX entries_blob ON entries(blob);
CREATE INDEX entries_valid_from ON entries(valid_from_run);

CREATE TABLE alerts (
    id              INTEGER PRIMARY KEY,
    run_id          INTEGER NOT NULL REFERENCES runs(id),
    kind            TEXT NOT NULL,
    summary         TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    acknowledged_at TEXT
);
CREATE INDEX alerts_unacknowledged ON alerts(id) WHERE acknowledged_at IS NULL;
