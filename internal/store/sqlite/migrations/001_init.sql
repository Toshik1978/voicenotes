-- Timestamps are stored as INTEGER Unix nanoseconds (UTC); NULL when unset.
CREATE TABLE captures (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    source            TEXT    NOT NULL,
    source_chat_id    INTEGER NOT NULL,
    source_message_id INTEGER NOT NULL,
    kind              TEXT    NOT NULL,
    received_at       TIMESTAMP NOT NULL,
    file_id           TEXT,
    file_size         INTEGER,
    mime_type         TEXT,
    audio_path        TEXT,
    text              TEXT,
    language          TEXT,
    enriched_json     TEXT,
    llm_skipped       TEXT,
    failure           TEXT,
    note_name         TEXT,
    note_url          TEXT,
    task_count        INTEGER NOT NULL DEFAULT 0,
    reply_message_id  INTEGER,
    step              TEXT    NOT NULL,
    status            TEXT    NOT NULL,
    waiting_on        TEXT,
    attempts          INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   TIMESTAMP NOT NULL,
    updated_at        TIMESTAMP NOT NULL,
    misconfig         INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT,
    queue_reply_edit  INTEGER NOT NULL DEFAULT 0,
    minimal_note      INTEGER NOT NULL DEFAULT 0,
    send_new_reply    INTEGER NOT NULL DEFAULT 0,
    enrich_started_at TIMESTAMP,
    UNIQUE (source, source_chat_id, source_message_id)
);

CREATE INDEX captures_status_id ON captures (status, id);

CREATE TABLE digest_runs (
    local_date TEXT PRIMARY KEY,
    status     TEXT NOT NULL,
    sent_at    TIMESTAMP NOT NULL,
    note_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE digest_seen (
    note_name   TEXT PRIMARY KEY,
    first_shown TEXT NOT NULL
);
