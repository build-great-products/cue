package app

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/build-great-products/cue/internal/protocol"
)

// TestQueuedTaskRequests locks the wire shape of queued creates, --title,
// task add, task queue, and task list --state queued, and that each needs
// task_queued_v1 only when it uses queued.
func TestQueuedTaskRequests(t *testing.T) {
	endpoint := characterizationEndpoint(t)
	queued := []string{protocol.CapabilityLocalTasks, protocol.CapabilityTaskQueued}
	cases := []struct {
		name         string
		args         []string
		capabilities []string
		command      string
		params       any
	}{
		{"create queued", []string{"task", "create", "--text", "later", "--state", "queued", "--request-id", "req_q_create"}, queued, "task.create", protocol.TaskCreateParams{Text: "later", State: "queued"}},
		{"create open stays off the wire", []string{"task", "create", "--text", "now", "--state", "open", "--request-id", "req_q_open"}, []string{protocol.CapabilityLocalTasks}, "task.create", protocol.TaskCreateParams{Text: "now"}},
		{"add alias with title and notes", []string{"task", "add", "--title", "Ship the thing", "--notes", "first line\nsecond line", "--state", "queued", "--request-id", "req_q_add"}, queued, "task.create", protocol.TaskCreateParams{Text: "Ship the thing\n\nfirst line\nsecond line", State: "queued"}},
		{"title only", []string{"task", "add", "--title", "Just a title", "--request-id", "req_q_title"}, []string{protocol.CapabilityLocalTasks}, "task.create", protocol.TaskCreateParams{Text: "Just a title"}},
		{"queue", []string{"task", "queue", "abcdefghijklmnopqrstuv", "--note", "later", "--request-id", "req_q_queue"}, queued, "task.transition", protocol.TaskTransitionParams{Task: "abcdefghijklmnopqrstuv", State: "queued", Note: stringPointer("later")}},
		{"list queued", []string{"task", "list", "--state", "queued"}, queued, "task.list", protocol.TaskListParams{State: stringPointer("queued")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := serveCharacterizationGatewayWith(t, endpoint, tc.capabilities)
			var stdout, stderr bytes.Buffer
			if exit := (App{Out: &stdout, Err: &stderr, Version: "characterization"}).Run(context.Background(), tc.args); exit != ExitSuccess {
				t.Fatalf("Run(%q) exit=%d stdout=%q stderr=%q", tc.args, exit, stdout.String(), stderr.String())
			}
			req := <-requests
			if req.Command != tc.command {
				t.Fatalf("command=%q want %q", req.Command, tc.command)
			}
			got := reflect.New(reflect.TypeOf(tc.params)).Interface()
			if err := json.Unmarshal(req.Params, got); err != nil || !reflect.DeepEqual(reflect.ValueOf(got).Elem().Interface(), tc.params) {
				t.Fatalf("params=%s decoded=%#v err=%v want=%#v", req.Params, got, err, tc.params)
			}
			if tc.command == "task.create" && tc.params.(protocol.TaskCreateParams).State == "" && strings.Contains(string(req.Params), `"state"`) {
				t.Fatalf("open create sent a state: %s", req.Params)
			}
		})
	}
	for _, args := range [][]string{
		{"task", "create", "--text", "later", "--state", "queued"},
		{"task", "queue", "abcdefghijklmnopqrstuv"},
		{"task", "list", "--state", "queued"},
	} {
		t.Run("old gateway "+strings.Join(args, " "), func(t *testing.T) {
			_ = serveCharacterizationGatewayWith(t, endpoint, []string{protocol.CapabilityLocalTasks})
			var stdout, stderr bytes.Buffer
			if exit := (App{Out: &stdout, Err: &stderr, Version: "characterization"}).Run(context.Background(), args); exit != ExitUnsupported {
				t.Fatalf("Run(%q) exit=%d stdout=%q stderr=%q", args, exit, stdout.String(), stderr.String())
			}
		})
	}
}

func TestQueuedCreateFlagsAreValidatedLocally(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	for _, args := range [][]string{
		{"task", "create", "--text", "x", "--state", "working"},
		{"task", "create", "--text", "x", "--state", "done"},
		{"task", "create", "--text", "x", "--state", "later"},
		{"task", "create", "--title", "x", "--text", "y"},
		{"task", "create", "--title", "x", "--stdin"},
		{"task", "create", "--title", "x", "--text-file", "/dev/null"},
		{"task", "create", "--text", "x", "--notes", "y"},
		{"task", "create", "--title", ""},
		{"task", "create", "--title", "two\nlines"},
		{"task", "create", "--title", "x", "--notes", ""},
		{"task", "create"},
		{"task", "list", "--state", "later"},
		{"task", "queue"},
	} {
		var stdout, stderr bytes.Buffer
		if exit := (App{Out: &stdout, Err: &stderr}).Run(context.Background(), args); exit != ExitUsage {
			t.Errorf("Run(%q) exit=%d stderr=%q", args, exit, stderr.String())
		}
	}
}

func TestQueuedTaskCardShowsState(t *testing.T) {
	var out bytes.Buffer
	renderHumanValue(&out, map[string]any{"tasks": []any{map[string]any{"task_id": "abcdefghijklmnopqrstuv", "state": "queued", "source_kind": "local", "version": 1, "text": "Ship the thing\n\nnotes"}}})
	if !strings.HasPrefix(out.String(), "abcdefghijklmnopqrstuv  queued  local  v1\n│ Ship the thing\n│ \n│ notes\n") {
		t.Fatalf("card=%q", out.String())
	}
}
