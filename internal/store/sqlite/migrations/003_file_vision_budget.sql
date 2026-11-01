-- Per-file vision-model budget: 3 attempts or 2 minutes (research R7).
ALTER TABLE capture_files ADD COLUMN vision_attempts   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE capture_files ADD COLUMN vision_started_at INTEGER;
