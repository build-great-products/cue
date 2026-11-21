package server

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/build-great-products/cue/internal/protocol"
	"github.com/build-great-products/cue/internal/slackapi"
	"github.com/build-great-products/cue/internal/slackapi/fake"
	"github.com/build-great-products/cue/internal/slackops"
	"github.com/build-great-products/cue/internal/store"
	"github.com/build-great-products/cue/internal/task"
)

const (
	replyTargetChannel = "C0TICKETS"
	replyTargetThread  = "1700000500.000100"
)

func newReplyTargetTask(t *testing.T, ledger *store.Store, key string) string {
	t.Helper()
	created, err := ledger.CreateFromSource(context.Background(), "req_create_"+strings.ReplaceAll(key, ":", "_"), "ticket comment", store.TaskSource{Name: "tickets", Key: key, ReplySlack: &store.SlackReplyTarget{ChannelID: replyTargetChannel, ThreadTS: replyTargetThread}})
	if err != nil {
		t.Fatal(err)
	}
	return created.TaskID
}

func newReplyLedger(t *testing.T) *store.Store {
	t.Helper()
	ledger, err := store.Initialize(context.Background(), filepath.Join(t.TempDir(), "cue.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return ledger
}

func TestTaskReplyPostsIntoLocalTaskReplyTarget(t *testing.T) {
	ctx := context.Background()
	ledger := newReplyLedger(t)
	taskID := newReplyTargetTask(t, ledger, "tickets:378:comment-992")
	s, web := newTaskReplyServer(t, ledger,
		fake.Step{Operation: fake.Permalink, Result: "https://example.invalid/thread"},
		fake.Step{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: replyTargetChannel, MessageTS: "1700000600.000100"}},
	)
	requestID := "req_reply_target"
	params := protocol.TaskReplyParams{Task: taskID, Text: "answered", Done: true}
	out, err := s.taskReply(ctx, requestID, params)
	if err != nil || out.Effect.State != string(store.SlackEffectConfirmed) || out.Task == nil || out.Task.State != "done" {
		t.Fatalf("reply=%+v err=%v", out, err)
	}
	// Replay returns the original outcome and does not post again.
	if again, err := s.taskReply(ctx, requestID, params); err != nil || again.Effect.EffectID != out.Effect.EffectID {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	var preflights, posts int
	for _, call := range web.Calls() {
		switch call.Operation {
		case fake.Permalink:
			preflights++
			if req := call.Request.(slackapi.PermalinkRequest); req.ChannelID != replyTargetChannel || string(req.MessageTS) != replyTargetThread {
				t.Fatalf("preflight request=%+v", req)
			}
		case fake.PostMessage:
			posts++
			if req := call.Request.(slackapi.PostMessageRequest); req.ChannelID != replyTargetChannel || string(req.ThreadTS) != replyTargetThread || req.Text != "answered" {
				t.Fatalf("post request=%+v", req)
			}
		}
	}
	if preflights != 1 || posts != 1 {
		t.Fatalf("preflights=%d posts=%d calls=%+v", preflights, posts, web.Calls())
	}
	effects, err := ledger.ListSlackEffects(ctx, nil, 10)
	if err != nil || len(effects) != 1 || effects[0].TeamID != "T012ABC" || effects[0].ChannelID != replyTargetChannel || effects[0].ThreadTS != replyTargetThread || effects[0].RelatedTaskID != taskID {
		t.Fatalf("effects=%+v err=%v", effects, err)
	}
}

func TestTaskReplyWithoutReplyTargetIsNotFound(t *testing.T) {
	ctx := context.Background()
	ledger := newReplyLedger(t)
	plain, err := ledger.Create(ctx, "req_plain", "plain")
	if err != nil {
		t.Fatal(err)
	}
	named, err := ledger.CreateFromSource(ctx, "req_named", "named", store.TaskSource{Name: "tickets", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	s, web := newTaskReplyServer(t, ledger)
	before := len(web.Calls())
	for _, id := range []string{plain.TaskID, named.TaskID} {
		if _, err := s.taskReply(ctx, "req_reply_"+id, protocol.TaskReplyParams{Task: id, Text: "x", Done: true}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("reply to %s err=%v, want not found", id, err)
		}
	}
	if len(web.Calls()) != before {
		t.Fatalf("unexpected Slack calls: %+v", web.Calls()[before:])
	}
}

func TestTaskReplyToReplyTargetNeedsSlackRuntime(t *testing.T) {
	ctx := context.Background()
	ledger := newReplyLedger(t)
	taskID := newReplyTargetTask(t, ledger, "k-unavailable")
	if _, err := (&Server{store: ledger}).taskReply(ctx, "req_unavailable", protocol.TaskReplyParams{Task: taskID, Text: "x", Done: true}); !errors.Is(err, slackops.ErrUnavailable) {
		t.Fatalf("err=%v, want Slack unavailable", err)
	}
	if effects, err := ledger.ListSlackEffects(ctx, nil, 10); err != nil || len(effects) != 0 {
		t.Fatalf("effects=%+v err=%v", effects, err)
	}
	if got, err := ledger.Get(ctx, taskID); err != nil || got.State != task.Open {
		t.Fatalf("task=%+v err=%v", got, err)
	}
}

func TestTaskReplyToChannelWithoutBotLeavesTaskOpen(t *testing.T) {
	ctx := context.Background()
	ledger := newReplyLedger(t)
	taskID := newReplyTargetTask(t, ledger, "k-not-member")
	s, web := newTaskReplyServer(t, ledger,
		fake.Step{Operation: fake.Permalink, Err: slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticNotInChannel, "", nil)},
	)
	if _, err := s.taskReply(ctx, "req_not_member", protocol.TaskReplyParams{Task: taskID, Text: "x", Done: true}); err == nil {
		t.Fatal("reply to a channel without the bot succeeded")
	}
	for _, call := range web.Calls() {
		if call.Operation == fake.PostMessage {
			t.Fatalf("posted despite failed preflight: %+v", web.Calls())
		}
	}
	if got, err := ledger.Get(ctx, taskID); err != nil || got.State != task.Open {
		t.Fatalf("task=%+v err=%v", got, err)
	}
}
