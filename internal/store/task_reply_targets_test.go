package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestCreateFromSourceStoresSlackReplyTarget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target := &SlackReplyTarget{ChannelID: "C0TICKETS", ThreadTS: "1700000500.000100"}
	src := TaskSource{Name: "tickets", Key: "tickets:378:comment-991", URL: "https://example.com/tickets/378", ReplySlack: target}

	first, err := s.CreateFromSource(ctx, "reply-first", "Comment on ticket 378", src)
	if err != nil || !first.Changed {
		t.Fatalf("first = %#v, %v", first, err)
	}
	got, err := s.Get(ctx, first.TaskID)
	if err != nil || got.Source == nil || !reflect.DeepEqual(*got.Source, src) {
		t.Fatalf("stored source = %#v, %v", got.Source, err)
	}
	tasks, _, err := s.List(ctx, nil, true, 10, "")
	if err != nil || len(tasks) != 1 || !reflect.DeepEqual(tasks[0].Source, got.Source) {
		t.Fatalf("list = %#v, %v", tasks, err)
	}
	model, err := s.GetTaskReadModel(ctx, first.TaskID)
	if err != nil || !reflect.DeepEqual(model.Task.Source, got.Source) {
		t.Fatalf("read model = %#v, %v", model.Task.Source, err)
	}
	changes, _, err := s.TaskChanges(ctx, 0, 10)
	if err != nil || len(changes) != 1 || !reflect.DeepEqual(changes[0].Task.Source, got.Source) {
		t.Fatalf("changes = %#v, %v", changes, err)
	}
	gotTask, gotTarget, err := s.GetTaskReplyTarget(ctx, first.TaskID[:8])
	if err != nil || gotTask.ID != first.TaskID || gotTarget != *target {
		t.Fatalf("reply target = %#v %#v, %v", gotTask, gotTarget, err)
	}

	// A repeated key returns the existing task; a different reply target is
	// ignored like a different URL.
	again, err := s.CreateFromSource(ctx, "reply-again", "again", TaskSource{Name: "tickets", Key: src.Key, ReplySlack: &SlackReplyTarget{ChannelID: "C0OTHER", ThreadTS: "1700000600.000100"}})
	if err != nil || again.Changed || again.TaskID != first.TaskID {
		t.Fatalf("duplicate = %#v, %v", again, err)
	}
	if _, gotTarget, _ := s.GetTaskReplyTarget(ctx, first.TaskID); gotTarget != *target {
		t.Fatalf("duplicate changed the reply target: %#v", gotTarget)
	}

	// The same request ID with a different reply target is a conflict, and a
	// replay with the same input returns the original outcome.
	if _, err := s.CreateFromSource(ctx, "reply-first", "Comment on ticket 378", TaskSource{Name: src.Name, Key: src.Key, URL: src.URL, ReplySlack: &SlackReplyTarget{ChannelID: "C0OTHER", ThreadTS: target.ThreadTS}}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed reply target replay err = %v", err)
	}
	if replay, err := s.CreateFromSource(ctx, "reply-first", "Comment on ticket 378", src); err != nil || replay.TaskID != first.TaskID || !replay.Changed {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
}

func TestGetTaskReplyTargetIsNotFoundWithoutTarget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	plain, err := s.Create(ctx, "no-target-plain", "plain")
	if err != nil {
		t.Fatal(err)
	}
	named, err := s.CreateFromSource(ctx, "no-target-named", "named", TaskSource{Name: "tickets", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{plain.TaskID, named.TaskID, "missing"} {
		if _, _, err := s.GetTaskReplyTarget(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetTaskReplyTarget(%s) err = %v", id, err)
		}
	}
}

func TestCreateFromSourceRejectsInvalidReplyTargets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i, src := range []TaskSource{
		{ReplySlack: &SlackReplyTarget{ChannelID: "C0TICKETS", ThreadTS: "1700000500.000100"}},
		{Name: "ok", ReplySlack: &SlackReplyTarget{ChannelID: "D0DIRECT", ThreadTS: "1700000500.000100"}},
		{Name: "ok", ReplySlack: &SlackReplyTarget{ChannelID: "c0lower", ThreadTS: "1700000500.000100"}},
		{Name: "ok", ReplySlack: &SlackReplyTarget{ChannelID: "", ThreadTS: "1700000500.000100"}},
		{Name: "ok", ReplySlack: &SlackReplyTarget{ChannelID: "C0TICKETS", ThreadTS: ""}},
		{Name: "ok", ReplySlack: &SlackReplyTarget{ChannelID: "C0TICKETS", ThreadTS: "1700000500"}},
		{Name: "ok", ReplySlack: &SlackReplyTarget{ChannelID: "C0TICKETS", ThreadTS: "1700000500.1001"}},
		{Name: "ok", ReplySlack: &SlackReplyTarget{ChannelID: "C0TICKETS", ThreadTS: "12345678901234567890123456.000100"}},
	} {
		_, err := s.CreateFromSource(ctx, "bad-reply-"+string(rune('a'+i)), "text", src)
		var semantic *mutationError
		if !errors.As(err, &semantic) || semantic.code != "invalid_input" {
			t.Fatalf("%#v err = %v", src.ReplySlack, err)
		}
	}
	if n := taskCount(t, s); n != 0 {
		t.Fatalf("tasks = %d", n)
	}
}

func TestTaskReplyTargetSchemaChecks(t *testing.T) {
	s := testStore(t)
	if _, err := s.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_schema_reply',1,'open','x','local',1,1,NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, values := range [][]any{
		{"C0TICKETS", nil},
		{nil, "1700000500.000100"},
		{"D0DIRECT", "1700000500.000100"},
		{"C0TICKETS", "1700000500"},
		{"C0TICKETS", "abc.000100"},
	} {
		if _, err := s.DB().Exec(`INSERT INTO task_sources(task_id,source_name,reply_channel_id,reply_thread_ts,created_at) VALUES('tsk_schema_reply','ok',?,?,1)`, values...); err == nil {
			t.Fatalf("schema accepted %v", values)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO task_sources(task_id,source_name,reply_channel_id,reply_thread_ts,created_at) VALUES('tsk_schema_reply','ok','G0PRIVATE','1700000500.000100',1)`); err != nil {
		t.Fatal(err)
	}
}

func TestOpenUpgradeV11AddsReplyTargets(t *testing.T) {
	path := ledgerAtVersion(t, 11,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_v11_task',1,'open','keep me','local',1,1,NULL)`,
		`INSERT INTO task_sources(task_id,source_name,external_key,url,created_at) VALUES('tsk_v11_task','tickets','k1','https://example.com/1',1)`,
	)
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open v11: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion || SchemaVersion != 14 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	got, err := s.Get(context.Background(), "tsk_v11_task")
	if err != nil || got.Source == nil || *got.Source != (TaskSource{Name: "tickets", Key: "k1", URL: "https://example.com/1"}) {
		t.Fatalf("task after upgrade=%#v source=%#v err=%v", got, got.Source, err)
	}
	r, err := s.CreateFromSource(context.Background(), "v11-reply", "new", TaskSource{Name: "tickets", Key: "k2", ReplySlack: &SlackReplyTarget{ChannelID: "C0TICKETS", ThreadTS: "1700000500.000100"}})
	if err != nil || !r.Changed {
		t.Fatalf("create after upgrade = %#v, %v", r, err)
	}
}
