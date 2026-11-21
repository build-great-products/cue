-- A named task source may carry a Slack thread to reply in: the channel and
-- the thread root timestamp. A local task with a reply target is answered with
-- task reply through the same durable Slack effect path as a Slack-origin
-- task. Both columns are set together or not at all; existing sources get no
-- reply target.
ALTER TABLE task_sources ADD COLUMN reply_channel_id TEXT CHECK (
  reply_channel_id IS NULL OR (
    length(CAST(reply_channel_id AS BLOB)) BETWEEN 2 AND 128
    AND substr(reply_channel_id, 1, 1) IN ('C', 'G')
    AND reply_channel_id NOT GLOB '*[^A-Z0-9]*'
  )
);

ALTER TABLE task_sources ADD COLUMN reply_thread_ts TEXT CHECK (
  (reply_thread_ts IS NULL) = (reply_channel_id IS NULL)
  AND (
    reply_thread_ts IS NULL OR (
      length(reply_thread_ts) BETWEEN 8 AND 32
      AND instr(reply_thread_ts, '.') > 1
      AND length(substr(reply_thread_ts, instr(reply_thread_ts, '.') + 1)) = 6
      AND substr(reply_thread_ts, 1, instr(reply_thread_ts, '.') - 1) NOT GLOB '*[^0-9]*'
      AND substr(reply_thread_ts, instr(reply_thread_ts, '.') + 1) NOT GLOB '*[^0-9]*'
    )
  )
);
