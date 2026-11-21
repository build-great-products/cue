# Cue v1 product contract

**Status:** normative contract for the current integrated v1 implementation

“Must”, “must not”, “should”, and “may” express requirements in descending order of strength. [`SYSTEM_MODEL.md`](SYSTEM_MODEL.md) explains the design; the [README](../README.md) and runbooks explain operation.

The former numbered phases were implementation milestones. They are not product modes and are not part of this contract.

## 1. Scope

Cue must provide:

- one foreground Go gateway and short-lived CLI clients;
- one Slack Socket Mode workspace connection that admits work from every channel the bot is a member of;
- admission of direct bot mentions and tracked-thread replies from every human member of that workspace;
- local and Slack-origin tasks with explicit state transitions, and local tasks labelled by a named external source with durable key deduplication and an optional Slack thread or reply adapter to reply through;
- commit-before-ack Slack source deduplication;
- a harness-neutral task event stream (`task watch`) with a durable resume cursor, which is the only way work reaches an agent;
- explicit Slack reads, writes, reactions, and file operations;
- configured reply adapters (commands that deliver task replies elsewhere) with durable, honest outcomes, and configured event hooks (commands told about Slack message and reaction events);
- Slack lifecycle reactions that project task state;
- durable Slack intents with honest uncertainty;
- temporary, bounded file transfer without permanent byte storage;
- validated, atomic configuration reload;
- a private versioned local protocol; and
- explicit inspection, retry, and metadata purge operations.

Cue does not need to start, supervise, or prompt an agent, support more than one Slack workspace or direct messages, retain historical task text, archive file bytes, infer task state from conversation, retry uncertain external work automatically, provide a browser UI, or guarantee end-to-end exactly-once execution.

## 2. Authority and persistence

The gateway is the only normal SQLite writer. CLI commands other than setup and offline-safe configuration edits must use the gateway.

SQLite is authoritative for coordination state: tasks, source events, effects, files, and request outcomes. Slack is an external persistence domain and cannot share a transaction with SQLite.

The gateway upgrades an intact older ledger atomically on open and refuses a newer or altered one. Upgrades are one-way. Schema version 8 drops the table of the removed Pi delivery feature without archiving it. Schema version 9 adds the nullable `parent_task_id` follow-up link to Slack task sources and thread lookup indexes; existing sources get no parent. Schema version 10 drops the `trusted_users` table of the removed trusted-user feature without archiving it. Schema version 11 adds the `task_sources` table for named task sources (section 4b); existing tasks get no named source. Schema version 12 adds the nullable Slack reply target columns `reply_channel_id` and `reply_thread_ts` to `task_sources`; existing sources get no reply target. Schema version 13 adds the nullable `reply_adapter` column to `task_sources` (a source has at most one of a Slack reply target and a reply adapter) and the `reply_adapter_effects` table (section 6a); existing sources get no adapter.

Every gateway mutation must carry a client-generated request ID. A replay with the same operation and canonical input must return the original typed outcome without repeating the mutation. Reuse for different input must fail as a conflict. Stored request outcomes must not become a hidden archive of task text or credentials.

## 3. Configuration and local security

Cue uses fixed XDG-derived paths:

```text
$XDG_CONFIG_HOME/cue/config.toml
$XDG_STATE_HOME/cue/cue.sqlite3
$XDG_STATE_HOME/cue/control.sock
$XDG_STATE_HOME/cue/tmp/
```

Fallbacks are `~/.config` and `~/.local/state`. Configuration and state roots must be owner-only directories. Sensitive files must be owner-only regular files; unsafe ownership, modes, symlinks, hard links, or escaped state paths must fail closed.

`config.toml` contains Slack app and bot tokens and all scalar limits/timeouts. It configures no channels and no users. Workspace/team and bot-user IDs are derived and verified through Slack authentication, not configured manually. Tokens must be set through the private config file, redacted from output, and excluded from SQLite, logs, and protocol results.

`config.toml` may also define reply adapters and event hooks as tables keyed by a name of 1 to 32 characters of `a-z`, `0-9`, and `-`, at most 8 of each:

```toml
[reply_adapters.NAME]
command = "/absolute/path"   # required
timeout = "30s"              # optional Go duration, 1s to 5m, default 30s

[event_hooks.NAME]
command = "/absolute/path"   # required
channels = ["C0123ABCD"]     # optional C/G channel IDs; empty or absent means every channel
timeout = "10s"              # optional Go duration, 1s to 1m, default 10s
```

A `command` must be an absolute, clean path to a regular, singly linked file owned by the effective user, executable by its owner and not writable by group or others, and every directory above it must be symlink-free and not group- or world-writable. The file itself must not be a symlink. These checks run whenever the configuration is loaded for use (startup and `gateway reload`), so a command that fails them stops the gateway from starting and fails a reload, which keeps the prior configuration. Commands run with no shell and no arguments, with the gateway's environment, in their own process group; on timeout the whole group is killed. Their stdout is discarded; the start of their stderr (at most 2048 bytes) is logged when they fail, so a command must not write message text or credentials to stderr. Unknown keys inside these tables are rejected like any unknown key. `config set` does not edit them; edit `config.toml` and reload. `config show` and status show command paths, which are not secret.

Keys of removed features must be rejected with guidance to remove them: `delivery_lease`, `leader_heartbeat`, and `leader_expiry` (Pi delivery), `slack_channel_id` and `slack_channel_allowlist` (channel restrictions), and `trusted_user_ids` (trusted users). The gateway must refuse to start, and a reload must fail, while any of them is present, and the error must name every unknown or obsolete key found, not only the first.

`config set` must update one known non-secret value atomically while preserving unrelated comments and formatting. `config secret set` must accept only known Slack token fields and avoid printing their values. Editing stages a candidate; it does not change the running snapshot.

On startup and `gateway reload`, the gateway must validate the complete candidate and verify Slack identity and required scopes for both public and private channels before activation. No channel is verified up front. Activation is all-or-nothing. Failed reload must leave the prior runtime and connection usable.

## 4. Tasks and Slack admission

A task has a unique opaque ID, positive monotonic content version, current text, `open`/`working`/`done` state, timestamps, and a local or Slack source. A local task may also carry a named source (section 4b). Previous text is not retained.

Every task also carries a durable change sequence and the kind of its latest change (`created`, `edited`, or `state`). The ledger must assign each creation, content-version change, and state change the next value of one monotonic counter inside the writing transaction, so sequence order equals commit order. Sequence values are never reused, including after purge. No-op edits and same-state transitions must not advance the counter. Only the latest change of each task is retained; it is not an event history.

Creation yields `open`. Explicit transitions may move between all three states; same-state transitions are idempotent. Entering `done` sets `done_at`; leaving it clears `done_at`. State-only changes do not increment content version. For Slack-origin tasks, each committed state change replaces the lifecycle reaction (section 4a); state transitions do not otherwise post or alter Slack content.

An initial Slack message is admitted only when all of these hold:

- it belongs to the authenticated workspace and to a `C...` or `G...` channel (any channel Slack delivers events for, which is every channel the bot is a member of);
- its sender is a human: it has no bot ID, its sender is not the authenticated bot user, and its subtype is empty, `thread_broadcast`, or `file_share`;
- its sender is a member of the authenticated workspace (see below); and
- its current text literally mentions the authenticated bot user.

There is no user or channel allowlist: every member of the workspace may send work.

**External senders.** A sender is not a member when the event names a sender workspace that differs from the authenticated team: the first non-empty of the event's `user_team`, `user_profile.team`, and `team` fields (for an edit, those of the changed message). This is how Slack Connect external users appear in shared channels. Such a message is `ignored_workspace`, whether it mentions the bot or replies in a tracked thread, and gets no reaction. The check uses only the delivered payload; the gateway must not call `users.info` per message. A message whose event names no sender workspace is treated as a member's. A malformed sender workspace ID makes the event `malformed`.

Every task records its channel. The same message timestamp in two channels is two distinct sources.

A new message (not an edit) is also admitted without a mention as a **follow-up** when all of these hold:

- it is a reply (its thread timestamp differs from its message timestamp) in an admissible channel;
- its thread is tracked: an admitted task's source is in that thread, or a confirmed `send` or `file_upload` effect posted the thread's root or a reply in it;
- its sender is a human member of the workspace, as for a mention (any member, not only the thread's original requester); and
- it has text or attached files.

A follow-up is a new task, never a change to an earlier one. Its source records the thread root as `thread_ts` and `parent_task_id`: the task admitted from the root message, else the related task of a confirmed root `send`, else the earliest task in the thread, else the related task of the earliest confirmed post in the thread, else none. `parent_task_id` is provenance only: it is not a foreign key and may name a purged task. Any admitted reply, mentioned or not, gets the same link. A message that neither mentions the bot nor is a follow-up is `ignored_unmentioned` and gets no reaction. Messages from the bot, or with a subtype other than `thread_broadcast` or `file_share`, are `ignored_bot`. An attachment-only follow-up has the task text `(attachment without message text)`. Admission, deduplication, commit-before-ack, edits, and deletion are otherwise exactly as for mentioned messages.

For each identifiable envelope, the gateway must deduplicate and commit the admission result before acknowledging Slack. A commit failure must not be acknowledged. Cue makes no receipt claim while disconnected from Socket Mode.

An admitted message creates at most one task. A meaningful newer source edit replaces current text/attachment metadata and increments version. Stale edits must not roll content backward. Source deletion preserves task text and state while recording deletion metadata. Incoming attachments are metadata-only until explicitly retrieved.

An edit of an admitted source whose sender is not the source's author is ignored and recorded with the result value `ignored_untrusted`; the name is retained from the removed trusted-user feature because the ledger fixes the set of result values. The result `ignored_channel` is no longer produced; it remains only on events recorded by releases that had a channel allowlist, so their redelivery replays it. No admission outcome other than a new task adds a reaction.

**Reactions and other events.** `reaction_added` and `reaction_removed` events on messages are never admitted, never create, change, or relate to a task, and leave no ledger record: the gateway reports them to event hooks (section 4c) and acknowledges them. This deliberately departs from commit-before-ack, because there is nothing to commit; a redelivery is dropped by the hooks' bounded in-memory deduplication, best-effort. Reactions on anything but a message, and every event type other than `app_mention`, `message`, `reaction_added`, and `reaction_removed`, are acknowledged and dropped on receipt, with no record; they must not be treated as a socket failure.

## 4a. Lifecycle reactions

A Slack-origin task's source message carries one lifecycle reaction that projects its current state:

| State | Reaction |
| --- | --- |
| `open` | `hourglass_flowing_sand` |
| `working` | `eyes` |
| `done` | none |

Admission prepares the `open` reaction before acknowledgement and submits it afterwards. Each later committed state change prepares, in order, an add of the new state's reaction and then a removal of the prior state's reaction, each under a durable intent keyed by that transition's state event, so a reopened task gets fresh intents. The removal is submitted only after the add is confirmed; a transition into `done` has no add and only removes. Content edits and source deletion do not change the reaction. The replacement is explicitly not atomic. A failed or ambiguous submission remains inspectable and is never automatically retried. Adding a reaction that is already present, or removing one that is absent, is a confirmed outcome because the requested end state holds.

On startup the gateway re-prepares any missing lifecycle intent for the latest transition of each `open` or `working` Slack task before effect recovery runs, so a crash between a transition commit and its projection converges. `done` tasks are not reconciled at startup.

## 4b. Named task sources

A feeder is any program that turns events from another system into tasks. It creates them with `task create --source NAME`, optionally with `--key KEY`, `--url URL`, and `--reply-slack CHANNEL:THREAD_TS`:

- `NAME` is 1 to 32 characters of `a-z`, `0-9`, and `-`. `local` and `slack` are reserved and must be rejected.
- `KEY` is 1 to 256 bytes of printable ASCII without spaces (`!` to `~`). It identifies the external event, for example `tracker:item-12:comment-34`.
- `URL` is an `http://` or `https://` link of at most 2048 bytes of printable ASCII without spaces, to the item the task came from.
- `CHANNEL:THREAD_TS` is a Slack reply target: a `C...` or `G...` channel ID and the timestamp of the thread root to reply in (`SECONDS.MICROS`, at most 32 bytes), separated by one colon. It names no workspace; it always means the active one.
- `--reply-via NAME` names a reply adapter (section 6a) instead of a Slack thread, with the syntax of a source name. A task has `--reply-slack` or `--reply-via`, not both: the CLI, the gateway, and the ledger schema each refuse both. The gateway refuses (`reply_adapter_not_configured`, usage, exit 2) an adapter its active configuration does not define, before recording anything, so a feeder can fall back to another reply path.
- `--key`, `--url`, `--reply-slack`, and `--reply-via` require `--source`. Invalid values fail locally with a usage error and are also refused by the gateway and by the ledger schema.

The task is an ordinary local task (`source_kind` is `local`): it has no Slack source, no lifecycle reaction, and is not a Slack-origin task. Its named source is immutable provenance stored in `task_sources` and shown as the `source` object (`name`, `key`, `url`, `reply_slack` with `channel_id` and `thread_ts`, and `reply_via`) in task DTOs, as the source name, key, URL, `reply=CHANNEL:THREAD_TS`, and `via=NAME` in human task cards, and on watch lines (section 5). Without a reply target or adapter it is not replied to with `task reply` (which fails as not found) and is finished with `task done`.

**Deduplication by key.** At most one retained task has a given source name and key. When `task create` names a source and key that a retained task already has, in any state, the gateway must create nothing and change nothing, and must return that task's mutation result with `changed: false`; the new text, URL, reply target, and reply adapter are ignored. A create that makes a new task returns `changed: true`. Keys are scoped to their source name. Without a key every create makes a new task. The key is released only when metadata purge removes its task (section 8), after which the same key creates a new task.

Request IDs keep their general meaning (section 2) and are not a substitute for keys: a request ID is one attempt of one exact input, its record expires, and its replay reports the original outcome. A feeder should therefore give every create a fresh request ID (the CLI generates one when omitted) and rely on the key for deduplication across runs and restarts.

The `task_sources_v1` capability advertises this feature. A client must see it before sending `source`, `key`, or `url`, and otherwise fail with the unsupported category (exit 9).

**Reply targets.** A local task whose named source has a reply target is answered with `task reply` exactly like a Slack-origin task (section 6), except that the destination is the reply target in the active workspace: the reply is a `send` effect into that channel and thread, related to the task. Because the target names no workspace, preparing the reply needs the active Slack runtime; while it is unavailable `task reply` fails with `slack_unavailable` and records nothing. The channel must be one the bot is a member of: the reply preflights the thread root in that channel like any threaded write, so a channel the bot cannot read, or a missing root, fails before any submission, and Slack enforces membership on the post itself. Once confirmed, the reply makes the thread tracked like any other confirmed `send` (section 4), so later human replies there are admitted as follow-ups. The `task_reply_targets_v1` capability advertises reply targets; a client must see it before sending `reply_slack`, and otherwise fail with the unsupported category (exit 9). The `task_reply_adapters_v1` capability advertises reply adapters; a client must see it before sending `reply_via`, and otherwise fail the same way, so a new CLI refuses an old gateway.

## 4c. Event hooks

Event hooks (section 3) are notification only. For every Slack message event (new, `message_changed`, `message_deleted`, including the bot's own and those Cue ignores for admission) after its admission result is committed and acknowledged, except a duplicate redelivery or a malformed event, and for every reaction event on a message, the gateway queues one notification for each hook whose `channels` include the event's channel. Hooks must never change what is admitted, deduplicated, or acknowledged.

The queue holds at most 256 events and one worker runs hooks one at a time, in name order for each event, off the Socket Mode path. Queueing never blocks: with no hook for the channel nothing is queued, and with a full queue the event is dropped and logged. Event IDs already queued are dropped (the last 4096 are remembered in memory). A hook that times out, ends by a signal, or exits non-zero is counted and logged as failed and never retried. Events queued when the gateway stops are lost. `gateway status` reports `event_hooks` counts (`configured`, `queued`, `delivered`, `failed`, `dropped`, `duplicates`) since start.

A hook reads one JSON object on stdin:

```json
{"version":1,"kind":"reaction_added","event_id":"Ev0123","team_id":"T0123","channel":"C0123","ts":"1700000000.000100","thread_ts":"","user":"U0123","bot_id":"","reaction":"+1"}
```

Empty fields are omitted. `kind` is `app_mention`, `message`, `message_changed`, `message_deleted`, `reaction_added`, or `reaction_removed`. `ts` is the message the event is about (the new, changed, or deleted message, or the message reacted to). `thread_ts` is the thread root of a reply and is absent for a top-level message and for every reaction, because Slack does not say which thread a reacted message is in. `user` is the author of a message or the person who reacted; `bot_id` is set for a bot's message; `reaction` is the emoji name of a reaction event. Message text and files are never included: a hook that needs them reads them from Slack.

## 5. Task watch

`task watch` is a read-only, long-running client of the `task_changes_v1` capability. It must poll the gateway over the private local protocol and write each selected task change to stdout as exactly one complete line, flushed per line, until interrupted. Stdout must contain only events. Reconnect and other diagnostics go to stderr.

Each event is the task's current snapshot at its latest change sequence. Changes committed between polls (or while no watcher runs) are coalesced: a watcher observes the newest version and state, not every intermediate value.

The default line is:

```text
cursor=SEQ change=created|edited|state task=TASK_ID version=N state=open|working|done source=local|slack|NAME channel=CHANNEL_ID thread=THREAD_TS parent=TASK_ID key=KEY url=URL reply=CHANNEL_ID:THREAD_TS via=ADAPTER text=ESCAPED
```

`source` is the task's named source when it has one (section 4b), else `local` or `slack`. `channel` and `thread` are the Slack channel and reply thread root of a Slack task and are empty otherwise. `parent` is the source's `parent_task_id`, empty when absent. `key` and `url` are the named source's key and URL, empty when absent; they are printable ASCII without spaces, so they need no escaping. `reply` is the named source's Slack reply target, empty when absent; `via` is its reply adapter, empty when absent; `channel` and `thread` stay empty for such a task because it is not a Slack task. `text` is the last field and is never truncated. It escapes backslash as `\\`, newline as `\n`, carriage return as `\r`, tab as `\t`, and every other control character and U+2028/U+2029 as `\uXXXX`; other printable Unicode is retained. With `--json`, each line is one `TaskChangeDTO` object (`change_seq`, `change`, and a `task` with the `TaskDTO` shape including `slack_source` for Slack tasks), and the cursor is `change_seq`.

By default a live event is emitted when a task is created, when its content version changes, or when it enters `open`. `--state-changes` also emits transitions to `working` and `done`. Filtered changes still advance the cursor.

The cursor is the change sequence. `--since CURSOR` resumes after that value. `--cursor-file PATH` reads the cursor at startup if the file exists and atomically replaces it after each processed change, only after that change's line was written; a crash can therefore repeat at most the last line and never skips one. Consumers that need exactly-once handling must deduplicate on `task`, `version`, and `state`. A cursor newer than the ledger head fails with `cursor_ahead` (exit 4). Without a cursor, the watcher first emits every currently `open` task, then streams live changes; `--no-backlog` starts at the current head instead.

When the gateway is unavailable, the watcher must retry with capped exponential backoff, report each failure and the eventual reconnection on stderr, and resume from its in-memory cursor. Typed refusals (unsupported capability, invalid or ahead cursor) end the command with their normal exit category. `--timeout` bounds each dial/handshake and each poll separately. A `--json` fatal error is the shared runtime error object on stdout, which callers distinguish from events by its `code` field.

Watching must not change task state or add Slack reactions. A watcher should claim work explicitly, for example with `task working` (which moves the lifecycle reaction to `eyes`), and complete it with `task reply --done` or `task done`. Several watchers may observe the same tasks; coordination between them is the caller's responsibility.

## 6. Slack operations and effects

Every Slack command targets the active workspace and exactly one channel. The channel is the explicit `--channel`, else the related `--task`'s source channel; otherwise the command fails with `channel_required`. A target that is not a `C`/`G` ID, or a durable effect or source in a workspace other than the active one, fails with `invalid_channel` before any intent is recorded. No operation accepts an alternate workspace. Slack enforces bot membership. Targeted writes preflight the target message in that channel. Each effect records its channel, and recovery and retry use the effect's own channel, which must still be in the active workspace. Clients must see `slack_channels_v1` before sending a channel.

Reads are bounded and do not create effects or change task state. Writes commit an exact effect intent before their single network submission.

Effect states are:

```text
prepared → submitted → confirmed
                       ├────→ failed
                       └────→ ambiguous
```

Conclusive rejection is `failed`. Timeout, disconnect, cancellation, or malformed completion after submission is `ambiguous`. Submitted work without a conclusive result must not be retried automatically, including after restart.

`effect retry` creates a linked successor rather than rewriting its predecessor. Retrying an ambiguous effect requires explicit duplicate-risk confirmation. Task relationships are audit metadata only.

`task reply` derives its destination from a durable Slack task source, or from the reply target or reply adapter of a local task's named source (section 4b). With `--done`, it must confirm the reply before applying the local completion. Interrupted replay with the same request ID must not post twice, and failures/ambiguity must leave the task incomplete.

## 6a. Reply adapters

A reply adapter delivers `task reply` text for a local task whose named source names it (`--reply-via NAME`), instead of Slack. `task reply` on such a task:

1. fails with `reply_adapter_not_configured` (usage, exit 2) if the active configuration no longer defines the adapter, recording nothing;
2. records a `prepared` reply adapter effect under the request ID, keyed to the task, the adapter, and the SHA-256 of the reply text (the text itself is not stored). A replay with the same request ID and input returns that effect in its current state; other input under the same request ID is a conflict;
3. if the effect is `prepared`, commits it as `submitted` (its one attempt) before the command starts, then runs the command once, detached from the client's cancellation, within the adapter's timeout;
4. records the outcome: exit 0 is `confirmed`; exit 3 is `failed` (the adapter is sure it delivered nothing; diagnostic `adapter_failed`); a command that cannot be started is `failed` (`adapter_not_started`); every other exit code (`adapter_exit`, with the code kept), a signal (`adapter_signaled`), and the timeout (`timeout`) are `ambiguous`;
5. with `--done`, completes the task only after a `confirmed` effect. A `failed` effect fails the command with `reply_adapter_failed` (external failure, exit 7) and an `ambiguous` one with `ambiguous_external_outcome` (exit 8), and both leave the task as it was.

An effect is never run again, by replay, restart, or anything else: Cue retries no ambiguous external work. On startup the gateway marks every effect left `submitted` `ambiguous` (`startup_reconciliation`). To answer again after a failure, the caller replies with a new request ID, aware that an ambiguous reply may already have been delivered. The adapter makes its own delivery idempotent with the effect ID, which is stable for the effect.

The command reads one JSON object on stdin:

```json
{"version":1,"effect_id":"EFFECT_ID","request_id":"REQUEST_ID","adapter":"NAME","task":{"id":"TASK_ID","source":"SOURCE","key":"KEY","url":"URL"},"text":"exact reply text"}
```

`key` and `url` are omitted when the task's source has none. `text` is the exact reply body. The command's stdout is ignored.

The reply result carries `adapter_effect` (`effect_id`, `related_task_id`, `adapter`, `state`, `attempt_count`, `exit_code`, `diagnostic_code`, and timestamps) instead of `effect`. `task context` shows a task's newest adapter effect as `effects.latest_adapter_reply`. `gateway status` reports `reply_adapters` with the configured count, effects by state, and `action_required` (failed plus ambiguous). Adapter effects are not Slack effects: `effect list`, `effect inspect`, and `effect retry` do not show or retry them.

## 7. Files

File records contain identity, direction, untrusted name/MIME metadata, size/hash when known, relationships, transfer state, diagnostics, and timestamps. File bytes and authenticated private transfer URLs must not be durable metadata.

`file get` must fetch an admitted incoming file only on explicit request, enforce configured limits, use a random private temporary file, and compute/validate its identity. Explicit output placement must be atomic and exclusive by default. `--force` may replace only a regular file using race-safe behavior.

Outgoing uploads must accept regular files only and reject symlinks, devices, FIFOs, sockets, unsafe sparse files, oversized files, and files that change during validation/upload. A prepared upload must record size and SHA-256 before submission; changed bytes must conclusively fail rather than upload under the old intent.

Temporary files must use generated owner-only paths under the state root. Expired temporary bytes may be removed automatically. Durable file metadata requires explicit purge.

## 8. Purge and diagnostics

Purge requires an absolute RFC 3339 cutoff and exactly one of `--dry-run` or `--confirm`. Dry-run must not mutate state. Confirmed purge may remove eligible finalized history but must preserve open/working tasks, unresolved effects, live request outcomes, configuration, and every record required by retained state. Removing a task removes its named source row with it, counted with the task, which releases its key. A task with a reply adapter effect that is not `confirmed` before the cutoff is retained; otherwise its adapter effects are removed with it and counted as `reply_adapter_effects`.

`gateway status` must distinguish process health from outcomes requiring operator action and report bounded task, Slack, effect, database, and temporary-file state. `doctor` must be non-destructive: it must not send Slack messages, acknowledge events, or transfer bytes.

Logs and diagnostics must exclude credentials, authorization headers, raw Slack envelopes, and full message bodies. Local paths and ordinary diagnostics are not redacted and may appear in agent context.

## 9. Commands and compatibility

The current command families are:

```text
cue setup
cue config show|set|secret set
cue gateway run|reload|status
cue doctor
cue task create|list|show|context|watch|update|open|working|done|reply
cue slack history|thread|send|file|edit|delete|react|unreact   (each accepts --channel; thread and writes accept --task)
cue effect list|inspect|retry
cue file show|get
cue purge
```

SQLite-backed `task list` and `effect list` accept bounded `--limit`/`--cursor` pages (effects also accept composable `--task`). Tasks are oldest-created-first; effects newest-created-first. Resource cursors are full IDs and a purged anchor returns typed `cursor_expired` conflict guidance to restart without `--cursor`. `slack history` and `slack thread` likewise accept `--limit` and `--cursor`; both default to and cap at 50 messages, preserve Slack's native order, and pass the opaque Slack cursor unchanged.

The private Unix-socket protocol uses bounded 32-bit-length-prefixed JSON frames, a versioned handshake, typed command/results, and capability negotiation. The only client kind is `cli`. Unknown incompatible envelopes or enum values must fail safe. Slow clients must be disconnected without discarding durable work. Same-user authentication is not protection from compromise of that OS account.

Every CLI leaf that contacts the gateway accepts a positive shared `--timeout` duration, defaulting to 10 seconds. For `task watch` it applies to each connection attempt and each poll (section 5); for every other leaf its deadline begins immediately before gateway contact and covers dial, hello, calls, and frame I/O; an earlier application/caller deadline wins. Explicit body stdin and local validation occur before this deadline. Cancellation before a Slack external boundary leaves work unsubmitted; after a boundary, the gateway durably records a confirmed, failed, or ambiguous outcome using a non-cancelled commit context. Timeout, cancellation, or disconnect after submission is ambiguous and is never retried automatically.

Cobra help is the complete command-discovery interface. Running the root or a command branch without a leaf prints help and exits successfully; every leaf documents its syntax, flags, examples, and applicable irreversible or duplicate-risk warnings. Help performs no configuration, gateway, or mutation work. Unknown commands and flags print usage for the relevant command and exit 2. Only unambiguous close command or flag misspellings may be suggested; positional values are never rewritten or suggested. Shell-completion and machine-readable help commands are not exposed.

`task context TASK [--thread-limit N]` is a read-only bounded task workflow view. It includes the current durable task/source attachment metadata, latest reply effect, and at most ten newest non-confirmed related effects and their total. For Slack tasks it makes one best-effort live thread-page read (default 20, maximum 100); `thread.status` is `available`, `unavailable`, or `not_applicable`. An unavailable live thread is a successful partial result with a warning; missing tasks remain not-found. It never downloads files or returns live-reply file metadata, file bytes, private URLs, effect body text, or operator reasons.

`task create` also accepts `--source NAME`, `--key KEY`, `--url URL`, and `--reply-slack CHANNEL:THREAD_TS` or `--reply-via NAME` (section 4b). `task create`, `task update`, `task reply`, `slack send`, and `slack edit` require exactly one text source: `--text`, `--text-file`, or `--stdin`. `--text` is exact literal text and must not decode escapes: a supplied `\\n` is a backslash followed by `n`. `--text-file` and explicit stdin preserve exact multiline UTF-8 bodies; stdin reads its complete bounded body (including trailing newlines) before gateway interaction. Empty, NUL-containing, malformed UTF-8, and over-limit bodies fail locally. This body stdin is independent from `config secret set --stdin` credential input.

For every syntactically valid leaf invocation other than `task watch` whose parsed `--json` flag is true, success and runtime failure write exactly one JSON object followed by one newline to stdout and nothing to stderr. Successful objects retain each command's existing DTO shape. Runtime failures use a shared error object with `code`, `category`, `message`, `retriable`, and optional safe `details`, `progress`, `request_id`, and guidance; doctor gateway failures additionally contain only the completed diagnostic report in `partial_result`. Parse and usage failures remain text on stderr with exit 2.

Without `--json`, each command uses a compact purpose-specific human view: task and Slack reads retain complete multiline text in cards, lists use rows, and mutations display resource state and request IDs. Presentation escapes terminal control characters while retaining printable Unicode; it never changes stored text or JSON output. Color is only enabled for a TTY and is disabled by `NO_COLOR` or `TERM=dumb`.

Stable process exit categories are:

| Code | Meaning |
| --- | --- |
| 0 | success |
| 2 | usage error |
| 3 | not found |
| 4 | conflict or ownership error |
| 5 | unsafe configuration or permissions |
| 6 | gateway unavailable |
| 7 | conclusive external failure |
| 8 | ambiguous outcome |
| 9 | unsupported schema or protocol |

## 10. Release invariants

1. An identifiable Slack envelope is acknowledged only after its admission/deduplication result commits, except a reaction or unused event type, which is never admitted and leaves no record (section 4).
2. One admitted Slack source maps to at most one task.
3. Task state and Slack content remain independent except for the explicit confirmed-reply composition; lifecycle reactions only project state.
4. SQLite is the sole coordination authority.
5. Slack intent commits before submission; possibly submitted work becomes ambiguous and is never automatically retried.
6. Durable records contain file metadata and outcomes, not permanent transfer bytes.
7. Purge preserves live work and unresolved evidence.
8. Cue never starts or owns an agent.
9. Watching tasks never changes task or Slack state.
10. Tokens never enter SQLite, logs, or protocol output.
11. Configuration reload either activates one complete verified snapshot or leaves the previous snapshot active.
12. Event hooks are notification only: they never change admission, deduplication, or acknowledgement, and are never retried.
13. A reply adapter effect is submitted before its command runs and is run at most once.

The automated Go suite (`just check`) is the repeatable release gate. Future work belongs in issues or scoped proposals, not numbered implementation milestones in this specification.
