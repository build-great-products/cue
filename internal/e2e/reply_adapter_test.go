package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/build-great-products/cue/internal/protocol"
)

// fakeAdapter writes a reply adapter into a private directory under the
// home directory (the shared temporary directory may be group writable,
// which the gateway refuses). Each run appends its stdin to <dir>/runs and
// then does what <dir>/mode says: an exit code, or "sleep" to outlive its
// timeout.
func fakeAdapter(t *testing.T) (command, dir string) {
	t.Helper()
	dir, err := os.MkdirTemp(effectiveHome(t), ".cue-adapter-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	command = filepath.Join(dir, "reply")
	script := "#!/bin/sh\ncat >> " + dir + "/runs\necho >> " + dir + "/runs\nmode=$(cat " + dir + "/mode 2>/dev/null || echo 0)\n" +
		"if [ \"$mode\" = sleep ]; then sleep 30; fi\necho \"adapter mode $mode\" >&2\nexit \"$mode\"\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return command, dir
}

func setMode(t *testing.T, dir, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(mode+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func adapterRuns(t *testing.T, dir string) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "runs"))
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var run map[string]any
		if err := json.Unmarshal([]byte(line), &run); err != nil {
			t.Fatalf("adapter stdin is not JSON: %q", line)
		}
		out = append(out, run)
	}
	return out
}

// TestReplyAdapterDeliversTaskReplies drives the built CLI against a gateway
// with a fake reply adapter: a feeder task with --reply-via is answered by
// running the adapter with a JSON description on stdin, never through Slack.
// Exit 0 is confirmed (and --done completes the task), exit 3 is failed, any
// other exit or a timeout is ambiguous; failed and ambiguous leave the task
// open and are not run again.
func TestReplyAdapterDeliversTaskReplies(t *testing.T) {
	command, dir := fakeAdapter(t)
	h := newSlackHarnessWith(t, nil, "[reply_adapters.factory]\ncommand = \""+command+"\"\ntimeout = \"1s\"\n", nil, verificationSteps())
	create := func(key string) string {
		t.Helper()
		var created struct {
			Result protocol.TaskMutationDTO `json:"result"`
		}
		out := h.run("", "task", "create", "--text", "comment "+key, "--source", "factory", "--key", key, "--url", "https://factory.example/t/12", "--reply-via", "factory", "--json")
		if err := json.Unmarshal(out, &created); err != nil || created.Result.TaskID == "" {
			t.Fatalf("task create --reply-via: %v %s", err, out)
		}
		return created.Result.TaskID
	}
	first := create("event-1")

	watch := startWatchProcess(t, h.binary, h.env, "--since", "0")
	if got, want := watch.next(t), "source=factory channel= thread= parent= key=event-1 url=https://factory.example/t/12 reply= via=factory text=comment event-1"; !strings.HasSuffix(got, want) {
		t.Fatalf("watch line=%q want suffix %q", got, want)
	}
	watch.stop(t)
	if shown := string(h.run("", "task", "show", first)); !strings.Contains(shown, "via=factory") {
		t.Fatalf("task show = %q", shown)
	}

	// Refusals: an adapter the gateway does not have (so a feeder can fall
	// back), and both reply flags at once.
	if out, code := h.runExit("", "task", "create", "--text", "x", "--source", "factory", "--key", "event-x", "--reply-via", "nope", "--json"); code != 2 || !strings.Contains(string(out), "reply_adapter_not_configured") {
		t.Fatalf("unknown adapter exit=%d out=%s", code, out)
	}
	if out, code := h.runExit("", "task", "create", "--text", "x", "--source", "factory", "--reply-via", "factory", "--reply-slack", "C0TICKETS:1700000500.000100", "--json"); code != 2 {
		t.Fatalf("both flags exit=%d out=%s", code, out)
	}
	if n := h.count("SELECT count(*) FROM tasks"); n != 1 {
		t.Fatalf("tasks after refusals = %d", n)
	}

	// Confirmed, with --done.
	var replied struct {
		Result protocol.TaskReplyDTO `json:"result"`
	}
	out := h.run("", "task", "reply", first, "--text", "On it.\nSecond line", "--done", "--request-id", "reply-first", "--json")
	if err := json.Unmarshal(out, &replied); err != nil || replied.Result.AdapterEffect == nil || replied.Result.AdapterEffect.State != "confirmed" || replied.Result.Task == nil || replied.Result.Task.State != "done" {
		t.Fatalf("confirmed reply = %v %s", err, out)
	}
	if strings.Contains(string(out), `"effect":`) {
		t.Fatalf("an adapter reply carries a Slack effect: %s", out)
	}
	runs := adapterRuns(t, dir)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	run := runs[0]
	task, _ := run["task"].(map[string]any)
	if run["version"] != float64(1) || run["effect_id"] != replied.Result.AdapterEffect.EffectID || run["request_id"] != "reply-first" || run["adapter"] != "factory" ||
		run["text"] != "On it.\nSecond line" || task["id"] != first || task["source"] != "factory" || task["key"] != "event-1" || task["url"] != "https://factory.example/t/12" {
		t.Fatalf("adapter stdin = %v", run)
	}
	// The same request replays the recorded outcome without a second run.
	if again := h.run("", "task", "reply", first, "--text", "On it.\nSecond line", "--done", "--request-id", "reply-first", "--json"); !strings.Contains(string(again), replied.Result.AdapterEffect.EffectID) {
		t.Fatalf("replay = %s", again)
	}
	if n := len(adapterRuns(t, dir)); n != 1 {
		t.Fatalf("runs after replay = %d", n)
	}

	// Failed: exit 3.
	setMode(t, dir, "3")
	failed := create("event-2")
	if out, code := h.runExit("", "task", "reply", failed, "--text", "x", "--done", "--json"); code != 7 || !strings.Contains(string(out), "reply_adapter_failed") {
		t.Fatalf("failed reply exit=%d out=%s", code, out)
	}
	// Ambiguous: an unknown exit code, and a timeout.
	setMode(t, dir, "1")
	unknown := create("event-3")
	if out, code := h.runExit("", "task", "reply", unknown, "--text", "x", "--done", "--json"); code != 8 || !strings.Contains(string(out), "ambiguous_external_outcome") {
		t.Fatalf("ambiguous reply exit=%d out=%s", code, out)
	}
	setMode(t, dir, "sleep")
	slow := create("event-4")
	start := time.Now()
	if out, code := h.runExit("", "task", "reply", slow, "--text", "x", "--done", "--request-id", "reply-slow", "--json"); code != 8 {
		t.Fatalf("timed-out reply exit=%d out=%s", code, out)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("timeout took %s", took)
	}
	// Not retried: the same request answers ambiguous again without a run.
	before := len(adapterRuns(t, dir))
	setMode(t, dir, "0")
	if _, code := h.runExit("", "task", "reply", slow, "--text", "x", "--done", "--request-id", "reply-slow", "--json"); code != 8 {
		t.Fatalf("replayed ambiguous exit=%d", code)
	}
	if after := len(adapterRuns(t, dir)); after != before {
		t.Fatalf("an ambiguous reply ran again: %d -> %d", before, after)
	}
	for _, id := range []string{failed, unknown, slow} {
		if n := h.count("SELECT count(*) FROM tasks WHERE task_id=? AND state='open'", id); n != 1 {
			t.Fatalf("task %s was completed after a reply that did not confirm", id)
		}
	}
	if got := h.count("SELECT count(*) FROM reply_adapter_effects WHERE state='ambiguous'"); got != 2 {
		t.Fatalf("ambiguous effects = %d", got)
	}
	if got := h.count("SELECT count(*) FROM reply_adapter_effects WHERE state='failed' AND exit_code=3"); got != 1 {
		t.Fatalf("failed effects = %d", got)
	}
	var context struct {
		Effects protocol.TaskContextEffectsDTO `json:"effects"`
	}
	if out := h.run("", "task", "context", slow, "--json"); json.Unmarshal(out, &context) != nil || context.Effects.LatestAdapterReply == nil || context.Effects.LatestAdapterReply.DiagnosticCode != "timeout" {
		t.Fatalf("task context = %s", out)
	}
	if status := string(h.run("", "gateway", "status", "--json")); !strings.Contains(status, `"reply_adapters"`) || !strings.Contains(status, `"action_required":3`) {
		t.Fatalf("gateway status = %s", status)
	}
	// No Slack call was made for any of it.
	h.waitWebDone()
	if calls := h.web.Calls(); len(calls) != len(verificationSteps()) {
		t.Fatalf("Slack calls = %+v", calls)
	}
}
