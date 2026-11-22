package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/build-great-products/cue/internal/protocol"
	"github.com/build-great-products/cue/internal/slackapi"
	"github.com/build-great-products/cue/internal/slackapi/fake"
)

// TestPostsTurnUnfurlingOffUnlessAsked runs the built CLI against fake Slack:
// every post (slack send, and task reply into a reply target) reaches
// chat.postMessage with unfurling off unless the caller passes --unfurl; the
// effect records the choice for inspection; a replay with the same request ID
// and the other choice is a conflict that posts nothing; and --unfurl is
// refused where it cannot apply.
func TestPostsTurnUnfurlingOffUnlessAsked(t *testing.T) {
	const (
		pitch   = "C0PITCH01"
		tickets = "C0TICKETS"
		thread  = "1700000500.000100"
	)
	adapter, _ := fakeAdapter(t)
	h := newSlackHarnessWith(t, nil, "[reply_adapters.factory]\ncommand = \""+adapter+"\"\ntimeout = \"1s\"\n", nil, steps(
		verificationSteps(),
		[]fake.Step{
			{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: pitch, MessageTS: "1700000600.000001"}},
			{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: pitch, MessageTS: "1700000600.000002"}},
			{Operation: fake.Permalink, Request: slackapi.PermalinkRequest{ChannelID: tickets, MessageTS: thread}, Result: "https://example.invalid/thread"},
			{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: tickets, MessageTS: "1700000600.000003"}},
			{Operation: fake.Permalink, Request: slackapi.PermalinkRequest{ChannelID: tickets, MessageTS: thread}, Result: "https://example.invalid/thread"},
			{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: tickets, MessageTS: "1700000600.000004"}},
		},
	))
	type sent struct {
		RequestID string                  `json:"request_id"`
		Effect    protocol.SlackEffectDTO `json:"effect"`
	}
	send := func(args ...string) sent {
		t.Helper()
		var out sent
		raw := h.run("", append([]string{"slack", "send", "--channel", pitch, "--json"}, args...)...)
		if err := json.Unmarshal(raw, &out); err != nil || out.Effect.State != "confirmed" {
			t.Fatalf("slack send %v: %v %s", args, err, raw)
		}
		return out
	}
	unfurlOf := func(d protocol.SlackEffectDTO) string {
		if d.Unfurl == nil {
			return "absent"
		}
		if *d.Unfurl {
			return "true"
		}
		return "false"
	}

	// Default: no previews.
	quiet := send("--text", "Discover: https://www.linkedin.com/in/someone", "--request-id", "send-quiet")
	if got := unfurlOf(quiet.Effect); got != "false" {
		t.Fatalf("default send effect unfurl=%s", got)
	}
	// --unfurl opts back in.
	loud := send("--text", "Demo: https://example.com/video", "--unfurl", "--request-id", "send-loud")
	if got := unfurlOf(loud.Effect); got != "true" {
		t.Fatalf("--unfurl send effect unfurl=%s", got)
	}

	// Inspection shows what was sent.
	var inspected protocol.EffectInspectDTO
	if raw := h.run("", "effect", "inspect", quiet.Effect.EffectID, "--json"); json.Unmarshal(raw, &inspected) != nil || unfurlOf(inspected.Effect) != "false" {
		t.Fatalf("effect inspect = %s", raw)
	}

	// Same request ID with the other choice is a conflict and posts nothing.
	if out, code := h.runExit("", "slack", "send", "--channel", pitch, "--text", "Discover: https://www.linkedin.com/in/someone", "--unfurl", "--request-id", "send-quiet", "--json"); code != 4 || !strings.Contains(string(out), "request_id_conflict") {
		t.Fatalf("replay with --unfurl exit=%d out=%s", code, out)
	}
	if out, code := h.runExit("", "slack", "send", "--channel", pitch, "--text", "Demo: https://example.com/video", "--request-id", "send-loud", "--json"); code != 4 || !strings.Contains(string(out), "request_id_conflict") {
		t.Fatalf("replay without --unfurl exit=%d out=%s", code, out)
	}
	// The exact same input replays the original effect without posting.
	if again := send("--text", "Discover: https://www.linkedin.com/in/someone", "--request-id", "send-quiet"); again.Effect.EffectID != quiet.Effect.EffectID {
		t.Fatalf("exact replay = %+v", again)
	}

	// Refusals: --unfurl with a file, and the dropped --no-unfurl flag.
	attachment := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(attachment, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, code := h.runExit("", "slack", "send", "--channel", pitch, "--text", "x", "--file", attachment, "--unfurl"); code != 2 {
		t.Fatalf("--unfurl --file exit=%d out=%s", code, out)
	}
	if out, code := h.runExit("", "slack", "send", "--channel", pitch, "--text", "x", "--no-unfurl"); code != 2 {
		t.Fatalf("--no-unfurl exit=%d out=%s", code, out)
	}

	// task reply into a feeder's reply target: off by default, on with --unfurl.
	create := func(key, via string) string {
		t.Helper()
		args := []string{"task", "create", "--text", "item " + key, "--source", "tickets", "--key", key, "--json"}
		if via != "" {
			args = append(args, "--reply-via", via)
		} else {
			args = append(args, "--reply-slack", tickets+":"+thread)
		}
		var created struct {
			Result protocol.TaskMutationDTO `json:"result"`
		}
		if raw := h.run("", args...); json.Unmarshal(raw, &created) != nil || created.Result.TaskID == "" {
			t.Fatalf("task create: %s", raw)
		}
		return created.Result.TaskID
	}
	reply := func(task string, args ...string) protocol.TaskReplyDTO {
		t.Helper()
		var replied struct {
			Result protocol.TaskReplyDTO `json:"result"`
		}
		raw := h.run("", append([]string{"task", "reply", task, "--done", "--json"}, args...)...)
		if err := json.Unmarshal(raw, &replied); err != nil || replied.Result.Effect.State != "confirmed" {
			t.Fatalf("task reply: %v %s", err, raw)
		}
		return replied.Result
	}
	if got := unfurlOf(reply(create("item-1", ""), "--text", "See https://www.linkedin.com/in/a").Effect); got != "false" {
		t.Fatalf("default reply effect unfurl=%s", got)
	}
	if got := unfurlOf(reply(create("item-2", ""), "--text", "See https://example.com/b", "--unfurl").Effect); got != "true" {
		t.Fatalf("--unfurl reply effect unfurl=%s", got)
	}

	// A reply adapter gets text only, so --unfurl is refused before it runs.
	if out, code := h.runExit("", "task", "reply", create("item-3", "factory"), "--text", "x", "--unfurl", "--json"); code != 2 || !strings.Contains(string(out), "unfurl_not_applicable") {
		t.Fatalf("adapter reply --unfurl exit=%d out=%s", code, out)
	}
	if n := h.count("SELECT count(*) FROM reply_adapter_effects"); n != 0 {
		t.Fatalf("adapter effects after refusal = %d", n)
	}

	// Fake Slack saw exactly four posts, each with the expected choice.
	h.waitWebDone()
	var posts []slackapi.PostMessageRequest
	for _, call := range h.web.Calls() {
		if call.Operation == fake.PostMessage {
			posts = append(posts, call.Request.(slackapi.PostMessageRequest))
		}
	}
	want := []struct {
		channel string
		unfurl  bool
	}{{pitch, false}, {pitch, true}, {tickets, false}, {tickets, true}}
	if len(posts) != len(want) {
		t.Fatalf("posts=%+v", posts)
	}
	for i, w := range want {
		if posts[i].ChannelID != w.channel || posts[i].Unfurl != w.unfurl {
			t.Errorf("post %d = %+v, want channel=%s unfurl=%v", i, posts[i], w.channel, w.unfurl)
		}
	}
	if n := h.count("SELECT count(*) FROM slack_effects WHERE kind='send' AND unfurl=0"); n != 2 {
		t.Fatalf("sends recorded without unfurl = %d", n)
	}
	if n := h.count("SELECT count(*) FROM slack_effects WHERE kind='send' AND unfurl=1"); n != 2 {
		t.Fatalf("sends recorded with unfurl = %d", n)
	}
}
