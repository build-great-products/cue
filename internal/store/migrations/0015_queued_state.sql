-- A task may be queued: promised work that the agent will pick up later. The
-- state CHECKs of tasks and task_state_events admit 'queued', which SQLite can
-- only change by rebuilding both tables. The gateway runs this migration with
-- foreign key enforcement off (SQLite cannot change that inside a
-- transaction) and checks every foreign key before it commits. With
-- legacy_alter_table on as well, renaming a table leaves the REFERENCES
-- clauses of its children naming "tasks", so they bind to the rebuilt table.
-- The new tables keep the column order, so rows copy unchanged, and the
-- change-sequence triggers are recreated only after the copy, so every
-- change_seq is preserved.
PRAGMA legacy_alter_table=ON;

ALTER TABLE tasks RENAME TO tasks_v14;

CREATE TABLE tasks (
  task_id TEXT PRIMARY KEY,
  version INTEGER NOT NULL CHECK (version >= 1),
  state TEXT NOT NULL CHECK (state IN ('queued','open','working','done')),
  text TEXT NOT NULL CHECK (length(text) > 0 AND instr(text, char(0)) = 0),
  source_kind TEXT NOT NULL CHECK (source_kind IN ('local','slack')),
  created_at INTEGER NOT NULL CHECK (created_at > 0),
  updated_at INTEGER NOT NULL CHECK (updated_at >= created_at),
  done_at INTEGER,
  change_seq INTEGER NOT NULL DEFAULT 0 CHECK (change_seq >= 0),
  change_kind TEXT NOT NULL DEFAULT 'created' CHECK (change_kind IN ('created','edited','state')),
  CHECK ((state = 'done' AND done_at IS NOT NULL) OR (state <> 'done' AND done_at IS NULL))
) STRICT;

INSERT INTO tasks SELECT * FROM tasks_v14 ORDER BY rowid;
DROP TABLE tasks_v14;

CREATE INDEX tasks_state_updated_idx ON tasks(state, updated_at, task_id);
CREATE UNIQUE INDEX tasks_change_seq_idx ON tasks(change_seq);

CREATE TRIGGER tasks_change_after_insert AFTER INSERT ON tasks
BEGIN
  UPDATE task_change_sequence SET last_seq = last_seq + 1 WHERE singleton = 1;
  UPDATE tasks SET change_seq = (SELECT last_seq FROM task_change_sequence WHERE singleton = 1), change_kind = 'created' WHERE task_id = NEW.task_id;
END;

CREATE TRIGGER tasks_change_after_update AFTER UPDATE OF version, state ON tasks
WHEN NEW.version <> OLD.version OR NEW.state <> OLD.state
BEGIN
  UPDATE task_change_sequence SET last_seq = last_seq + 1 WHERE singleton = 1;
  UPDATE tasks SET change_seq = (SELECT last_seq FROM task_change_sequence WHERE singleton = 1), change_kind = CASE WHEN NEW.version <> OLD.version THEN 'edited' ELSE 'state' END WHERE task_id = NEW.task_id;
END;

ALTER TABLE task_state_events RENAME TO task_state_events_v14;

CREATE TABLE task_state_events (
  event_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE RESTRICT,
  prior_state TEXT NOT NULL CHECK (prior_state IN ('queued','open','working','done')),
  new_state TEXT NOT NULL CHECK (new_state IN ('queued','open','working','done')),
  actor TEXT NOT NULL CHECK (length(actor) BETWEEN 1 AND 128),
  note TEXT CHECK (note IS NULL OR length(note) <= 4096),
  request_id TEXT NOT NULL UNIQUE,
  committed_at INTEGER NOT NULL CHECK (committed_at > 0),
  CHECK (prior_state <> new_state)
) STRICT;

-- rowid order is commit order, which LatestStateEvent relies on for ties.
INSERT INTO task_state_events SELECT * FROM task_state_events_v14 ORDER BY rowid;
DROP TABLE task_state_events_v14;

CREATE INDEX task_state_events_task_committed_idx ON task_state_events(task_id, committed_at, event_id);

PRAGMA legacy_alter_table=OFF;
