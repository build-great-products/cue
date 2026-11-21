package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/build-great-products/cue/internal/task"
)

func adapterTask(t *testing.T, s *Store, key string) string {
	t.Helper()
	r, err := s.CreateFromSource(context.Background(), "create-"+key, "comment "+key, TaskSource{Name: "factory", Key: key, ReplyAdapter: "factory"})
	if err != nil || !r.Changed {
		t.Fatalf("create = %#v, %v", r, err)
	}
	return r.TaskID
}

func TestReplyAdapterSourceIsStoredAndExclusive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := adapterTask(t, s, "event-1")
	got, err := s.Get(ctx, id)
	if err != nil || got.Source == nil || got.Source.ReplyAdapter != "factory" || got.Source.ReplySlack != nil {
		t.Fatalf("source = %#v, %v", got.Source, err)
	}
	if _, adapter, err := s.GetTaskReplyAdapter(ctx, id); err != nil || adapter != "factory" {
		t.Fatalf("reply adapter = %q, %v", adapter, err)
	}
	if _, _, err := s.GetTaskReplyTarget(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Slack target of an adapter task = %v", err)
	}
	both := TaskSource{Name: "factory", Key: "event-2", ReplyAdapter: "factory", ReplySlack: &SlackReplyTarget{ChannelID: "C0TICKETS", ThreadTS: "1700000500.000100"}}
	if _, err := s.CreateFromSource(ctx, "create-both", "x", both); err == nil {
		t.Fatal("a source with a Slack target and an adapter was accepted")
	}
	for _, bad := range []TaskSource{{Name: "factory", ReplyAdapter: "Bad_Name"}, {ReplyAdapter: "factory"}} {
		if _, err := s.CreateFromSource(ctx, "create-bad-"+bad.ReplyAdapter, "x", bad); err == nil {
			t.Fatalf("accepted %#v", bad)
		}
	}
	// The schema refuses both as well.
	if _, err := s.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_schema_adapter',1,'open','x','local',1,1,NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO task_sources(task_id,source_name,reply_channel_id,reply_thread_ts,reply_adapter,created_at) VALUES('tsk_schema_adapter','ok','C0TICKETS','1700000500.000100','factory',1)`); err == nil {
		t.Fatal("schema accepted a Slack target and an adapter")
	}
}

func TestReplyAdapterEffectLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := adapterTask(t, s, "event-1")
	prepared, err := s.PrepareReplyAdapterEffect(ctx, "reply-1", id, "factory", "On it.")
	if err != nil || prepared.State != SlackEffectPrepared || prepared.AttemptCount != 0 || len(prepared.ID) != 22 {
		t.Fatalf("prepare = %#v, %v", prepared, err)
	}
	// A replay returns the same effect; other input under the ID conflicts.
	if again, err := s.PrepareReplyAdapterEffect(ctx, "reply-1", id, "factory", "On it."); err != nil || again.ID != prepared.ID {
		t.Fatalf("replay = %#v, %v", again, err)
	}
	if _, err := s.PrepareReplyAdapterEffect(ctx, "reply-1", id, "factory", "Something else."); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflicting replay = %v", err)
	}
	claimed, err := s.ClaimReplyAdapterEffect(ctx, prepared.ID)
	if err != nil || claimed.State != SlackEffectSubmitted || claimed.AttemptCount != 1 || claimed.SubmittedAt == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if _, err := s.ClaimReplyAdapterEffect(ctx, prepared.ID); !errors.Is(err, ErrSlackEffectTransition) {
		t.Fatalf("second claim = %v", err)
	}
	zero := 0
	done, err := s.CommitReplyAdapterOutcome(ctx, prepared.ID, ReplyAdapterOutcome{State: SlackEffectConfirmed, ExitCode: &zero})
	if err != nil || done.State != SlackEffectConfirmed || done.ExitCode == nil || *done.ExitCode != 0 || done.CompletedAt == nil {
		t.Fatalf("commit = %#v, %v", done, err)
	}
	if _, err := s.CommitReplyAdapterOutcome(ctx, prepared.ID, ReplyAdapterOutcome{State: SlackEffectFailed}); !errors.Is(err, ErrSlackEffectTransition) {
		t.Fatalf("rewrite = %v", err)
	}
	// The replay after completion still returns the confirmed effect.
	if again, err := s.PrepareReplyAdapterEffect(ctx, "reply-1", id, "factory", "On it."); err != nil || again.State != SlackEffectConfirmed {
		t.Fatalf("replay after confirm = %#v, %v", again, err)
	}
	latest, ok, err := s.LatestReplyAdapterEffect(ctx, id)
	if err != nil || !ok || latest.ID != prepared.ID {
		t.Fatalf("latest = %#v %v %v", latest, ok, err)
	}
	var text int
	if err := s.DB().QueryRow("SELECT count(*) FROM reply_adapter_effects WHERE payload_hash=?", ReplyAdapterPayloadHash(id, "factory", "On it.")).Scan(&text); err != nil || text != 1 {
		t.Fatalf("payload hash rows = %d, %v", text, err)
	}
}

func TestReplyAdapterEffectsLeftSubmittedBecomeAmbiguous(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := adapterTask(t, s, "event-1")
	e, err := s.PrepareReplyAdapterEffect(ctx, "reply-1", id, "factory", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimReplyAdapterEffect(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PrepareReplyAdapterEffect(ctx, "reply-2", id, "factory", "y")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReconcileSubmittedReplyAdapterEffects(ctx); err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v", n, err)
	}
	if got, _ := s.GetReplyAdapterEffect(ctx, e.ID); got.State != SlackEffectAmbiguous || got.DiagnosticCode != ReplyAdapterDiagnosticStartupReconciliation {
		t.Fatalf("after reconcile = %#v", got)
	}
	if got, _ := s.GetReplyAdapterEffect(ctx, pending.ID); got.State != SlackEffectPrepared {
		t.Fatalf("a prepared effect changed: %#v", got)
	}
	counts, err := s.ReplyAdapterEffectCounts(ctx)
	if err != nil || counts[SlackEffectAmbiguous] != 1 || counts[SlackEffectPrepared] != 1 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
}

func TestPurgeKeepsUnresolvedAdapterEffectsAndRemovesConfirmed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	finish := func(key string, state SlackEffectState) string {
		id := adapterTask(t, s, key)
		e, err := s.PrepareReplyAdapterEffect(ctx, "reply-"+key, id, "factory", "x")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimReplyAdapterEffect(ctx, e.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CommitReplyAdapterOutcome(ctx, e.ID, ReplyAdapterOutcome{State: state}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Transition(ctx, "done-"+key, id, task.Done, nil); err != nil {
			t.Fatal(err)
		}
		return id
	}
	confirmed := finish("event-1", SlackEffectConfirmed)
	ambiguous := finish("event-2", SlackEffectAmbiguous)
	got, err := s.ConfirmMetadataPurge(ctx, "purge-adapters", time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || got.Removed.Tasks != 1 || got.Removed.ReplyAdapterEffects != 1 || got.SkippedReferenced.Tasks != 1 {
		t.Fatalf("purge = removed %#v skipped %#v, %v", got.Removed, got.SkippedReferenced, err)
	}
	if _, err := s.Get(ctx, confirmed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("confirmed task kept: %v", err)
	}
	if _, err := s.Get(ctx, ambiguous); err != nil {
		t.Fatalf("task with an ambiguous adapter effect purged: %v", err)
	}
}

func TestOpenUpgradeV12AddsReplyAdapters(t *testing.T) {
	path := ledgerAtVersion(t, 12,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_v12_task',1,'open','keep me','local',1,1,NULL)`,
		`INSERT INTO task_sources(task_id,source_name,external_key,reply_channel_id,reply_thread_ts,created_at) VALUES('tsk_v12_task','tickets','k1','C0TICKETS','1700000500.000100',1)`,
	)
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open v12: %v", err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), "tsk_v12_task")
	if err != nil || got.Source == nil || got.Source.ReplySlack == nil || got.Source.ReplyAdapter != "" {
		t.Fatalf("task after upgrade = %#v, %v", got.Source, err)
	}
	if r, err := s.CreateFromSource(context.Background(), "v12-adapter", "new", TaskSource{Name: "tickets", Key: "k2", ReplyAdapter: "factory"}); err != nil || !r.Changed {
		t.Fatalf("create after upgrade = %#v, %v", r, err)
	}
}
