package orchstatus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/build-great-products/cue/internal/protocol"
)

func TestReadTailNewestFirstSkipsSidechainsAndToolResults(t *testing.T) {
	tail, err := ReadTail("testdata/transcript.jsonl", 1<<20, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, a := range tail.Actions {
		got = append(got, a.Tool+": "+a.Description)
	}
	want := []string{
		"Bash: Post with token=[redacted]",
		"Bash: curl -H 'Authorization: [redacted] [redacted]' https://x.test",
		"Edit: book.ts",
		"Bash: List the worktree",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("actions:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if tail.Actions[0].At != "2026-11-25T07:00:06.000Z" {
		t.Fatalf("at = %q", tail.Actions[0].At)
	}
	if tail.LastMessage == nil || tail.LastMessage.Text != "Done: the booking page is fixed.\n\nPR is up." {
		t.Fatalf("last message = %+v", tail.LastMessage)
	}
}

func TestReadTailReadsOnlyTheEnd(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// A huge first line that would blow any budget if it were read whole.
	path := filepath.Join(t.TempDir(), "t.jsonl")
	big := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("x", 4<<20) + `"}]}}` + "\n"
	if err := os.WriteFile(path, append([]byte(big), data...), 0600); err != nil {
		t.Fatal(err)
	}
	tail, err := ReadTail(path, int64(len(data))+100, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Actions) != 2 || tail.Actions[1].Description != "curl -H 'Authorization: [redacted] [redacted]' https://x.test" {
		t.Fatalf("actions = %+v", tail.Actions)
	}
	if !strings.HasPrefix(tail.LastMessage.Text, "Done") {
		t.Fatalf("the cut first line was parsed: %q", tail.LastMessage.Text[:20])
	}
}

func TestFirstUserTextAndPurpose(t *testing.T) {
	text, err := FirstUserText("testdata/transcript.jsonl", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := Purpose(text); got != "Brief" {
		t.Fatalf("purpose = %q", got)
	}
	if got := Purpose("\n---\n**Fix the CRM** booking page.\n"); got != "Fix the CRM** booking page." {
		t.Fatalf("purpose = %q", got)
	}
	if got := Tickets(text); len(got) != 1 || got[0] != 396 {
		t.Fatalf("tickets = %v", got)
	}
	prs := PRs(text, "status · PR #2909 · esc to interrupt", "build-great-products/rough.app")
	if len(prs) != 2 || prs[0].Number != 2909 || *prs[0].URL != "https://github.com/build-great-products/rough.app/pull/2909" || prs[1].Number != 2889 {
		t.Fatalf("prs = %+v", prs)
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"export SLACK_BOT_TOKEN=xoxb-123-456-abcdefgh":                         "export SLACK_BOT_TOKEN=[redacted]",
		"use xoxb-1234567890-abcdefghij now":                                   "use [redacted] now",
		`-H "CF-Access-Client-Secret: 0123456789abcdef0123456789abcdef"`:       `-H "CF-Access-Client-Secret: [redacted]"`,
		"Authorization: Bearer abc.def.ghi-jkl":                                "Authorization: [redacted] [redacted]",
		"clone https://bot:hunter2secret@github.com/x/y":                       "clone https://[redacted]@github.com/x/y",
		"key ghp_abcdefghijklmnopqrstuvwxyz0123 here":                          "key [redacted] here",
		"sha 4f2a9c1e8b7d6a5f4e3d2c1b0a9f8e7d6c5b4a39 merged":                  "sha [redacted] merged",
		"blob QmFzZTY0IGVuY29kZWQgc2VjcmV0IHZhbHVlIDEyMzQ1Njc4OTA= end":        "blob [redacted] end",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----": "[redacted]",
		// Not secrets: paths, ids, prose.
		"edit src/lib/server/orchestrator-status/status.ts": "edit src/lib/server/orchestrator-status/status.ts",
		"session 18e8a977-33b0-4ffe-8091-fe770f055927":      "session 18e8a977-33b0-4ffe-8091-fe770f055927",
		"Rotate the Slack token tomorrow":                   "Rotate the Slack token tomorrow",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapCountsUTF16(t *testing.T) {
	if got := Cap("abcdef", 4); got != "abc…" {
		t.Fatalf("got %q", got)
	}
	if got := Cap("😀😀😀", 4); got != "😀…" {
		t.Fatalf("got %q", got)
	}
	if got := Cap("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
}

func TestWorking(t *testing.T) {
	if !Working(&ClaudeSession{Status: "shell"}, "") || !Working(&ClaudeSession{Status: "busy"}, "") {
		t.Fatal("busy and shell sessions are working")
	}
	if Working(&ClaudeSession{Status: "idle"}, "talked about a monitor earlier\n\n\n\n\n❯ \n───\n  ⏵⏵ bypass permissions on\n") {
		t.Fatal("an idle pane is idle")
	}
	if !Working(nil, "x\n  ⏵⏵ bypass permissions on · 1 shell · ↓ to manage\n") {
		t.Fatal("a running shell is working")
	}
	if !Working(nil, "  ⏵⏵ bypass permissions on · 5 monitors · esc to interrupt\n") {
		t.Fatal("esc to interrupt is working")
	}
}

func TestBriefPathAndSlug(t *testing.T) {
	cmd := `"claude --remote-control claude-x --dangerously-skip-permissions \"$(cat /tmp/s/brief.md)\""`
	if got := BriefPath(cmd); got != "/tmp/s/brief.md" {
		t.Fatalf("brief path = %q", got)
	}
	if got := ProjectDir("/p", "/home/jacob/rough.app/.claude/worktrees/x"); got != "/p/-home-jacob-rough-app--claude-worktrees-x" {
		t.Fatalf("project dir = %q", got)
	}
}

func TestPrettyTitleAndTasks(t *testing.T) {
	task := protocol.TaskDTO{TaskID: "abc", State: "open", SourceKind: "local", CreatedAt: "2026-11-25T07:00:00Z", Text: "FACTORY_EVENT ticket=12 type=comment author=george@rough.app text=Use the new photo.", Source: &protocol.TaskSourceDTO{Name: "factory", URL: "https://factory.rough.app/t/12"}}
	got := buildTask(task, "rough-app")
	if got.Title != "#12 comment by george@rough.app: Use the new photo." || got.Source != "factory" || *got.URL != "https://factory.rough.app/t/12" || len(got.Tickets) != 1 || got.Tickets[0] != 12 || got.Notes != nil {
		t.Fatalf("task = %+v", got)
	}
	slack := protocol.TaskDTO{TaskID: "s", State: "working", SourceKind: "slack", CreatedAt: "2026-11-25T07:00:00Z", Text: "<@U1> do it\nmore detail token=abc123secretvalue", SlackSource: &protocol.SlackSourceDTO{ChannelID: "C0BAQM6JEAJ", MessageTS: "1795382267.775129", ThreadTS: "1795382000.000001"}}
	got = buildTask(slack, "rough-app")
	if *got.SlackURL != "https://rough-app.slack.com/archives/C0BAQM6JEAJ/p1795382267775129?thread_ts=1795382000.000001&cid=C0BAQM6JEAJ" || got.URL != nil {
		t.Fatalf("slack url = %v", got.SlackURL)
	}
	if got.Title != "<@U1> do it" || *got.Notes != "more detail token=[redacted]" {
		t.Fatalf("task = %+v", got)
	}
}

type fakeTmux struct{ panes, capture map[string]string }

func (f fakeTmux) run(_ context.Context, name string, args ...string) ([]byte, error) {
	switch {
	case name == "tmux" && args[0] == "list-panes":
		return []byte(f.panes["list"]), nil
	case name == "tmux" && args[0] == "capture-pane":
		return []byte(f.capture[args[3]]), nil
	case name == "git":
		return []byte("git@github.com:build-great-products/rough.app.git\n"), nil
	}
	return nil, &exec.ExitError{}
}

func TestBuild(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	sessions := filepath.Join(home, ".claude", "sessions")
	childCwd := filepath.Join(home, "rough.app")
	for _, dir := range []string{ProjectDir(projects, home), ProjectDir(projects, childCwd), sessions} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixture, _ := os.ReadFile("testdata/transcript.jsonl")
	write := func(path, data string) {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(ProjectDir(projects, home), "orch.jsonl"), string(fixture))
	write(filepath.Join(ProjectDir(projects, childCwd), "kid.jsonl"), string(fixture))
	write(filepath.Join(sessions, "100.json"), `{"pid":100,"sessionId":"orch","cwd":"`+home+`","status":"busy"}`)
	write(filepath.Join(sessions, "200.json"), `{"pid":200,"sessionId":"kid","cwd":"`+childCwd+`","startedAt":1795540000000,"status":"idle"}`)
	brief := filepath.Join(home, "brief.md")
	write(brief, "\n# Ship the booking fix for ticket #396\n")

	tm := fakeTmux{
		panes: map[string]string{"list": strings.Join([]string{
			"claude\t100\t1795500000\t" + home + "\tclaude --resume orch",
			"claude-crm-book\t200\t1795500100\t" + childCwd + "\t\"claude --remote-control claude-crm-book \\\"$(cat " + brief + ")\\\"\"",
			"other\t300\t1795500200\t/tmp\tbash",
		}, "\n") + "\n"},
		capture: map[string]string{"=claude:": "x\n · esc to interrupt\n", "=claude-crm-book:": "❯ \n  ⏵⏵ bypass permissions on · PR #2909\n"},
	}
	tasks := map[string][]protocol.TaskDTO{
		"open":    {{TaskID: "o1", State: "open", SourceKind: "local", CreatedAt: "2026-11-25T07:00:00Z", Text: "open one"}},
		"working": {{TaskID: "w1", State: "working", SourceKind: "local", CreatedAt: "2026-11-25T07:00:00Z", Text: "working one, factory #410"}},
		"queued":  {{TaskID: "q1", State: "queued", SourceKind: "local", CreatedAt: "2026-11-25T07:00:00Z", Text: "Benchmark just qa\n\nWhen idle."}},
	}
	s := Build(context.Background(), Options{
		Home: home, Run: tm.run, SlackWorkspace: "rough-app",
		Now:   func() time.Time { return time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC) },
		Tasks: func(_ context.Context, state string) ([]protocol.TaskDTO, error) { return tasks[state], nil },
	})
	if len(s.Errors) != 0 {
		t.Fatalf("errors: %v", s.Errors)
	}
	if s.Orchestrator.State != "working" || *s.Orchestrator.SessionID != "orch" || len(s.Now) != 4 || s.GeneratedAt != "2026-11-25T08:00:00Z" {
		t.Fatalf("orchestrator: %+v now=%d", s.Orchestrator, len(s.Now))
	}
	if len(s.Children) != 1 {
		t.Fatalf("children: %+v", s.Children)
	}
	c := s.Children[0]
	if c.Name != "claude-crm-book" || c.State != "idle" || c.Purpose != "Ship the booking fix for ticket #396" || c.Tickets[0] != 396 || c.PRs[0].Number != 2909 || *c.StartedAt != "2026-11-24T17:06:40Z" || c.LastAction.Description != "Post with token=[redacted]" {
		t.Fatalf("child: %+v", c)
	}
	if len(s.Inbox) != 2 || s.Inbox[0].ID != "w1" || s.Inbox[0].Tickets[0] != 410 || len(s.Queue) != 1 || s.Queue[0].Title != "Benchmark just qa" || *s.Queue[0].Notes != "When idle." {
		t.Fatalf("inbox %+v queue %+v", s.Inbox, s.Queue)
	}
	b, _ := json.Marshal(s)
	for _, key := range []string{`"generatedAt"`, `"lastMessage"`, `"startedAt"`, `"lastAction"`, `"slackUrl"`, `"createdAt"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON lacks %s", key)
		}
	}
}

func TestBuildKeepsGoingWhenSourcesFail(t *testing.T) {
	s := Build(context.Background(), Options{
		Home: t.TempDir(),
		Run:  func(context.Context, string, ...string) ([]byte, error) { return nil, &exec.ExitError{} },
		Tasks: func(context.Context, string) ([]protocol.TaskDTO, error) {
			return nil, errors.New("gateway unavailable")
		},
	})
	if s.Orchestrator.State != "unknown" || len(s.Errors) < 3 || s.Now == nil || s.Children == nil || s.Inbox == nil {
		t.Fatalf("snapshot: %+v", s)
	}
}
