package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/build-great-products/cue/internal/protocol"
	"github.com/build-great-products/cue/internal/slackapi"
	"github.com/build-great-products/cue/internal/slackapi/fake"
)

// TestFeederTaskRepliesIntoItsSlackThread: a feeder creates a local task with
// a Slack reply target; task watch and task show name the target, and task
// reply --done posts into that thread through the durable effect path and
// completes the task only after Slack confirms. A local task without a target
// still cannot be replied to.
func TestFeederTaskRepliesIntoItsSlackThread(t *testing.T) {
	const (
		channel = "C0TICKETS"
		thread  = "1700000500.000100"
	)
	h := newSlackHarness(t, nil, nil, steps(
		verificationSteps(),
		[]fake.Step{
			{Operation: fake.Permalink, Request: slackapi.PermalinkRequest{ChannelID: channel, MessageTS: thread}, Result: "https://example.invalid/thread"},
			{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: channel, MessageTS: "1700000600.000100"}},
		},
	))
	create := func(text string) protocol.TaskMutationDTO {
		t.Helper()
		var created struct {
			Result protocol.TaskMutationDTO `json:"result"`
		}
		out := h.run("", "task", "create", "--text", text, "--source", "tickets", "--key", "tickets:378:comment-992", "--url", "https://example.com/tickets/378", "--reply-slack", channel+":"+thread, "--json")
		if err := json.Unmarshal(out, &created); err != nil || created.Result.TaskID == "" {
			t.Fatalf("task create --reply-slack: %v %s", err, out)
		}
		return created.Result
	}
	first := create("Comment on ticket 378")
	if again := create("Comment on ticket 378, polled again"); again.Changed || again.TaskID != first.TaskID {
		t.Fatalf("duplicate create = %#v", again)
	}

	watch := startWatchProcess(t, h.binary, h.env, "--since", "0")
	if got, want := watch.next(t), "source=tickets channel= thread= parent= key=tickets:378:comment-992 url=https://example.com/tickets/378 reply="+channel+":"+thread+" via= text=Comment on ticket 378"; !strings.Contains(got, "task="+first.TaskID+" ") || !strings.HasSuffix(got, want) {
		t.Fatalf("watch line=%q want suffix %q", got, want)
	}
	watch.stop(t)
	if shown := string(h.run("", "task", "show", first.TaskID)); !strings.Contains(shown, "reply="+channel+":"+thread) {
		t.Fatalf("task show = %q", shown)
	}
	var shownJSON protocol.TaskDTO
	if out := h.run("", "task", "show", first.TaskID, "--json"); json.Unmarshal(out, &shownJSON) != nil || shownJSON.Source == nil || shownJSON.Source.ReplySlack == nil || *shownJSON.Source.ReplySlack != (protocol.SlackThreadDTO{ChannelID: channel, ThreadTS: thread}) {
		t.Fatalf("task show --json = %s", out)
	}

	var replied struct {
		Result protocol.TaskReplyDTO `json:"result"`
	}
	out := h.run("", "task", "reply", first.TaskID, "--text", "Answered in the ticket thread", "--done", "--json")
	if err := json.Unmarshal(out, &replied); err != nil || replied.Result.Effect.State != "confirmed" || replied.Result.Task == nil || replied.Result.Task.State != "done" {
		t.Fatalf("task reply = %v %s", err, out)
	}
	h.waitWebDone()
	calls := h.web.Calls()
	post := calls[len(calls)-1].Request.(slackapi.PostMessageRequest)
	if post.ChannelID != channel || post.ThreadTS != thread || post.Text != "Answered in the ticket thread" {
		t.Fatalf("post = %+v", post)
	}
	if n := h.count("SELECT count(*) FROM slack_effects WHERE kind='send' AND state='confirmed' AND channel_id=? AND thread_ts=? AND related_task_id=?", channel, thread, first.TaskID); n != 1 {
		t.Fatalf("confirmed sends = %d", n)
	}

	// A local task without a reply target keeps today's behaviour.
	var plain struct {
		Result protocol.TaskMutationDTO `json:"result"`
	}
	if err := json.Unmarshal(h.run("", "task", "create", "--text", "plain", "--json"), &plain); err != nil {
		t.Fatal(err)
	}
	if out, code := h.runExit("", "task", "reply", plain.Result.TaskID, "--text", "x", "--json"); code != 3 || !strings.Contains(string(out), "not_found") {
		t.Fatalf("plain reply exit=%d out=%s", code, out)
	}
}
