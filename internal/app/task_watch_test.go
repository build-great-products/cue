package app

import (
	"bytes"
	"testing"

	"github.com/build-great-products/cue/internal/protocol"
)

func TestTaskWatchTextIsOneReversibleLine(t *testing.T) {
	got := escapeTaskWatchText("a\\b\nc\r\td\x1b[31m\u0085e\u2028f\u2029g café ✓")
	want := "a\\\\b\\nc\\r\\td\\u001b[31m\\u0085e\\u2028f\\u2029g café ✓"
	if got != want {
		t.Fatalf("escaped=%q want %q", got, want)
	}
	var out bytes.Buffer
	change := protocol.TaskChangeDTO{ChangeSeq: 7, Change: "edited", Task: protocol.TaskDTO{TaskID: "AbCdEfGhIjKlMnOpQrStUv", Version: 2, State: "open", SourceKind: "slack", Text: "x\ny", SlackSource: &protocol.SlackSourceDTO{ChannelID: "C0CC6JQ4A2U", ThreadTS: "1700000000.000001", ParentTaskID: "ParentTaskIdParentTask"}}}
	if err := writeTaskWatchLine(&out, change, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "cursor=7 change=edited task=AbCdEfGhIjKlMnOpQrStUv version=2 state=open source=slack channel=C0CC6JQ4A2U thread=1700000000.000001 parent=ParentTaskIdParentTask key= url= reply= via= text=x\\ny\n" {
		t.Fatalf("line=%q", out.String())
	}
	out.Reset()
	local := protocol.TaskChangeDTO{ChangeSeq: 8, Change: "created", Task: protocol.TaskDTO{TaskID: "LocalTaskIdLocalTaskId", Version: 1, State: "open", SourceKind: "local", Text: "local"}}
	if err := writeTaskWatchLine(&out, local, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "cursor=8 change=created task=LocalTaskIdLocalTaskId version=1 state=open source=local channel= thread= parent= key= url= reply= via= text=local\n" {
		t.Fatalf("local line=%q", out.String())
	}
	out.Reset()
	named := protocol.TaskChangeDTO{ChangeSeq: 9, Change: "created", Task: protocol.TaskDTO{TaskID: "NamedTaskIdNamedTaskId", Version: 1, State: "open", SourceKind: "local", Text: "ticket comment", Source: &protocol.TaskSourceDTO{Name: "tickets", Key: "tickets:378:comment-991", URL: "https://example.com/tickets/378"}}}
	if err := writeTaskWatchLine(&out, named, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "cursor=9 change=created task=NamedTaskIdNamedTaskId version=1 state=open source=tickets channel= thread= parent= key=tickets:378:comment-991 url=https://example.com/tickets/378 reply= via= text=ticket comment\n" {
		t.Fatalf("named line=%q", out.String())
	}
	out.Reset()
	replyable := protocol.TaskChangeDTO{ChangeSeq: 10, Change: "created", Task: protocol.TaskDTO{TaskID: "ReplyTaskIdReplyTaskId", Version: 1, State: "open", SourceKind: "local", Text: "ticket comment", Source: &protocol.TaskSourceDTO{Name: "tickets", Key: "tickets:378:comment-992", ReplySlack: &protocol.SlackThreadDTO{ChannelID: "C0TICKETS", ThreadTS: "1700000500.000100"}}}}
	if err := writeTaskWatchLine(&out, replyable, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "cursor=10 change=created task=ReplyTaskIdReplyTaskId version=1 state=open source=tickets channel= thread= parent= key=tickets:378:comment-992 url= reply=C0TICKETS:1700000500.000100 via= text=ticket comment\n" {
		t.Fatalf("reply target line=%q", out.String())
	}
	out.Reset()
	adapted := protocol.TaskChangeDTO{ChangeSeq: 11, Change: "created", Task: protocol.TaskDTO{TaskID: "ViaTaskIdViaTaskIdViaT", Version: 1, State: "open", SourceKind: "local", Text: "ticket comment", Source: &protocol.TaskSourceDTO{Name: "factory", Key: "event-12", ReplyVia: "factory"}}}
	if err := writeTaskWatchLine(&out, adapted, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "cursor=11 change=created task=ViaTaskIdViaTaskIdViaT version=1 state=open source=factory channel= thread= parent= key=event-12 url= reply= via=factory text=ticket comment\n" {
		t.Fatalf("reply adapter line=%q", out.String())
	}
	out.Reset()
	if err := writeTaskWatchLine(&out, change, true); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(out.Bytes(), []byte("\n")) != 1 || !bytes.HasPrefix(out.Bytes(), []byte(`{"change_seq":7,"change":"edited","task":{`)) {
		t.Fatalf("json line=%q", out.String())
	}
}

func TestTaskWatchDefaultFilter(t *testing.T) {
	change := func(seq int64, kind, state string) protocol.TaskChangeDTO {
		return protocol.TaskChangeDTO{ChangeSeq: seq, Change: kind, Task: protocol.TaskDTO{State: state}}
	}
	w := &taskWatcher{backlog: true, backlogHead: 2}
	for _, c := range []struct {
		change protocol.TaskChangeDTO
		want   bool
	}{
		{change(1, "edited", "done"), false},   // backlog: open tasks only
		{change(2, "state", "open"), true},     // backlog: reopened task is open work
		{change(3, "created", "open"), true},   // new task
		{change(4, "edited", "done"), true},    // content version changed
		{change(5, "state", "open"), true},     // reopened
		{change(6, "state", "working"), false}, // claimed elsewhere
		{change(7, "state", "done"), false},
	} {
		if got := w.emits(c.change); got != c.want {
			t.Errorf("emits(%+v)=%v want %v", c.change, got, c.want)
		}
	}
	w.opts.stateChanges = true
	if !w.emits(change(8, "state", "done")) {
		t.Fatal("--state-changes did not include done")
	}
}

func TestTaskWatchFlagConflictsAreUsageErrors(t *testing.T) {
	for _, opts := range []taskWatchOptions{
		{since: "1", sinceSet: true, cursorFile: "/tmp/cursor"},
		{since: "1", sinceSet: true, noBacklog: true},
		{since: "-1", sinceSet: true},
		{since: "x", sinceSet: true},
		{interval: "1ms"},
		{interval: "20s"},
		{cursorFile: "relative"},
	} {
		if got := exitCode((App{}).taskWatchTyped(opts)); got != ExitUsage {
			t.Errorf("%+v exit=%d want usage", opts, got)
		}
	}
}
