package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/build-great-products/cue/internal/protocol"
)

const (
	outgoingIn   = "Merged #2909 (see factory #12) `#2910`"
	outgoingWant = "Merged <https://github.com/build-great-products/rough.app/pull/2909|#2909> (see <https://factory.rough.app/t/12|factory #12>) `#2910`"
)

func TestOutgoingTextIsFormattedBeforeTheGateway(t *testing.T) {
	endpoint := characterizationEndpoint(t)
	file := filepath.Join(t.TempDir(), "result.txt")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, command string
		args          []string
		want          any
		capability    string
	}{
		{"task reply", "task.reply", []string{"task", "reply", "abcdefghijklmnopqrstuv", "--text", outgoingIn, "--request-id", "req_fmt_reply"}, protocol.TaskReplyParams{Task: "abcdefghijklmnopqrstuv", Text: outgoingWant}, protocol.CapabilityTaskReply},
		{"task reply raw", "task.reply", []string{"task", "reply", "abcdefghijklmnopqrstuv", "--text", outgoingIn, "--raw", "--request-id", "req_fmt_reply_raw"}, protocol.TaskReplyParams{Task: "abcdefghijklmnopqrstuv", Text: outgoingIn}, protocol.CapabilityTaskReply},
		{"slack send", "slack.send", []string{"slack", "send", "--text", outgoingIn, "--request-id", "req_fmt_send"}, protocol.SlackSendParams{Text: outgoingWant}, protocol.CapabilitySlackEffects},
		{"slack send raw", "slack.send", []string{"slack", "send", "--text", outgoingIn, "--raw", "--request-id", "req_fmt_send_raw"}, protocol.SlackSendParams{Text: outgoingIn}, protocol.CapabilitySlackEffects},
		{"slack edit", "slack.edit", []string{"slack", "edit", "1700000000.000001", "--text", outgoingIn, "--request-id", "req_fmt_edit"}, protocol.SlackEditParams{MessageTS: "1700000000.000001", Text: outgoingWant}, protocol.CapabilitySlackEffects},
		{"slack file comment", "slack.file", []string{"slack", "file", "--file", file, "--comment", outgoingIn, "--request-id", "req_fmt_file"}, protocol.SlackFileParams{FilePath: file, Comment: outgoingWant}, protocol.CapabilityFileTransfer},
		{"slack file comment raw", "slack.file", []string{"slack", "file", "--file", file, "--comment", outgoingIn, "--raw", "--request-id", "req_fmt_file_raw"}, protocol.SlackFileParams{FilePath: file, Comment: outgoingIn}, protocol.CapabilityFileTransfer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := serveCharacterizationGatewayWith(t, endpoint, []string{tc.capability, protocol.CapabilitySlackUnfurl})
			var out, err bytes.Buffer
			exit := (App{In: strings.NewReader(""), Out: &out, Err: &err, Version: "format-test"}).Run(context.Background(), tc.args)
			if exit != ExitSuccess {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, out.String(), err.String())
			}
			req := <-requests
			got := reflect.New(reflect.TypeOf(tc.want)).Interface()
			if req.Command != tc.command || json.Unmarshal(req.Params, got) != nil || !reflect.DeepEqual(reflect.ValueOf(got).Elem().Interface(), tc.want) {
				t.Fatalf("request=%+v params=%s want=%#v", req, req.Params, tc.want)
			}
		})
	}
}

func TestOutgoingDryRunSendsNothing(t *testing.T) {
	_ = characterizationEndpoint(t) // a valid config but no gateway listening
	for _, tc := range []struct {
		name  string
		input string
		args  []string
		want  string
	}{
		{"slack send", "", []string{"slack", "send", "--channel", "C0BAQM6JEAJ", "--text", outgoingIn, "--dry-run"}, outgoingWant + "\n"},
		{"slack send raw", "", []string{"slack", "send", "--text", outgoingIn, "--dry-run", "--raw"}, outgoingIn + "\n"},
		{"slack send stdin keeps newline", outgoingIn + "\n", []string{"slack", "send", "--stdin", "--dry-run"}, outgoingWant + "\n"},
		{"slack edit", "", []string{"slack", "edit", "1700000000.000001", "--text", outgoingIn, "--dry-run"}, outgoingWant + "\n"},
		{"task reply", "", []string{"task", "reply", "abcdefghijklmnopqrstuv", "--text", outgoingIn, "--done", "--dry-run"}, outgoingWant + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, err bytes.Buffer
			exit := (App{In: strings.NewReader(tc.input), Out: &out, Err: &err}).Run(context.Background(), tc.args)
			if exit != ExitSuccess || out.String() != tc.want {
				t.Fatalf("exit=%d stdout=%q stderr=%q, want %q", exit, out.String(), err.String(), tc.want)
			}
		})
	}
	var out, errOut bytes.Buffer
	exit := (App{Out: &out, Err: &errOut}).Run(context.Background(), []string{"slack", "send", "--text", outgoingIn, "--dry-run", "--json"})
	var got map[string]any
	if exit != ExitSuccess || json.Unmarshal(out.Bytes(), &got) != nil || got["text"] != outgoingWant || got["formatted"] != true || got["dry_run"] != true || got["raw"] != false {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, out.String(), errOut.String())
	}
	// Usage errors still apply to a dry run.
	out.Reset()
	errOut.Reset()
	exit = (App{Out: &out, Err: &errOut}).Run(context.Background(), []string{"slack", "send", "--text", "x", "--unfurl", "--file", "/tmp/x", "--dry-run"})
	if exit != ExitUsage || !strings.Contains(errOut.String(), "--unfurl applies only") {
		t.Fatalf("exit=%d stderr=%q", exit, errOut.String())
	}
}
