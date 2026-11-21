package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/build-great-products/cue/internal/task"
)

// TaskSource is the optional provenance of a task created by a named
// external feeder. Key and URL are empty when absent.
type TaskSource struct {
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
	URL  string `json:"url,omitempty"`
}

// taskSourceColumns are appended, in this order, to every task select that
// LEFT JOINs task_sources as x.
const taskSourceColumns = `x.source_name,x.external_key,x.url`

func taskSourceFrom(name, key, link sql.NullString) *TaskSource {
	if !name.Valid {
		return nil
	}
	return &TaskSource{Name: name.String, Key: key.String, URL: link.String}
}

func validateTaskSource(src TaskSource) error {
	if src.Name == "" {
		if src.Key != "" || src.URL != "" {
			return invalidInput("a task key or URL requires a source name")
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
	return nil
}

// CreateFromSource creates a local task, optionally labelled with a named
// source. When src has a key and a task with the same source name and key
// already exists, it returns that task unchanged with Changed false and
// creates nothing; the new text and URL are ignored. The key stays reserved
// for as long as that task is retained, whatever its state.
func (s *Store) CreateFromSource(ctx context.Context, requestID, text string, src TaskSource) (MutationResult, error) {
	normalized, normalizeErr := task.NormalizeText(text)
	canonical := normalized
	if normalizeErr != nil {
		canonical = text
	}
	inputs := []string{canonical}
	if src != (TaskSource{}) {
		// Plain creates keep their original request hash, so a request ID
		// recorded before named sources existed still replays.
		inputs = append(inputs, "source", src.Name, src.Key, src.URL)
	}
	return s.mutate(ctx, request{"task.create", hash("task.create", inputs...)}, requestID, func(tx *sql.Tx, now int64) (MutationResult, error) {
		if normalizeErr != nil {
			return MutationResult{}, invalidInput(normalizeErr.Error())
		}
		if err := validateTaskSource(src); err != nil {
			return MutationResult{}, err
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
		if _, err = tx.ExecContext(ctx, "INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES(?,1,'open',?,'local',?,?,NULL)", id, normalized, now, now); err != nil {
			return MutationResult{}, err
		}
		if src.Name != "" {
			if _, err = tx.ExecContext(ctx, "INSERT INTO task_sources(task_id,source_name,external_key,url,created_at) VALUES(?,?,?,?,?)", id, src.Name, nullable(src.Key), nullable(src.URL), now); err != nil {
				return MutationResult{}, err
			}
		}
		return result(id, 1, task.Open, true, now, now), nil
	})
}
