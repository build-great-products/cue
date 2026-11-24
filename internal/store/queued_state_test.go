package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/build-great-products/cue/internal/clock"
	"github.com/build-great-products/cue/internal/task"
)

const (
	v14OpenTask    = "AAAAAAAAAAAAAAAAAAAAA1"
	v14WorkingTask = "AAAAAAAAAAAAAAAAAAAAA2"
	v14DoneTask    = "AAAAAAAAAAAAAAAAAAAAA3"
)

// v14Ledger builds a schema 14 ledger with a task in every state it knows and
// a row in each kind of child table that references tasks.
func v14Ledger(t *testing.T) string {
	t.Helper()
	return ledgerAtVersion(t, 14,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('`+v14OpenTask+`',1,'open','open task','local',1,1,NULL)`,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('`+v14WorkingTask+`',2,'open','working task','slack',2,2,NULL)`,
		`UPDATE tasks SET state='working',updated_at=3 WHERE task_id='`+v14WorkingTask+`'`,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('`+v14DoneTask+`',1,'open','done task','local',4,4,NULL)`,
		`UPDATE tasks SET state='done',updated_at=5,done_at=5 WHERE task_id='`+v14DoneTask+`'`,
		`INSERT INTO task_state_events(event_id,task_id,prior_state,new_state,actor,note,request_id,committed_at) VALUES('evt-working','`+v14WorkingTask+`','open','working','local',NULL,'req-working',3)`,
		`INSERT INTO task_state_events(event_id,task_id,prior_state,new_state,actor,note,request_id,committed_at) VALUES('evt-done','`+v14DoneTask+`','open','done','local','finished','req-done',5)`,
		`INSERT INTO task_sources(task_id,source_name,external_key,url,created_at) VALUES('`+v14OpenTask+`','tracker','item-1','https://example.com/1',1)`,
		`INSERT INTO slack_source_events(source_event_id,event_kind,received_at_us,committed_at_us,admission_result,task_id,task_version,changed,last_seen_at_us) VALUES('Ev-v14','message',2,2,'admitted','`+v14WorkingTask+`',1,1,2)`,
		`INSERT INTO slack_effects(effect_id,idempotency_key,related_task_id,kind,team_id,channel_id,text,payload_hash,state,attempt_count,prepared_at_us,submitted_at_us,completed_at_us,response_message_ts,unfurl,created_at_us,updated_at_us) VALUES('abcdefghijklmnopqrstuv','v14-send','`+v14WorkingTask+`','send','T1','C1','reply','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','confirmed',1,1,1,1,'1700000000.000001',0,1,1)`,
		`INSERT INTO reply_adapter_effects(effect_id,related_task_id,adapter,payload_hash,state,attempt_count,prepared_at_us,submitted_at_us,completed_at_us,updated_at_us) VALUES('bcdefghijklmnopqrstuvw','`+v14DoneTask+`','tracker','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','confirmed',1,1,1,1,1)`,
	)
}

func TestOpenUpgradeV14AdmitsQueuedAndPreservesTasks(t *testing.T) {
	ctx := context.Background()
	path := v14Ledger(t)
	type row struct {
		state   string
		version int64
		seq     int64
		kind    string
	}
	before := map[string]row{}
	func() {
		db, err := sql.Open("sqlite", sqliteDSN(path, time.Second))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query("SELECT task_id,state,version,change_seq,change_kind FROM tasks")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var r row
			if err := rows.Scan(&id, &r.state, &r.version, &r.seq, &r.kind); err != nil {
				t.Fatal(err)
			}
			before[id] = r
		}
	}()
	if len(before) != 3 {
		t.Fatalf("v14 tasks=%v", before)
	}

	s, err := Open(ctx, path, Options{Clock: clock.Fixed{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}})
	if err != nil {
		t.Fatalf("Open v14: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 15 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	for id, want := range before {
		var got row
		if err := s.DB().QueryRow("SELECT state,version,change_seq,change_kind FROM tasks WHERE task_id=?", id).Scan(&got.state, &got.version, &got.seq, &got.kind); err != nil || got != want {
			t.Fatalf("task %s after upgrade=%+v want %+v err=%v", id, got, want, err)
		}
	}
	var events, sources, effects, adapterEffects, sourceEvents, leftovers int
	for query, dest := range map[string]*int{
		"SELECT count(*) FROM task_state_events":                                          &events,
		"SELECT count(*) FROM task_sources":                                               &sources,
		"SELECT count(*) FROM slack_effects WHERE related_task_id IS NOT NULL":            &effects,
		"SELECT count(*) FROM reply_adapter_effects":                                      &adapterEffects,
		"SELECT count(*) FROM slack_source_events WHERE task_id IS NOT NULL":              &sourceEvents,
		"SELECT count(*) FROM sqlite_master WHERE name LIKE '%_v14' OR sql LIKE '%_v14%'": &leftovers,
	} {
		if err := s.DB().QueryRow(query).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if events != 2 || sources != 1 || effects != 1 || adapterEffects != 1 || sourceEvents != 1 || leftovers != 0 {
		t.Fatalf("events=%d sources=%d effects=%d adapter=%d source_events=%d leftovers=%d", events, sources, effects, adapterEffects, sourceEvents, leftovers)
	}
	rows, err := s.DB().Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after upgrade")
	}
	_ = rows.Close()
	var integrity string
	if err := s.DB().QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%q err=%v", integrity, err)
	}
	// Children still reference the rebuilt tasks table: RESTRICT holds and
	// a dangling reference is refused.
	if _, err := s.DB().Exec("DELETE FROM tasks WHERE task_id=?", v14DoneTask); err == nil {
		t.Fatal("deleting a task with children succeeded")
	}
	if _, err := s.DB().Exec("INSERT INTO task_state_events(event_id,task_id,prior_state,new_state,actor,request_id,committed_at) VALUES('evt-x','AAAAAAAAAAAAAAAAAAAAA9','open','queued','local','req-x',9)"); err == nil {
		t.Fatal("state event for a missing task was accepted")
	}

	// The triggers survived: a transition advances the change sequence.
	var head int64
	if err := s.DB().QueryRow("SELECT last_seq FROM task_change_sequence").Scan(&head); err != nil {
		t.Fatal(err)
	}
	moved, err := s.Transition(ctx, "req-queue-open", v14OpenTask, task.Queued, nil)
	if err != nil || moved.State != task.Queued || !moved.Changed {
		t.Fatalf("queue=%+v err=%v", moved, err)
	}
	var seq int64
	var kind string
	if err := s.DB().QueryRow("SELECT change_seq,change_kind FROM tasks WHERE task_id=?", v14OpenTask).Scan(&seq, &kind); err != nil || seq != head+1 || kind != "state" {
		t.Fatalf("seq=%d kind=%s head=%d err=%v", seq, kind, head, err)
	}
	created, err := s.CreateInState(ctx, "req-create-queued", "later", TaskSource{}, task.Queued)
	if err != nil || created.State != task.Queued {
		t.Fatalf("create queued=%+v err=%v", created, err)
	}
	if err := s.DB().QueryRow("SELECT change_seq FROM tasks WHERE task_id=?", created.TaskID).Scan(&seq); err != nil || seq != head+2 {
		t.Fatalf("created seq=%d err=%v", seq, err)
	}

	// No pooled connection is left without foreign keys or with legacy ALTER
	// TABLE behaviour: hold every connection at once and check each one.
	conns := make([]*sql.Conn, 0, 4)
	for i := 0; i < 4; i++ {
		conn, err := s.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		var foreign, legacy int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreign); err != nil || foreign != 1 {
			t.Fatalf("foreign_keys=%d err=%v", foreign, err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA legacy_alter_table").Scan(&legacy); err != nil || legacy != 0 {
			t.Fatalf("legacy_alter_table=%d err=%v", legacy, err)
		}
		_ = conn.Close()
	}

	// Reopening the upgraded ledger is an ordinary compatible open.
	_ = s.Close()
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

func TestInitializeHasQueuedSchema(t *testing.T) {
	s := testStore(t)
	var ddl string
	if err := s.DB().QueryRow("SELECT sql FROM sqlite_master WHERE name='task_state_events'").Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "REFERENCES tasks(task_id)") || !strings.Contains(ddl, "'queued'") {
		t.Fatalf("task_state_events DDL=%s", ddl)
	}
	rows, err := s.DB().Query("SELECT sql FROM sqlite_master WHERE sql LIKE '%REFERENCES%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sql string
		if err := rows.Scan(&sql); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sql, "_v14") {
			t.Fatalf("a reference names a migration table: %s", sql)
		}
	}
}

func TestQueuedTasksStayOutOfTheChangeFeed(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	var queued []string
	for i := 0; i < 5; i++ {
		r, err := s.CreateInState(ctx, "req-q-"+string(rune('a'+i)), "later", TaskSource{}, task.Queued)
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, r.TaskID)
	}
	open1, err := s.Create(ctx, "req-open-1", "now")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.CreateInState(ctx, "req-q2-"+string(rune('a'+i)), "later", TaskSource{}, task.Queued); err != nil {
			t.Fatal(err)
		}
	}
	open2, err := s.Create(ctx, "req-open-2", "now too")
	if err != nil {
		t.Fatal(err)
	}

	// A page of 1 still reaches both open tasks: the filter is applied before
	// LIMIT, so queued rows never fill a page.
	changes, head, err := s.TaskChanges(ctx, 0, 1)
	if err != nil || len(changes) != 1 || changes[0].Task.ID != open1.TaskID || head != 10 {
		t.Fatalf("page 1=%+v head=%d err=%v", changes, head, err)
	}
	changes, _, err = s.TaskChanges(ctx, changes[0].Seq, 1)
	if err != nil || len(changes) != 1 || changes[0].Task.ID != open2.TaskID {
		t.Fatalf("page 2=%+v err=%v", changes, err)
	}
	after := changes[0].Seq
	changes, _, err = s.TaskChanges(ctx, after, 100)
	if err != nil || len(changes) != 0 {
		t.Fatalf("tail=%+v err=%v", changes, err)
	}

	// Moving a queued task to open makes it appear as a state change.
	if _, err := s.Transition(ctx, "req-unqueue", queued[2], task.Open, nil); err != nil {
		t.Fatal(err)
	}
	changes, _, err = s.TaskChanges(ctx, after, 100)
	if err != nil || len(changes) != 1 || changes[0].Task.ID != queued[2] || changes[0].Kind != TaskChangeState || changes[0].Task.State != task.Open {
		t.Fatalf("unqueued=%+v err=%v", changes, err)
	}
	// Moving an open task to queued hides it.
	if _, err := s.Transition(ctx, "req-requeue", open1.TaskID, task.Queued, nil); err != nil {
		t.Fatal(err)
	}
	changes, _, err = s.TaskChanges(ctx, 0, 100)
	if err != nil || len(changes) != 2 {
		t.Fatalf("after requeue=%+v err=%v", changes, err)
	}
	for _, c := range changes {
		if c.Task.State == task.Queued {
			t.Fatalf("queued task in feed: %+v", c)
		}
	}

	counts, err := s.Counts(ctx)
	if err != nil || counts.Queued != 8 || counts.Open != 2 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
	state := "queued"
	listed, _, err := s.List(ctx, &state, false, 100, "")
	if err != nil || len(listed) != 8 {
		t.Fatalf("queued list=%d err=%v", len(listed), err)
	}
	listed, _, err = s.List(ctx, nil, false, 100, "")
	if err != nil || len(listed) != 2 {
		t.Fatalf("default list=%d err=%v", len(listed), err)
	}
	listed, _, err = s.List(ctx, nil, true, 100, "")
	if err != nil || len(listed) != 10 {
		t.Fatalf("all list=%d err=%v", len(listed), err)
	}
}

func TestCreateQueuedDedupAndRequestHash(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	src := TaskSource{Name: "tracker", Key: "item-1"}
	first, err := s.CreateInState(ctx, "req-dedup-1", "first", src, task.Open)
	if err != nil || !first.Changed || first.State != task.Open {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := s.CreateInState(ctx, "req-dedup-2", "second", src, task.Queued)
	if err != nil || again.Changed || again.TaskID != first.TaskID || again.State != task.Open {
		t.Fatalf("dedup=%+v err=%v", again, err)
	}
	// The state is part of the request: the same ID with another state conflicts.
	if _, err := s.CreateInState(ctx, "req-dedup-1", "first", src, task.Queued); err != ErrRequestConflict {
		t.Fatalf("replay with other state err=%v", err)
	}
	if _, err := s.CreateInState(ctx, "req-working", "x", TaskSource{}, task.Working); err == nil {
		t.Fatal("created a working task")
	}
}

// TestUpgradeLedgerCopy upgrades a copy of a real ledger. It runs only when
// CUE_UPGRADE_LEDGER names one; the ledger is read through a read-only
// connection and copied with VACUUM INTO, and the copy is removed afterwards.
func TestUpgradeLedgerCopy(t *testing.T) {
	source := os.Getenv("CUE_UPGRADE_LEDGER")
	if source == "" {
		t.Skip("CUE_UPGRADE_LEDGER is not set")
	}
	ctx := context.Background()
	ro, err := sql.Open("sqlite", "file:"+filepath.ToSlash(source)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.sqlite3")
	if _, err := ro.Exec("VACUUM INTO ?", copyPath); err != nil {
		_ = ro.Close()
		t.Fatal(err)
	}
	_ = ro.Close()
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(copyPath + suffix)
		}
	})
	counts := func(db *sql.DB) map[string]int64 {
		out := map[string]int64{}
		rows, err := db.Query("SELECT state,count(*) FROM tasks GROUP BY state")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			var n int64
			if err := rows.Scan(&state, &n); err != nil {
				t.Fatal(err)
			}
			out[state] = n
		}
		var seqSum, events int64
		if err := db.QueryRow("SELECT coalesce(sum(change_seq),0) FROM tasks").Scan(&seqSum); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT count(*) FROM task_state_events").Scan(&events); err != nil {
			t.Fatal(err)
		}
		out["change_seq_sum"], out["state_events"] = seqSum, events
		return out
	}
	plain, err := sql.Open("sqlite", sqliteDSN(copyPath, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var fromVersion int
	if err := plain.QueryRow("PRAGMA user_version").Scan(&fromVersion); err != nil {
		t.Fatal(err)
	}
	want := counts(plain)
	_ = plain.Close()

	s, err := Open(ctx, copyPath, Options{})
	if err != nil {
		t.Fatalf("Open copy (version %d): %v", fromVersion, err)
	}
	defer s.Close()
	got := counts(s.DB())
	for key, n := range want {
		if got[key] != n {
			t.Fatalf("%s: before %d after %d", key, n, got[key])
		}
	}
	rows, err := s.DB().Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation")
	}
	_ = rows.Close()
	var integrity string
	if err := s.DB().QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%q err=%v", integrity, err)
	}
	t.Logf("upgraded a version %d ledger copy to %d: %v", fromVersion, SchemaVersion, got)
}
