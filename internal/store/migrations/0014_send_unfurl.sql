-- A send effect records whether Slack may unfurl its links and media. Cue
-- turns unfurling off unless the caller asks for it, so the flag is part of
-- the exact external payload. Only send effects carry it; every other kind
-- keeps NULL. Sends recorded before this migration were posted without
-- unfurl parameters, which is Slack's own default, so they record 1. SQLite
-- checks a new column's CHECK against existing rows, so the constraint admits
-- NULL until the backfill below; the gateway always writes 0 or 1 for a send.
ALTER TABLE slack_effects ADD COLUMN unfurl INTEGER CHECK (
  unfurl IS NULL OR (kind = 'send' AND unfurl IN (0, 1))
);

UPDATE slack_effects SET unfurl = 1 WHERE kind = 'send';
