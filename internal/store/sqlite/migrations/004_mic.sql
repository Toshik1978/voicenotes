ALTER TABLE captures ADD COLUMN no_speech INTEGER NOT NULL DEFAULT 0;

CREATE TABLE mic_replies (
    recording_id INTEGER PRIMARY KEY,
    chat_id      INTEGER NOT NULL,
    message_id   INTEGER NOT NULL,
    created_at   TEXT    NOT NULL
);

CREATE TABLE mic_recorders (
    name             TEXT PRIMARY KEY,
    last_seen        TEXT    NOT NULL,
    receiver_present INTEGER NOT NULL,
    storage_full     INTEGER NOT NULL,
    alert            TEXT    NOT NULL DEFAULT 'ok'
);
