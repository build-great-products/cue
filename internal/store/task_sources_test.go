package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/build-great-products/cue/internal/task"
)

func taskCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateFromSourceLabelsDeduplicatesAndLinks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	src := TaskSource{Name: "tickets", Key: "tickets:378:comment-991", URL: "https://example.com/tickets/378"}

	first, err := s.CreateFromSource(ctx, "src-first", "Comment on ticket 378", src)
	if err != nil || !first.Changed || first.Version != 1 || first.State != task.Open {
		t.Fatalf("first = %#v, %v", first, err)
	}
	got, err := s.Get(ctx, first.TaskID)
	if err != nil || got.SourceKind != "local" || got.Source == nil || *got.Source != src {
		t.Fatalf("stored task = %#v source=%#v err=%v", got, got.Source, err)
	}

	// A new request with the same source and key returns the existing task,
	// unchanged, even when the text and URL differ.
	again, err := s.CreateFromSource(ctx, "src-second", "different text", TaskSource{Name: "tickets", Key: src.Key, URL: "https://example.com/other"})
	if err != nil || again.Changed || again.TaskID != first.TaskID || again.Version != 1 {
		t.Fatalf("duplicate = %#v, %v", again, err)
	}
	if got, _ := s.Get(ctx, first.TaskID); got.Text != "Comment on ticket 378" || got.Source.URL != src.URL {
		t.Fatalf("duplicate changed the task: %#v %#v", got, got.Source)
	}
	if n := taskCount(t, s); n != 1 {
		t.Fatalf("tasks = %d", n)
	}

	// The key stays reserved once the task is done.
	if _, err := s.Transition(ctx, "src-done", first.TaskID, task.Done, nil); err != nil {
		t.Fatal(err)
	}
	if done, err := s.CreateFromSource(ctx, "src-after-done", "again", src); err != nil || done.Changed || done.TaskID != first.TaskID || done.State != task.Done {
		t.Fatalf("after done = %#v, %v", done, err)
	}

	// Keys are scoped to their source name.
	other, err := s.CreateFromSource(ctx, "src-other", "other source", TaskSource{Name: "mail", Key: src.Key})
	if err != nil || !other.Changed || other.TaskID == first.TaskID {
		t.Fatalf("other source = %#v, %v", other, err)
	}
	// Without a key, every create is a new task.
	for _, id := range []string{"src-nokey-1", "src-nokey-2"} {
		if r, err := s.CreateFromSource(ctx, id, "no key", TaskSource{Name: "tickets"}); err != nil || !r.Changed {
			t.Fatalf("no key = %#v, %v", r, err)
		}
	}
	if n := taskCount(t, s); n != 4 {
		t.Fatalf("tasks = %d", n)
	}

	// Replaying a request ID returns its original outcome; reusing it for
	// different input is a conflict, as for every mutation.
	if replay, err := s.CreateFromSource(ctx, "src-first", "Comment on ticket 378", src); err != nil || replay != first {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if _, err := s.CreateFromSource(ctx, "src-first", "Comment on ticket 378", TaskSource{Name: "tickets", Key: "other"}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed source under same request ID = %v", err)
	}
}

func TestCreateFromSourceRejectsInvalidSources(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i, src := range []TaskSource{
		{Name: "local"},
		{Name: "slack", Key: "k"},
		{Name: "Upper"},
		{Name: "has space"},
		{Name: "abcdefghijklmnopqrstuvwxyz0123456"},
		{Key: "orphan-key"},
		{URL: "https://example.com"},
		{Name: "ok", Key: "has space"},
		{Name: "ok", Key: "tab\tkey"},
		{Name: "ok", URL: "ftp://example.com"},
		{Name: "ok", URL: "https://"},
		{Name: "ok", URL: "https://example.com/a b"},
	} {
		_, err := s.CreateFromSource(ctx, "bad-source-"+string(rune('a'+i)), "text", src)
		var semantic *mutationError
		if !errors.As(err, &semantic) || semantic.code != "invalid_input" {
			t.Fatalf("%#v err = %v", src, err)
		}
	}
	if n := taskCount(t, s); n != 0 {
		t.Fatalf("tasks = %d", n)
	}
}

func TestTaskSourceSchemaChecks(t *testing.T) {
	s := testStore(t)
	if _, err := s.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_schema_source',1,'open','x','local',1,1,NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, values := range [][]any{
		{"slack", nil, nil},
		{"Bad", nil, nil},
		{"ok", "has space", nil},
		{"ok", nil, "mailto:x@example.com"},
		{"ok", nil, "https://"},
	} {
		if _, err := s.DB().Exec(`INSERT INTO task_sources(task_id,source_name,external_key,url,created_at) VALUES('tsk_schema_source',?,?,?,1)`, values...); err == nil {
			t.Fatalf("schema accepted %v", values)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO task_sources(task_id,source_name,external_key,url,created_at) VALUES('tsk_schema_source','ok','a:b/c#1','https://example.com/x?y=1',1)`); err != nil {
		t.Fatal(err)
	}
}

func TestTaskSourceAppearsInListReadModelAndChanges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	src := TaskSource{Name: "prs", Key: "pr-12:comment-3", URL: "https://example.com/pr/12"}
	named, err := s.CreateFromSource(ctx, "rm-named", "review comment", src)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.Create(ctx, "rm-plain", "plain")
	if err != nil {
		t.Fatal(err)
	}
	tasks, _, err := s.List(ctx, nil, true, 10, "")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("list = %#v, %v", tasks, err)
	}
	for _, got := range tasks {
		switch got.ID {
		case named.TaskID:
			if got.Source == nil || *got.Source != src {
				t.Fatalf("named list source = %#v", got.Source)
			}
		case plain.TaskID:
			if got.Source != nil {
				t.Fatalf("plain list source = %#v", got.Source)
			}
		}
	}
	model, err := s.GetTaskReadModel(ctx, named.TaskID)
	if err != nil || model.Task.Source == nil || *model.Task.Source != src || model.SlackSource != nil {
		t.Fatalf("read model = %#v, %v", model, err)
	}
	changes, _, err := s.TaskChanges(ctx, 0, 10)
	if err != nil || len(changes) != 2 || changes[0].Task.Source == nil || *changes[0].Task.Source != src || changes[1].Task.Source != nil {
		t.Fatalf("changes = %#v, %v", changes, err)
	}
}

func TestPurgeRemovesTaskSourceAndReleasesKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	src := TaskSource{Name: "tickets", Key: "tickets:1"}
	first, err := s.CreateFromSource(ctx, "purge-src-create", "first", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, "purge-src-done", first.TaskID, task.Done, nil); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	got, err := s.ConfirmMetadataPurge(ctx, "purge-src", cutoff)
	if err != nil || got.Removed.Tasks != 1 {
		t.Fatalf("purge = %#v, %v", got.Removed, err)
	}
	var rows int
	if err := s.DB().QueryRow("SELECT count(*) FROM task_sources").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("task_sources rows = %d, %v", rows, err)
	}
	again, err := s.CreateFromSource(ctx, "purge-src-recreate", "second", src)
	if err != nil || !again.Changed || again.TaskID == first.TaskID {
		t.Fatalf("recreate after purge = %#v, %v", again, err)
	}
}

func TestOpenUpgradeV10AddsTaskSources(t *testing.T) {
	path := ledgerAtVersion(t, 10,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_v10_task',1,'open','keep me','local',1,1,NULL)`,
	)
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open v10: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion || SchemaVersion != 11 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	got, err := s.Get(context.Background(), "tsk_v10_task")
	if err != nil || got.Text != "keep me" || got.Source != nil {
		t.Fatalf("task after upgrade=%+v err=%v", got, err)
	}
	if r, err := s.CreateFromSource(context.Background(), "v10-src", "new", TaskSource{Name: "tickets", Key: "k"}); err != nil || !r.Changed {
		t.Fatalf("create after upgrade = %#v, %v", r, err)
	}
}
