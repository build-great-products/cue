-- A named task source lets an external feeder (a script that turns events
-- from some other system into tasks) label its tasks, deduplicate them by an
-- external key, and link back to the original item. The task itself remains
-- a local task (source_kind 'local'); this row only adds provenance. The
-- names 'local' and 'slack' are reserved for the built-in sources.
CREATE TABLE task_sources (
  task_id TEXT PRIMARY KEY REFERENCES tasks(task_id) ON DELETE RESTRICT,
  source_name TEXT NOT NULL CHECK (
    length(source_name) BETWEEN 1 AND 32
    AND source_name NOT GLOB '*[^a-z0-9-]*'
    AND source_name NOT IN ('local','slack')
  ),
  external_key TEXT CHECK (
    external_key IS NULL OR (
      length(CAST(external_key AS BLOB)) BETWEEN 1 AND 256
      AND external_key NOT GLOB '*[^!-~]*'
    )
  ),
  url TEXT CHECK (
    url IS NULL OR (
      length(CAST(url AS BLOB)) BETWEEN 1 AND 2048
      AND url NOT GLOB '*[^!-~]*'
      AND (url GLOB 'https://?*' OR url GLOB 'http://?*')
    )
  ),
  created_at INTEGER NOT NULL CHECK (created_at > 0)
) STRICT;

CREATE UNIQUE INDEX task_sources_key_idx
  ON task_sources(source_name, external_key)
  WHERE external_key IS NOT NULL;
