-- A named task source may name a reply adapter instead of a Slack thread: a
-- configured command that delivers task reply text somewhere else. A source
-- has at most one of the two; existing sources get no adapter.
ALTER TABLE task_sources ADD COLUMN reply_adapter TEXT CHECK (
  reply_adapter IS NULL OR (
    length(reply_adapter) BETWEEN 1 AND 32
    AND reply_adapter NOT GLOB '*[^a-z0-9-]*'
    AND reply_channel_id IS NULL
  )
);

-- One durable intent per task reply delivered through an adapter, with the
-- same lifecycle as a Slack effect. The reply text is not kept: only its
-- SHA-256, so a request replay can be checked against it.
CREATE TABLE reply_adapter_effects (
  effect_id TEXT PRIMARY KEY CHECK (
    length(effect_id) = 22
    AND effect_id NOT GLOB '*[^A-Za-z0-9_-]*'
  ),
  related_task_id TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE RESTRICT,
  adapter TEXT NOT NULL CHECK (
    length(adapter) BETWEEN 1 AND 32
    AND adapter NOT GLOB '*[^a-z0-9-]*'
  ),
  payload_hash TEXT NOT NULL CHECK (
    length(payload_hash) = 64
    AND payload_hash NOT GLOB '*[^0-9a-f]*'
  ),
  state TEXT NOT NULL CHECK (state IN ('prepared', 'submitted', 'confirmed', 'failed', 'ambiguous')),
  attempt_count INTEGER NOT NULL CHECK (attempt_count IN (0, 1)),
  exit_code INTEGER CHECK (exit_code IS NULL OR exit_code BETWEEN 0 AND 255),
  diagnostic_code TEXT CHECK (
    diagnostic_code IS NULL OR (
      length(diagnostic_code) BETWEEN 1 AND 64
      AND diagnostic_code NOT GLOB '*[^a-z0-9_]*'
    )
  ),
  prepared_at_us INTEGER NOT NULL CHECK (prepared_at_us > 0),
  submitted_at_us INTEGER CHECK (submitted_at_us IS NULL OR submitted_at_us >= prepared_at_us),
  completed_at_us INTEGER CHECK (completed_at_us IS NULL OR completed_at_us >= submitted_at_us),
  updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= prepared_at_us),
  CHECK ((state = 'prepared') = (attempt_count = 0)),
  CHECK ((state = 'prepared') = (submitted_at_us IS NULL)),
  CHECK ((state IN ('confirmed', 'failed', 'ambiguous')) = (completed_at_us IS NOT NULL))
) STRICT;

CREATE INDEX reply_adapter_effects_task_idx ON reply_adapter_effects(related_task_id, prepared_at_us, effect_id);
CREATE INDEX reply_adapter_effects_state_idx ON reply_adapter_effects(state, updated_at_us, effect_id);
