package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/build-great-products/cue/internal/slackapi"
	"github.com/build-great-products/cue/internal/task"
)

// TaskSource is the optional provenance of a task created by a named
// external feeder. Key and URL are empty when absent. ReplySlack, when set,
// is the Slack thread that task reply posts into.
type TaskSource struct {
	Name       string            `json:"name"`
	Key        string            `json:"key,omitempty"`
	URL        string            `json:"url,omitempty"`
	ReplySlack *SlackReplyTarget `json:"reply_slack,omitempty"`
	// ReplyAdapter, when set, names the configured reply adapter that task
	// reply runs instead of posting to Slack. A source has at most one of
	// ReplySlack and ReplyAdapter.
	ReplyAdapter string `json:"reply_adapter,omitempty"`
}

// SlackReplyTarget is a Slack channel and thread root timestamp. The
// workspace is always the active one.
type SlackReplyTarget struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
}

// ValidSlackReplyTarget reports whether channel is a Slack C/G channel ID and
// threadTS a Slack message timestamp that the ledger schema accepts.
func ValidSlackReplyTarget(channel, threadTS string) bool {
	return slackapi.ValidateThreadTarget(channel, threadTS) == nil
}

// taskSourceColumns are appended, in this order, to every task select that
// LEFT JOINs task_sources as x. Scan them with taskSourceScan.
const taskSourceColumns = `x.source_name,x.external_key,x.url,x.reply_channel_id,x.reply_thread_ts,x.reply_adapter`

type taskSourceScan struct {
	name, key, link, replyChannel, replyThread, replyAdapter sql.NullString
}

func (x *taskSourceScan) dest() []any {
	return []any{&x.name, &x.key, &x.link, &x.replyChannel, &x.replyThread, &x.replyAdapter}
}

func (x *taskSourceScan) source() *TaskSource {
	if !x.name.Valid {
		return nil
	}
	out := &TaskSource{Name: x.name.String, Key: x.key.String, URL: x.link.String}
	if x.replyChannel.Valid {
		out.ReplySlack = &SlackReplyTarget{ChannelID: x.replyChannel.String, ThreadTS: x.replyThread.String}
	}
	out.ReplyAdapter = x.replyAdapter.String
	return out
}

func validateTaskSource(src TaskSource) error {
	if src.Name == "" {
		if src.Key != "" || src.URL != "" || src.ReplySlack != nil || src.ReplyAdapter != "" {
			return invalidInput("a task key, URL, Slack reply target, or reply adapter requires a source name")
		}
		return nil
	}
	if !task.ValidSourceName(src.Name) {
		return invalidInput("source name must be 1 to 32 characters of a-z, 0-9, and '-', and not local or slack")
	}
	if src.Key != "" && !task.ValidExternalKey(src.Key) {
		return invalidInput("task key must be 1 to 256 printable ASCII characters without spaces")
	}
	if src.URL != "" && !task.ValidSourceURL(src.URL) {
		return invalidInput("task URL must be an http or https URL of at most 2048 printable ASCII characters")
	}
	if src.ReplySlack != nil && !ValidSlackReplyTarget(src.ReplySlack.ChannelID, src.ReplySlack.ThreadTS) {
		return invalidInput("Slack reply target must be a C or G channel ID and a Slack thread timestamp")
	}
	if src.ReplyAdapter != "" && !task.ValidSourceName(src.ReplyAdapter) {
		return invalidInput("reply adapter name must be 1 to 32 characters of a-z, 0-9 and -")
	}
	if src.ReplySlack != nil && src.ReplyAdapter != "" {
		return invalidInput("a task has a Slack reply target or a reply adapter, not both")
	}
	return nil
}

// CreateFromSource creates a local task, optionally labelled with a named
// source. When src has a key and a task with the same source name and key
// already exists, it returns that task unchanged with Changed false and
// creates nothing; the new text, URL, and reply target are ignored. The key stays reserved
// for as long as that task is retained, whatever its state.
func (s *Store) CreateFromSource(ctx context.Context, requestID, text string, src TaskSource) (MutationResult, error) {
	return s.CreateInState(ctx, requestID, text, src, task.Open)
}

// CreateInState is CreateFromSource for a new task that starts open or
// queued. Deduplication by key ignores state as it ignores text: an existing
// task comes back unchanged, whatever state either asks for.
func (s *Store) CreateInState(ctx context.Context, requestID, text string, src TaskSource, state task.State) (MutationResult, error) {
	normalized, normalizeErr := task.NormalizeText(text)
	canonical := normalized
	if normalizeErr != nil {
		canonical = text
	}
	inputs := []string{canonical}
	if src.Name != "" || src.Key != "" || src.URL != "" || src.ReplySlack != nil || src.ReplyAdapter != "" {
		// Plain creates keep their original request hash, so a request ID
		// recorded before named sources existed still replays.
		inputs = append(inputs, "source", src.Name, src.Key, src.URL)
	}
	if src.ReplySlack != nil {
		// Likewise, sources without a reply target keep their hash.
		inputs = append(inputs, "reply_slack", src.ReplySlack.ChannelID, src.ReplySlack.ThreadTS)
	}
	if src.ReplyAdapter != "" {
		inputs = append(inputs, "reply_adapter", src.ReplyAdapter)
	}
	if state != task.Open {
		// Open creates keep their hash, so earlier request IDs still replay.
		inputs = append(inputs, "state", string(state))
	}
	return s.mutate(ctx, request{"task.create", hash("task.create", inputs...)}, requestID, func(tx *sql.Tx, now int64) (MutationResult, error) {
		if normalizeErr != nil {
			return MutationResult{}, invalidInput(normalizeErr.Error())
		}
		if err := validateTaskSource(src); err != nil {
			return MutationResult{}, err
		}
		if state != task.Open && state != task.Queued {
			return MutationResult{}, invalidInput("a new task is open or queued")
		}
		if src.Key != "" {
			var existingID string
			err := tx.QueryRowContext(ctx, "SELECT task_id FROM task_sources WHERE source_name=? AND external_key=?", src.Name, src.Key).Scan(&existingID)
			if err == nil {
				existing, err := getTx(ctx, tx, existingID)
				if err != nil {
					return MutationResult{}, err
				}
				return resultTask(existing, false), nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return MutationResult{}, err
			}
		}
		id, err := s.ids.NewTaskID()
		if err != nil {
			return MutationResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES(?,1,?,?,'local',?,?,NULL)", id, state, normalized, now, now); err != nil {
			return MutationResult{}, err
		}
		if src.Name != "" {
			var replyChannel, replyThread string
			if src.ReplySlack != nil {
				replyChannel, replyThread = src.ReplySlack.ChannelID, src.ReplySlack.ThreadTS
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO task_sources(task_id,source_name,external_key,url,reply_channel_id,reply_thread_ts,reply_adapter,created_at) VALUES(?,?,?,?,?,?,?,?)", id, src.Name, nullable(src.Key), nullable(src.URL), nullable(replyChannel), nullable(replyThread), nullable(src.ReplyAdapter), now); err != nil {
				return MutationResult{}, err
			}
		}
		return result(id, 1, state, true, now, now), nil
	})
}

// GetTaskReplyTarget resolves a task selector to a local task with a Slack
// reply target. It returns ErrNotFound when the task does not exist or has
// no reply target.
func (s *Store) GetTaskReplyTarget(ctx context.Context, selector string) (Task, SlackReplyTarget, error) {
	t, err := getTx(ctx, s.db, selector)
	if err != nil {
		return Task{}, SlackReplyTarget{}, err
	}
	if t.SourceKind != "local" || t.Source == nil || t.Source.ReplySlack == nil {
		return Task{}, SlackReplyTarget{}, ErrNotFound
	}
	return t, *t.Source.ReplySlack, nil
}

// GetTaskReplyAdapter resolves a task selector to a local task whose named
// source names a reply adapter. It returns ErrNotFound when the task does not
// exist or has no reply adapter.
func (s *Store) GetTaskReplyAdapter(ctx context.Context, selector string) (Task, string, error) {
	t, err := getTx(ctx, s.db, selector)
	if err != nil {
		return Task{}, "", err
	}
	if t.SourceKind != "local" || t.Source == nil || t.Source.ReplyAdapter == "" {
		return Task{}, "", ErrNotFound
	}
	return t, t.Source.ReplyAdapter, nil
}
