package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/build-great-products/cue/internal/slackapi"
	"github.com/build-great-products/cue/internal/slackapi/fake"
)

func reactionEvent(id string, kind slackapi.EventKind, channel, ts, user, emoji string) (slackapi.Envelope, *fake.AckRecorder) {
	ack, recorder := fake.NewAckRecorder(nil)
	return slackapi.Envelope{EnvelopeID: "env-" + id, EventID: "Ev-" + id, Kind: kind, TeamID: harnessTeam, ChannelID: channel, UserID: user, MessageTS: slackapi.Timestamp(ts), EventTS: "1700000900.000001", Reaction: emoji, Ack: ack}, recorder
}

// TestEventHooksSeeMessagesAndReactionsWithoutChangingAdmission: a hook for
// one channel is told about every message there (admitted or not), its edits,
// deletions and reactions, once each even when Slack redelivers; reactions
// are acknowledged without a ledger record and admit nothing; messages in
// another channel reach no hook; and admission is what it would be without
// the hook.
func TestEventHooksSeeMessagesAndReactionsWithoutChangingAdmission(t *testing.T) {
	dir, err := os.MkdirTemp(effectiveHome(t), ".cue-hook-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	hook := filepath.Join(dir, "hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ncat >> "+dir+"/events\necho >> "+dir+"/events\n"), 0700); err != nil {
		t.Fatal(err)
	}
	const rootTS, replyTS, plainTS, otherTS = "1700000000.000001", "1700000010.000001", "1700000020.000001", "1700000030.000001"
	mention, mentionAck := mentionIn("mention", mainChannel, rootTS, "<@U012BOT> please look")
	reply, replyAck := threadMessage("reply", "message", mainChannel, replyTS, rootTS, "U87654321", "and this")
	plain, plainAck := threadMessage("plain", "message", mainChannel, plainTS, "", harnessUser, "just chatting")
	edit, editAck := threadMessage("edit", "message_changed", mainChannel, plainTS, "", harnessUser, "just chatting, edited")
	edit.EventTS = "1700000021.000001"
	other, otherAck := threadMessage("other", "message", pitchChannel, otherTS, "", harnessUser, "elsewhere")
	added, addedAck := reactionEvent("added", slackapi.EventReactionAdded, mainChannel, replyTS, harnessUser, "+1")
	again, againAck := reactionEvent("added", slackapi.EventReactionAdded, mainChannel, replyTS, harnessUser, "+1")
	removed, removedAck := reactionEvent("removed", slackapi.EventReactionRemoved, mainChannel, rootTS, harnessUser, "eyes")
	again.EnvelopeID = "env-added-redelivered"

	h := newSlackHarnessWith(t, nil, "[event_hooks.factory]\ncommand = \""+hook+"\"\nchannels = [\""+mainChannel+"\"]\n",
		[]fake.ReceiveStep{{Envelope: mention}, {Envelope: reply}, {Envelope: plain}, {Envelope: edit}, {Envelope: other}, {Envelope: added}, {Envelope: again}, {Envelope: removed}},
		steps(
			verificationSteps(),
			reactionSteps(fake.AddReaction, mainChannel, rootTS, "hourglass_flowing_sand"),
			reactionSteps(fake.AddReaction, mainChannel, replyTS, "hourglass_flowing_sand"),
		))
	waitAcks(t, mentionAck, replyAck, plainAck, editAck, otherAck, addedAck, againAck, removedAck)
	h.waitWebDone()

	var events []map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(dir, "events"))
		events = events[:0]
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var e map[string]any
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("hook stdin is not JSON: %q", line)
			}
			events = append(events, e)
		}
		if len(events) >= 6 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := make([]string, len(events))
	for i, e := range events {
		got[i] = strings.Join([]string{present(e["kind"]), present(e["channel"]), present(e["ts"]), present(e["thread_ts"]), present(e["user"]), present(e["reaction"])}, " ")
	}
	want := []string{
		"app_mention " + mainChannel + " " + rootTS + "  " + harnessUser + " ",
		"message " + mainChannel + " " + replyTS + " " + rootTS + " U87654321 ",
		"message " + mainChannel + " " + plainTS + "  " + harnessUser + " ",
		"message_changed " + mainChannel + " " + plainTS + "  " + harnessUser + " ",
		"reaction_added " + mainChannel + " " + replyTS + "  " + harnessUser + " +1",
		"reaction_removed " + mainChannel + " " + rootTS + "  " + harnessUser + " eyes",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("hook events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, e := range events {
		if e["version"] != float64(1) || e["text"] != nil {
			t.Fatalf("event = %v", e)
		}
	}
	// Admission is unchanged: the mention and the thread reply are tasks;
	// the plain message, its edit and the other channel's message are
	// ledger records only; reactions leave no record at all.
	if n := len(h.taskIDs()); n != 2 {
		t.Fatalf("tasks = %d", n)
	}
	if n := h.count("SELECT count(*) FROM slack_source_events"); n != 5 {
		t.Fatalf("source events = %d", n)
	}
	if status := string(h.run("", "gateway", "status", "--json")); !strings.Contains(status, `"event_hooks":{"configured":1,"delivered":6,"dropped":0,"duplicates":1,"failed":0,"queued":6}`) {
		t.Fatalf("gateway status = %s", status)
	}
}

func present(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
