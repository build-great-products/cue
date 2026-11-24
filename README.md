# Cue

**Durable Slack work for a long-running agent session.**

Cue connects the Slack channels its bot is a member of to an agent session such as Claude Code. Eligible Slack mentions and local requests become durable tasks in SQLite. The agent follows them with `cue task watch`, which prints one line per task event, and handles each task with explicit CLI commands.

Cue does not start, supervise, or prompt the agent. A person (or a supervisor) runs the agent and points its background monitor at `task watch`.

```text
Slack ───────┐
             ├──> foreground Go gateway ───> SQLite
Go CLI ──────┘              │
                            │ private local protocol
                            ▼
                 cue task watch (one line per event)
                            │
                            ▼
                     agent session
```

## What is implemented

The current v1 workflow includes:

- single-workspace Slack Socket Mode admission from every channel the bot is a member of, for every member of that workspace;
- local and Slack-origin tasks with `open`, `working`, and `done` states;
- tasks from other systems, labelled with a source name, deduplicated by an external key, and optionally answered in a Slack thread (see [Feeding tasks from other systems](#feeding-tasks-from-other-systems));
- `task watch`, a one-line-per-event task stream with a durable resume cursor;
- `status`, a redacted JSON snapshot of the agent session, its child sessions in tmux, and the inbox and queue, for a dashboard (see [Agent status snapshot](#agent-status-snapshot));
- explicit Slack reads, replies, edits, deletes, reactions, and file uploads;
- configurable formatting of every outgoing message, which links `#1234` to its pull request by default (see [Message formatting](#message-formatting));
- Slack lifecycle reactions that mirror task state;
- lazy incoming file download and private temporary-file handling;
- durable ambiguous outcomes that are never retried automatically; and
- explicit metadata purge with dry-run and confirmation modes.

## Install and configure

Requirements are Go and `just`.

```sh
just build
./bin/cue setup
./bin/cue config secret set slack_app_token
./bin/cue config secret set slack_bot_token
```

Import [`manifest.json`](manifest.json) into a Slack development app, create an app-level Socket Mode token with `connections:write`, install the app in the intended workspace, and invite it to each channel that should give it work. The manifest covers public (`message.channels`, `channels:read`, `channels:history`) and private (`message.groups`, `groups:read`, `groups:history`) channels, and subscribes to `reaction_added` and `reaction_removed` (with `reactions:read`) for event hooks; the gateway verifies every listed scope at startup. Reactions are never work: they only reach event hooks. Do not add administrative, DM, slash-command, interactivity, or broad channel-join permissions.

The gateway admits work from every channel the bot is a member of. Inviting the bot to a channel is how you give that channel's members a way to send it work, and removing it is how you take that away. There is no channel or user allowlist.

Start the gateway in the foreground:

```sh
./bin/cue gateway run
```

Upgrading from Mosscap (the project's previous name): stop the gateway, move `~/.config/mosscap` to `~/.config/cue` and `~/.local/state/mosscap` to `~/.local/state/cue` (or the same names under your `XDG_CONFIG_HOME` and `XDG_STATE_HOME`), rename `mosscap.sqlite3` in the state directory to `cue.sqlite3`, rename `MOSSCAP_SLACK_*_TOKEN` environment variables to `CUE_SLACK_*_TOKEN`, then start `cue gateway run`. The ledger needs no migration. Temporary download files keep the `mosscap-xfr-` prefix because the ledger schema checks it.

Upgrading from an earlier release (schema migrations 8 to 10 run automatically; they are one-way): remove `delivery_lease`, `leader_heartbeat`, `leader_expiry`, `slack_channel_id`, `slack_channel_allowlist`, and `trusted_user_ids` from `config.toml`. The gateway refuses to start while any of them is present and names every one it finds. The ledger migrates on first start: it drops its Pi delivery records and its trusted-user set (`cue trust` no longer exists). Existing tasks and effects already record their channel, so nothing is backfilled. Schema migration 11 adds the `task_sources` table for [named task sources](#feeding-tasks-from-other-systems), and migration 12 adds their optional Slack reply target; both are additive and also one-way, so an older binary refuses the upgraded ledger. Migration 13 adds reply adapters, and migration 14 records on each Slack send whether link previews were allowed (existing sends are recorded as allowed, which is how they were posted); both are additive and one-way too.

## Admission and tasks

Anyone in the workspace may send the bot work. A Slack message becomes a task when all of these hold:

- it arrives from a channel the bot is a member of in the authenticated workspace;
- its sender is a human: not a bot, not the bot itself, and not a `bot_message` or other system subtype;
- its sender is a member of the workspace, not a Slack Connect external user from another organization; and
- it mentions the bot directly, or it is a reply in a thread the bot already tracks (see below).

Messages that neither mention the bot nor reply in a tracked thread are ignored without a reaction. A mention from an external Slack Connect user is ignored the same way, recorded as `ignored_workspace`. Cue reads the sender's workspace from the event Slack delivers (`user_team`, else `user_profile.team`, else `team`) and makes no per-message `users.info` call; an event that names no sender workspace is treated as a member's.

Any member of the workspace can direct a fully capable local agent. Choose its working directory, model, and channels with that in mind: these checks decide who may send work, they are not a sandbox.

### Thread follow-ups

People usually answer in the thread without mentioning the bot again ("Perfect", "it's out of sync"). A new reply from any workspace member in a tracked thread is admitted without a mention, as its own **follow-up task**, whether or not that person started the thread. A thread is tracked when it already holds an admitted task, or when the bot posted its root or a reply in it (a confirmed `slack send`, `task reply`, or upload). Each follow-up:

- is a separate task with the reply's text, so nothing overwrites earlier text, and gets its own lifecycle reaction on the reply;
- records the thread (`thread_ts`, the thread root) and `parent_task_id`, the thread's original task: the task from the root message, else the task the bot's root post was for, else the earliest task in the thread;
- is replied to in the same thread by `task reply`, and `task context` shows the whole thread.

Replies from external Slack Connect users, the bot's own messages, and edits of replies that were never admitted do not create follow-ups. A reply that also mentions the bot is admitted the same way and gets the same link. Edits of an admitted follow-up update it like any other task. A reply with files but no text is admitted with the text `(attachment without message text)`. Replies with the `thread_broadcast` and `file_share` subtypes count as human messages.

Task state remains locally authoritative. Slack-origin tasks also show a lifecycle reaction on their source message that mirrors task state:

| Task state | Reaction |
| --- | --- |
| `open` (queued) | `:hourglass_flowing_sand:` |
| `working` (claimed with `task working`) | `:eyes:` |
| `done` | none |

Each transition adds the new marker before removing the old one. These are separately recorded Slack effects, so an interrupted or ambiguous Slack call remains inspectable rather than being silently retried or atomically replaced. Edits to the source message change the task text, not the reaction.

```sh
cue task create --text "review the release"
cue task list
cue task working AbCdEfGh
cue task show AbCdEfGh --json
# Prefer this bounded durable-plus-live view before handling a Slack task.
cue task context AbCdEfGh --thread-limit 20
cue task reply AbCdEfGh --stdin --done <<'REPLY'
I found the problem.
REPLY
```

`task reply --done` is a resumable composition: it completes the task only after Slack confirms the reply. A plain Slack send never changes task state, and marking a task done never posts to Slack.

For exact multiline or piped bodies, `task create`, `task update`, `task reply`, `slack send`, and `slack edit` accept `--stdin` or `--text-file`; prefer those sources over shell-quoted `--text`. Provide exactly one of `--text`, `--text-file`, or `--stdin`; stdin is read completely (including trailing newlines) before contacting the gateway. `--text` is literal: a typed `\\n` remains a backslash followed by `n`, not a newline.

Every command that contacts the gateway accepts `--timeout DURATION` (default `10s`). The budget starts only immediately before dialing the gateway, covers dial, handshake, request, and frame I/O, and never extends an earlier caller deadline. Stdin and local input validation happen before that budget begins.

Default output is compact human-readable cards, rows, and labelled details; use `--json` for the unchanged command DTOs. Message cards preserve full multiline Unicode text and visibly escape terminal control bytes. Colors require a TTY and are disabled by `NO_COLOR`.

## Watching tasks

`cue task watch` is how an agent receives work. It stays connected to the gateway and prints one line per task event on stdout until interrupted:

```text
cursor=7 change=created task=AbCdEfGhIjKlMnOpQrStUv version=1 state=open source=slack channel=C0BAQM6JEAJ thread=1700000000.000001 parent= key= url= reply= via= text=<@U012BOT> review the release\nthanks
cursor=9 change=created task=ZyXwVuTsRqPoNmLkJiHgFe version=1 state=open source=slack channel=C0BAQM6JEAJ thread=1700000000.000001 parent=AbCdEfGhIjKlMnOpQrStUv key= url= reply= via= text=Perfect
cursor=12 change=created task=QwErTyUiOpAsDfGhJkLzXc version=1 state=open source=tracker channel= thread= parent= key=tracker:item-12:comment-34 url=https://tracker.example.com/items/12 reply=C0TRACKER:1700000500.000100 via= text=New comment on item 12
```

- `change` is `created`, `edited` (content version changed, for example a Slack edit), or `state` (the task re-entered `open`).
- `source` is `slack`, `local`, or the name a feeder gave with `--source`.
- `channel` is the task's Slack channel ID and `thread` its reply thread (the root message timestamp); both are empty for other tasks.
- `parent` is set on a thread follow-up and names the thread's original task; it is empty otherwise.
- `key` and `url` are a feeder's `--key` and `--url`, `reply` its `--reply-slack` thread, and `via` its `--reply-via` adapter; all are empty when absent.
- `text` is the last field, never truncated, with backslash, newlines, tabs, and other control characters escaped so one event is always one line.
- `--json` prints one JSON object per line instead: `{"change_seq":7,"change":"created","task":{...}}`, where `task` is the same DTO as `task show --json`.
- `--state-changes` also reports transitions to `working` and `done`.
- Without a cursor it first prints every task that is currently `open`; `--no-backlog` skips them.
- `--cursor-file PATH` makes restarts resume where the last run stopped. The file is updated after each printed line, so a crash can repeat at most one line and never skips one. `--since CURSOR` resumes from an explicit cursor instead.
- If the gateway restarts, the watcher reconnects with backoff and says so on stderr; stdout carries events only.

Watching is read-only. It changes no task state and adds no Slack reactions, so claim work explicitly with `task working` and finish it with `task reply --done` or `task done`.

### Claude Code

Run the watcher under Claude Code's Monitor tool (or any background runner that turns stdout lines into notifications):

```sh
cue task watch --cursor-file "$HOME/.local/state/cue-watch.cursor"
```

For each notification, the agent claims and handles the task, then replies (Slack-origin tasks and tasks with `reply=`; use `task done` for other local ones). A line with `parent=` is a follow-up in an existing thread: `task context` shows the conversation it answers, and its reply lands in the same thread.

```sh
cue task working AbCdEfGhIjKlMnOpQrStUv
cue task context AbCdEfGhIjKlMnOpQrStUv
cue task reply AbCdEfGhIjKlMnOpQrStUv --stdin --done <<'REPLY'
Done: the release checklist passes.
REPLY
```

## Feeding tasks from other systems

A feeder is a small program that watches some other system (an issue tracker, a CI server, a mailbox) and turns each new event into a task, so the agent follows one `task watch` stream for everything. Feeders use `task create` with a source name and an external key:

```sh
cue task create --source tracker --key tracker:item-12:comment-34 \
  --url https://tracker.example.com/items/12 --text "New comment on item 12" --json
```

- `--source NAME` labels the task: 1 to 32 characters of `a-z`, `0-9`, and `-`; `local` and `slack` are reserved. It appears as `source=NAME` on watch lines, in `task list` and `task show`, and as `"source": {"name", "key", "url"}` in `--json`.
- `--key KEY` makes re-running a feeder safe. If a task with the same source and key already exists, in any state, nothing is created or changed and the existing task comes back with `"changed": false`. A new task has `"changed": true`. Choose a key that identifies the event, not its content: an ID from the other system, never a timestamp of when the feeder ran. Keys are 1 to 256 printable ASCII characters without spaces, and are scoped to their source.
- `--url URL` is an `http(s)` link to the original item, shown with the task.
- `--reply-slack CHANNEL:THREAD_TS` names a Slack thread to answer in, for a system that mirrors its items into Slack threads: a `C...` or `G...` channel ID and the thread root's timestamp, for example `C0TRACKER:1700000500.000100`. It appears as `reply=` on watch lines and in `task show`, and as `"reply_slack": {"channel_id", "thread_ts"}` in `--json`. The bot must be a member of that channel.
- `--reply-via NAME` names a reply adapter instead (see [Reply adapters](#reply-adapters)): `task reply` runs that command rather than posting to Slack. It appears as `via=` on watch lines and `"reply_via"` in `--json`. A task has `--reply-slack` or `--reply-via`, not both, and a gateway without that adapter refuses the create with exit 2, so a feeder can retry with `--reply-slack`.

Do not use `--request-id` for this. A request ID is for retrying one interrupted command with exactly the same input: reusing it with different text fails as a conflict, a replay cannot tell you whether the task already existed, and its record expires after 30 days. Let the CLI generate a fresh request ID for every create and rely on `--key`.

Named-source tasks are local tasks. Finish one with `task done`, or, when it has a reply target, with `task reply --done`: the reply goes into that thread through the same durable path as a Slack task's reply, and the task completes only after Slack confirms it. `task reply` on a local task without a reply target fails as not found. A reply into the thread makes it tracked, so people's later replies there arrive as follow-up tasks. A key is released only if `purge` removes its task.

```sh
out=$(cue task create --source tracker --key "$key" --url "$url" --text "$text" --json)
if [ "$(printf '%s' "$out" | jq -r .result.changed)" = true ]; then echo "new task"; fi
```

## Reply adapters

A reply adapter is a command that delivers `task reply` text somewhere other than Slack, for example into another system that shows the conversation and forwards it to Slack itself. Define it in `config.toml` and reload:

```toml
[reply_adapters.tracker]
command = "/home/me/.local/libexec/cue/tracker-reply"
timeout = "30s"
```

The command must be an absolute path to an executable owned by you, not writable by group or others, in directories that are not group- or world-writable and contain no symlinks; the gateway refuses to start (and a reload fails) otherwise. Tasks created with `--source tracker --reply-via tracker` are answered by running it once per reply, with no shell, and this JSON on stdin:

```json
{"version":1,"effect_id":"…","request_id":"…","adapter":"tracker","task":{"id":"…","source":"tracker","key":"tracker:item-12:comment-34","url":"https://tracker.example.com/items/12"},"text":"exact reply text"}
```

Exit 0 means delivered (`confirmed`, and `--done` completes the task). Exit 3 means certainly not delivered (`failed`, exit 7). Anything else, a signal, or the timeout is `ambiguous` (exit 8). Cue records the reply as submitted before it runs the command and never runs it again for that reply, even after a restart, so make delivery idempotent on `effect_id`. Keep message text out of stderr: its start is logged when the command fails. `task context` shows the latest run as `latest_adapter_reply`, and `gateway status` counts failed and ambiguous runs under `reply_adapters`.

## Event hooks

An event hook is a command Cue runs for every Slack message event (new, edited, deleted, whether or not it became a task) and every reaction added or removed on a message, in the channels it lists, so another system can refresh what it shows at once instead of polling:

```toml
[event_hooks.tracker]
command = "/home/me/.local/libexec/cue/tracker-hook"
channels = ["C0BAQM6JEAJ"]
timeout = "10s"
```

The command gets one JSON object on stdin and no message text:

```json
{"version":1,"kind":"reaction_added","event_id":"Ev0123","team_id":"T0123","channel":"C0BAQM6JEAJ","ts":"1700000000.000100","user":"U0123","reaction":"+1"}
```

`kind` is `app_mention`, `message`, `message_changed`, `message_deleted`, `reaction_added`, or `reaction_removed`; `thread_ts` is set for a reply (never for a reaction, which Slack does not place in a thread). Hooks never change what becomes a task. They run one at a time from a queue of 256 after the event is committed and acknowledged; a full queue drops events, Slack redeliveries are dropped by event ID, and a failed run is logged and not retried. `gateway status` counts them under `event_hooks`. The same path rules as for reply adapters apply.

## Slack commands and channels

Every `cue slack ...` command works in one channel. Name it with `--channel C...`, or pass `--task` to use a Slack task's own channel. With neither, the command fails with `channel_required`. A target that is not a public (`C...`) or private (`G...`) channel in the active workspace fails with `invalid_channel`; Slack itself refuses channels the bot is not a member of.

```sh
cue slack history --channel C0BAQM6JEAJ --limit 20
cue slack thread 1700000000.000001 --task AbCdEfGh
cue slack send --task AbCdEfGh --thread 1700000000.000001 --stdin <<'REPLY'
Long multiline reply.
REPLY
cue slack file --channel C0CC6JQ4A2U --thread 1700000000.000001 --file ./demo.mp4
cue slack react 1700000000.000001 white_check_mark --task AbCdEfGh
```

`task reply` and `task context` always use the task's own channel and thread.

### Link previews

Cue posts without link or media previews: every `slack send` and every `task reply` into Slack sets `unfurl_links=false` and `unfurl_media=false`. Pass `--unfurl` to let Slack unfurl a particular post (Cue then sends neither parameter and Slack decides):

```sh
cue slack send --channel C0BAQM6JEAJ --text "Demo: https://example.com/video" --unfurl
cue task reply AbCdEfGh --text "See https://example.com/notes" --unfurl --done
```

The choice is recorded on the send effect (`"unfurl": false` or `true` in `effect inspect --json`), so an ambiguous post shows what was sent, and retrying with the same `--request-id` but a different choice fails as a conflict, like changing the text. `--unfurl` cannot be combined with `--file`, and is refused for a task that replies through a reply adapter. A CLI with this behaviour refuses (exit 9) to post through an older gateway, which would let Slack unfurl.

### Message formatting

Before Cue posts text (`slack send`, `slack edit`, `slack file --comment`, and `task reply`, including replies that go through a reply adapter), the CLI runs it through an ordered list of formatting rules. The defaults link pull requests and factory tickets, so a reply that mentions `#2909` arrives as a link even if you forget to write one:

| You write | Cue posts |
| --- | --- |
| `Merged #2909` | `Merged <https://github.com/build-great-products/rough.app/pull/2909\|#2909>` |
| `*#123*`, `(#123)` | `*<…/pull/123\|#123>*`, `(<…/pull/123\|#123>)` |
| `build-great-products/cue#12` | `<https://github.com/build-great-products/cue/pull/12\|build-great-products/cue#12>` |
| `factory #45`, `factory#45` | `<https://factory.rough.app/t/45\|factory #45>` |
| `` `git show #2909` ``, `<https://x.com\|#2909>`, `https://x.com/a#2909` | unchanged: code, links, and URLs are never touched |
| `<#C0BAQM6JEAJ>`, `#general`, `#2024-planning` | unchanged: channel and user mentions |
| `#fff`, `#a1b2c3`, `#123abc`, `#123456`, `#12`, `2909` | unchanged: colours, other lengths, no `#` |

A bare `#N` needs 3 to 5 digits; `/pull/N` also works for issues, because GitHub redirects it. Preview what would be posted, without contacting the gateway, with `--dry-run` (on `slack send`, `slack edit`, and `task reply`), and skip formatting for one message with `--raw`:

```sh
cue slack send --channel C0BAQM6JEAJ --text "Shipped #2909" --dry-run
# Shipped <https://github.com/build-great-products/rough.app/pull/2909|#2909>
cue task reply AbCdEfGh --text "Literal #2909, please" --raw --done
```

Change the rules in `config.toml`. Without a `[formatting]` table Cue uses the defaults below, which is equivalent to writing them out; `rules = []` under `[formatting]` turns formatting off. Rules run in order, and a link one rule makes is left alone by the rules after it.

```toml
[[formatting.rules]]
kind = "ticket_links"                          # "factory #12" and "factory#12"
prefix = "factory"                             # matched case-insensitively, kept as written
url = "https://factory.rough.app/t/{number}"

[[formatting.rules]]
kind = "github_links"                          # "#1234" and "owner/repo#12"
repo = "build-great-products/rough.app"        # where a bare #1234 points
ignore_after = ["factory"]                     # optional: #N after these words is someone else's number
```

The rules are read by the CLI each time it runs, so an edit applies to the next command without `gateway reload`. A gateway from before this release refuses a `config.toml` with a `[formatting]` table (unknown key), so restart the gateway on the new binary before adding one. Formatting happens before the request is recorded, so a retry with the same `--request-id` must run with the same rules and the same `--raw` choice. `cue config show --json` lists the effective rules. [`docs/SPEC.md`](docs/SPEC.md) section 6b has the exact matching rules.

## Files and exceptional outcomes

Incoming Slack attachments are stored as metadata and downloaded only on request:

```sh
cue file show EXAMPLE --json
cue file get EXAMPLE --output ./attachment.bin
cue slack file --file ./result.bin
```

Cue retains file metadata and transfer outcomes, not permanent copies of transferred bytes.

SQLite is authoritative for tasks and Slack effects. Cue does not claim exactly-once execution across SQLite and Slack:

- a Slack operation that may have reached Slack becomes `ambiguous` and requires inspection before retry; and
- retries preserve the previous attempt and make duplicate risk explicit.

```sh
cue effect list --state ambiguous --json
cue gateway status --json
cue doctor --json
```

## Agent status snapshot

`cue status` describes what the agent session Cue serves is doing right now, for a dashboard. With `--json` it prints one object:

```json
{"version":1,"generatedAt":"2026-11-25T08:00:00Z",
 "orchestrator":{"state":"working","sessionId":"18e8a977-..."},
 "now":[{"at":"2026-11-25T07:59:50.123Z","tool":"Bash","description":"Run the CRM e2e tests"}],
 "lastMessage":{"at":"...","text":"..."},
 "children":[{"name":"claude-crm-book","purpose":"...","state":"working","startedAt":"...","lastAction":{"at":"...","tool":"Edit","description":"book.ts"},"prs":[{"number":2909,"url":"https://github.com/..."}],"tickets":[396]}],
 "inbox":[{"id":"...","state":"open","title":"...","notes":null,"source":"slack","createdAt":"...","url":null,"slackUrl":"https://...","tickets":[]}],
 "queue":[{"id":"...","state":"queued","title":"...","notes":"...","source":"local","createdAt":"...","url":null,"slackUrl":null,"tickets":[410]}],
 "errors":[]}
```

- **The agent** is the tmux session `--session` (default `claude`). Claude Code records each running session in `~/.claude/sessions/<pid>.json` (its session id, directory and busy, shell or idle status), so the pane's pid finds its transcript, `~/.claude/projects/<directory with every other character as ->/<session id>.jsonl`. Failing that, the newest transcript for its directory, then `--transcript PATH`.
- **Now** is its newest 30 tool calls, newest first: the tool and the call's `description` (every Bash call has one), else the file it touched or the first line of its command. **lastMessage** is its last text. Only the last 2 MB of the transcript is read, however large it is, and only lines that are assistant messages are decoded.
- **Children** are the tmux sessions named `--child-prefix` (default `claude-`). The purpose is the first meaningful line of the brief its start command reads (`"$(cat /path/brief.md)"`), else of its first user message. It is working when its session is busy or running a shell, or its status line shows `esc to interrupt`, a shell or a monitor. Its last action comes from the last 512 KB of its own transcript; its PRs from GitHub URLs in the brief and `PR #N` on its status line (in its checkout's `origin` repo); its tickets from factory links and "factory #N" or "ticket #N" in the brief.
- **Inbox** is the open and working tasks, **queue** the queued ones (see `task add --state queued`), from the gateway. The title is the first line (a factory feeder line reads as `#12 comment by ...: text`), notes the rest. `--slack-workspace rough-app` turns a Slack task into a thread link.
- **Redaction:** every text field has secret-looking values replaced by `[redacted]`: bearer and basic credentials, `name=value` or `name: value` where the name holds token, secret, password, key or cookie, credentials in URLs, Slack, GitHub, OpenAI-style and AWS keys, JWTs, PEM private keys, hex runs of 32 or more (commit hashes included), and long base64-looking runs that mix cases and digits. Then each field is capped (descriptions 500 characters, the message 1,000, a purpose or title 300, notes 500), counted in UTF-16 units as JavaScript counts them.
- A source that is missing or fails (no tmux, a gateway that is down or too old for `queued`) is a line in `errors`; the rest is still reported. The command takes about 0.1 s of CPU on a 1.3 GB transcript. `--timeout` bounds it (default 10s).

## Configuration and local state

`config.toml` contains all scalar settings. Slack tokens are redacted from command output and are never stored in SQLite. Paths are fixed under owner-only XDG roots:

```text
$XDG_CONFIG_HOME/cue/config.toml
$XDG_STATE_HOME/cue/cue.sqlite3
$XDG_STATE_HOME/cue/control.sock
$XDG_STATE_HOME/cue/tmp/
```

`config set` stages one validated change while preserving comments and formatting. `gateway reload` verifies and activates the complete candidate snapshot atomically; a failed reload leaves the active runtime unchanged.

Every mutation has a request ID. When the CLI generates one, it reports it in the result. After an interrupted mutation, retry exactly the same input with the same ID; never reuse an ID for changed input.

## Documentation

- [`docs/SYSTEM_MODEL.md`](docs/SYSTEM_MODEL.md): architecture and authority boundaries
- [`docs/SPEC.md`](docs/SPEC.md): normative v1 product contract

## Development

```sh
just check   # gofmt check, go test ./..., go vet ./...
just build
```
