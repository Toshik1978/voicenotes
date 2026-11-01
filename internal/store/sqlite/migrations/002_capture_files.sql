-- One capture holds many files (data-model.md "Capture file", research R10).
-- Each 001 audio capture keeps its file as one audio row at position 0, so
-- queued captures and their local audio survive the upgrade.
CREATE TABLE capture_files (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    capture_id        INTEGER NOT NULL REFERENCES captures (id) ON DELETE CASCADE,
    position          INTEGER NOT NULL,
    source            TEXT    NOT NULL,
    source_chat_id    INTEGER NOT NULL,
    source_message_id INTEGER NOT NULL,
    kind              TEXT    NOT NULL,
    file_id           TEXT,
    mime_type         TEXT,
    file_name         TEXT,
    file_size         INTEGER,
    local_path        TEXT,
    state             TEXT    NOT NULL,
    text              TEXT,
    text_source       TEXT,
    language          TEXT,
    failure           TEXT,
    timeouts          INTEGER NOT NULL DEFAULT 0,
    attached          INTEGER NOT NULL DEFAULT 0,
    attach_failure    TEXT,
    UNIQUE (source, source_chat_id, source_message_id),
    UNIQUE (capture_id, position)
);

-- Past transcribe the transcript is already in captures.text: the file is done.
INSERT INTO capture_files (
    capture_id, position, source, source_chat_id, source_message_id, kind,
    file_id, mime_type, file_size, local_path, state)
SELECT id, 0, source, source_chat_id, source_message_id, 'audio',
       file_id, mime_type, file_size, audio_path,
       CASE WHEN step IN ('download', 'transcribe') THEN 'pending' ELSE 'done' END
FROM captures
WHERE COALESCE(file_id, '') != '' OR COALESCE(audio_path, '') != ''
ORDER BY id;

UPDATE captures SET step = 'extract' WHERE step = 'transcribe';

ALTER TABLE captures ADD COLUMN album_id TEXT;

ALTER TABLE captures DROP COLUMN file_id;
ALTER TABLE captures DROP COLUMN file_size;
ALTER TABLE captures DROP COLUMN mime_type;
ALTER TABLE captures DROP COLUMN audio_path;
