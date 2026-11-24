// Package orchstatus assembles a snapshot of what an orchestrating Claude
// Code session is doing: its newest tool calls and last message from the
// tail of its transcript, its child sessions in tmux, and Cue's inbox and
// queue. Every text field is redacted and capped, so the snapshot is safe
// to post elsewhere.
package orchstatus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/build-great-products/cue/internal/protocol"
)

// Snapshot is the reporter's output, version 1.
type Snapshot struct {
	Version      int          `json:"version"`
	GeneratedAt  string       `json:"generatedAt"`
	Orchestrator Orchestrator `json:"orchestrator"`
	Now          []Action     `json:"now"`
	LastMessage  *Message     `json:"lastMessage"`
	Children     []Child      `json:"children"`
	Inbox        []Task       `json:"inbox"`
	Queue        []Task       `json:"queue"`
	Errors       []string     `json:"errors"`
}

// Orchestrator is the orchestrating session itself.
type Orchestrator struct {
	State     string  `json:"state"`
	SessionID *string `json:"sessionId"`
}

// Child is one child session (a tmux session named with the child prefix).
type Child struct {
	Name       string  `json:"name"`
	Purpose    string  `json:"purpose"`
	State      string  `json:"state"`
	StartedAt  *string `json:"startedAt"`
	LastAction *Action `json:"lastAction"`
	PRs        []PR    `json:"prs"`
	Tickets    []int   `json:"tickets"`
}

// Task is a Cue task in the inbox (open, working) or the queue (queued).
type Task struct {
	ID        string  `json:"id"`
	State     string  `json:"state"`
	Title     string  `json:"title"`
	Notes     *string `json:"notes"`
	Source    string  `json:"source"`
	CreatedAt string  `json:"createdAt"`
	URL       *string `json:"url"`
	SlackURL  *string `json:"slackUrl"`
	Tickets   []int   `json:"tickets"`
}

// TaskLister lists Cue tasks in one state.
type TaskLister func(ctx context.Context, state string) ([]protocol.TaskDTO, error)

// Options are where the reporter looks. Zero values take the defaults.
type Options struct {
	Home           string
	ProjectsDir    string // ~/.claude/projects
	SessionsDir    string // ~/.claude/sessions
	Session        string // the orchestrator's tmux session, "claude"
	ChildPrefix    string // "claude-"
	Transcript     string // fallback transcript path
	SlackWorkspace string // for thread links, e.g. "rough-app"
	Tmux           string
	Run            Runner
	Tasks          TaskLister
	Now            func() time.Time
}

const (
	nowLimit          = 30
	orchestratorBytes = 2 << 20
	childBytes        = 512 << 10
	briefBytes        = 64 << 10
	maxChildren       = 30
	maxInbox          = 50
	maxQueue          = 100
	maxPurposeLen     = 300
	maxTitleLen       = 300
	maxNotesLen       = 500
	maxErrors         = 10
	maxErrorLen       = 300
)

func (o *Options) defaults() {
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	if o.ProjectsDir == "" {
		o.ProjectsDir = filepath.Join(o.Home, ".claude", "projects")
	}
	if o.SessionsDir == "" {
		o.SessionsDir = filepath.Join(o.Home, ".claude", "sessions")
	}
	if o.Session == "" {
		o.Session = "claude"
	}
	if o.ChildPrefix == "" {
		o.ChildPrefix = "claude-"
	}
	if o.Tmux == "" {
		o.Tmux = "tmux"
	}
	if o.Run == nil {
		o.Run = execRunner
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Build assembles a snapshot. A source that fails is noted in Errors and
// the rest still comes back.
func Build(ctx context.Context, o Options) Snapshot {
	o.defaults()
	s := Snapshot{Version: 1, GeneratedAt: o.Now().UTC().Format(time.RFC3339), Orchestrator: Orchestrator{State: "unknown"}, Now: []Action{}, Children: []Child{}, Inbox: []Task{}, Queue: []Task{}, Errors: []string{}}
	fail := func(format string, args ...any) {
		if len(s.Errors) < maxErrors {
			s.Errors = append(s.Errors, Clean(fmt.Sprintf(format, args...), maxErrorLen, true))
		}
	}

	panes, err := ListPanes(ctx, o.Run, o.Tmux)
	if err != nil {
		fail("tmux: %v", err)
	}
	var orchestrator *Pane
	for i := range panes {
		if panes[i].Session == o.Session {
			orchestrator = &panes[i]
		}
	}

	// The orchestrator: its session file names its transcript; else the
	// newest transcript for its directory; else the configured one.
	transcript := ""
	if orchestrator != nil {
		cs, _ := ReadClaudeSession(o.SessionsDir, orchestrator.PID)
		pane, _ := CapturePane(ctx, o.Run, o.Tmux, o.Session)
		if Working(cs, pane) {
			s.Orchestrator.State = "working"
		} else if cs != nil || pane != "" {
			s.Orchestrator.State = "idle"
		}
		if cs != nil {
			id := Clean(cs.SessionID, 64, true)
			s.Orchestrator.SessionID = &id
			transcript = TranscriptPath(o.ProjectsDir, cs)
		}
		if transcript == "" {
			dir := orchestrator.Path
			if dir == "" {
				dir = o.Home
			}
			transcript = NewestTranscript(ProjectDir(o.ProjectsDir, dir))
		}
	} else {
		fail("tmux session %q is not running", o.Session)
	}
	if transcript == "" {
		transcript = o.Transcript
	}
	if transcript == "" {
		fail("no transcript for the orchestrator")
	} else if tail, err := ReadTail(transcript, orchestratorBytes, nowLimit); err != nil {
		fail("transcript: %v", err)
	} else {
		s.Now = tail.Actions
		s.LastMessage = tail.LastMessage
	}

	for _, p := range panes {
		if !strings.HasPrefix(p.Session, o.ChildPrefix) || len(s.Children) == maxChildren {
			continue
		}
		s.Children = append(s.Children, buildChild(ctx, o, p))
	}

	if o.Tasks != nil {
		for _, state := range []string{"working", "open"} {
			tasks, err := o.Tasks(ctx, state)
			if err != nil {
				fail("cue tasks (%s): %v", state, err)
				continue
			}
			for _, t := range tasks {
				if len(s.Inbox) < maxInbox {
					s.Inbox = append(s.Inbox, buildTask(t, o.SlackWorkspace))
				}
			}
		}
		if tasks, err := o.Tasks(ctx, "queued"); err != nil {
			fail("cue queue: %v", err)
		} else {
			for _, t := range tasks {
				if len(s.Queue) < maxQueue {
					s.Queue = append(s.Queue, buildTask(t, o.SlackWorkspace))
				}
			}
		}
	}
	return s
}

func buildChild(ctx context.Context, o Options, p Pane) Child {
	c := Child{Name: Clean(p.Session, 100, true), State: "idle", PRs: []PR{}, Tickets: []int{}}
	cs, _ := ReadClaudeSession(o.SessionsDir, p.PID)
	pane, _ := CapturePane(ctx, o.Run, o.Tmux, p.Session)
	if Working(cs, pane) {
		c.State = "working"
	}
	started := p.Created
	if cs != nil && cs.StartedAt > 0 {
		started = time.UnixMilli(cs.StartedAt).UTC()
	}
	if !started.IsZero() && started.Unix() > 0 {
		at := started.Format(time.RFC3339)
		c.StartedAt = &at
	}

	transcript := TranscriptPath(o.ProjectsDir, cs)
	brief := ""
	if path := BriefPath(p.StartCommand); path != "" {
		if b, err := readHead(path, briefBytes); err == nil {
			brief = b
		}
	}
	if brief == "" && transcript != "" {
		brief, _ = FirstUserText(transcript, 1<<20)
		if len(brief) > briefBytes {
			brief = brief[:briefBytes]
		}
	}
	c.Purpose = Clean(Purpose(brief), maxPurposeLen, true)
	if transcript != "" {
		if tail, err := ReadTail(transcript, childBytes, 1); err == nil && len(tail.Actions) > 0 {
			c.LastAction = &tail.Actions[0]
		}
	}
	dir := p.Path
	if cs != nil && cs.Cwd != "" {
		dir = cs.Cwd
	}
	repo := ""
	if statusPR.MatchString(lastLines(pane, 4)) && dir != "" {
		repo = RepoOf(ctx, o.Run, dir)
	}
	c.PRs = PRs(brief, pane, repo)
	c.Tickets = Tickets(brief)
	return c
}

func readHead(path string, max int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, max)
	n, err := f.Read(b)
	if n == 0 && err != nil {
		return "", err
	}
	return string(b[:n]), nil
}

func buildTask(t protocol.TaskDTO, slackWorkspace string) Task {
	out := Task{ID: Clean(t.TaskID, 64, true), State: t.State, Source: t.SourceKind, Tickets: []int{}}
	if t.Source != nil && t.Source.Name != "" {
		out.Source = t.Source.Name
	}
	out.Source = Clean(out.Source, 32, true)
	if c, err := time.Parse(time.RFC3339Nano, t.CreatedAt); err == nil {
		out.CreatedAt = c.UTC().Format(time.RFC3339)
	} else {
		out.CreatedAt = time.Unix(0, 0).UTC().Format(time.RFC3339)
	}

	title, notes := splitTask(t.Text)
	out.Title = Clean(prettyTitle(title), maxTitleLen, true)
	if out.Title == "" {
		out.Title = "(no text)"
	}
	if notes != "" {
		n := Clean(notes, maxNotesLen, false)
		out.Notes = &n
	}

	refs := t.Text
	if t.Source != nil && t.Source.URL != "" {
		u := Redact(t.Source.URL)
		if len(u) <= 2048 && isHTTP(u) {
			out.URL = &u
		}
		refs += "\n" + t.Source.URL
	}
	if link := slackLink(t, slackWorkspace); link != "" {
		out.SlackURL = &link
	}
	out.Tickets = Tickets(refs)
	return out
}

// splitTask is a task's first line and the rest (its notes).
func splitTask(text string) (string, string) {
	text = strings.TrimSpace(text)
	first, rest, _ := strings.Cut(text, "\n")
	return strings.TrimSpace(first), strings.TrimSpace(rest)
}

// prettyTitle reads a factory feeder line ("FACTORY_EVENT ticket=12
// type=comment author=george@rough.app text=...") as "#12 comment by
// george@rough.app: ...". Anything else is as it is.
func prettyTitle(s string) string {
	if !strings.HasPrefix(s, "FACTORY_EVENT ") {
		return s
	}
	fields := map[string]string{}
	rest := strings.TrimPrefix(s, "FACTORY_EVENT ")
	for _, key := range []string{"ticket", "type", "author"} {
		prefix := key + "="
		if strings.HasPrefix(rest, prefix) {
			value, tail, _ := strings.Cut(strings.TrimPrefix(rest, prefix), " ")
			fields[key], rest = value, tail
		}
	}
	text := strings.TrimPrefix(rest, "text=")
	title := "#" + fields["ticket"] + " " + strings.ReplaceAll(fields["type"], "_", " ")
	if fields["author"] != "" {
		title += " by " + fields["author"]
	}
	if text != "" {
		title += ": " + text
	}
	return title
}

func slackLink(t protocol.TaskDTO, workspace string) string {
	if t.SlackSource != nil {
		if t.SlackSource.Permalink != "" && isHTTP(t.SlackSource.Permalink) {
			return t.SlackSource.Permalink
		}
		if workspace != "" {
			thread := t.SlackSource.ThreadTS
			link := fmt.Sprintf("https://%s.slack.com/archives/%s/p%s", workspace, t.SlackSource.ChannelID, strings.ReplaceAll(t.SlackSource.MessageTS, ".", ""))
			if thread != "" && thread != t.SlackSource.MessageTS {
				link += "?thread_ts=" + thread + "&cid=" + t.SlackSource.ChannelID
			}
			return link
		}
	}
	if t.Source != nil && t.Source.ReplySlack != nil && workspace != "" {
		return fmt.Sprintf("https://%s.slack.com/archives/%s/p%s", workspace, t.Source.ReplySlack.ChannelID, strings.ReplaceAll(t.Source.ReplySlack.ThreadTS, ".", ""))
	}
	return ""
}

func isHTTP(s string) bool {
	return (strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) && !strings.ContainsAny(s, " \t\n")
}
