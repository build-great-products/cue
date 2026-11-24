package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/build-great-products/cue/internal/config"
	"github.com/build-great-products/cue/internal/orchstatus"
	"github.com/build-great-products/cue/internal/protocol"
)

// statusCommand is `cue status`: a snapshot of what the orchestrating agent
// session is doing (its transcript tail, its child sessions in tmux, and
// Cue's inbox and queue), redacted and capped so it can be posted elsewhere.
// It reads local files and tmux directly; only the task lists go through
// the gateway, and a gateway that is down only empties them.
func (a App) statusCommand() *cobra.Command {
	var jsonOut bool
	var opts orchstatus.Options
	var timeout string
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Snapshot what the agent session and its children are doing.",
		Long:    "Snapshot what the orchestrating agent session is doing: its newest tool calls and last message (from the tail of its Claude Code transcript), its child sessions (tmux sessions named claude-*), and Cue's inbox (open and working tasks) and queue (queued tasks). Secrets are redacted and every field is capped.",
		Example: "  cue status --json\n  cue status --session claude --slack-workspace rough-app",
		Args:    noArgsWithSuggestions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			budget := 10 * time.Second
			if timeout != "" {
				parsed, err := time.ParseDuration(timeout)
				if err != nil || parsed <= 0 {
					return usage("--timeout must be a positive duration")
				}
				budget = parsed
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), budget)
			defer cancel()
			call := a
			call.ctx = ctx
			call.timeout = budget
			opts.Tasks = func(ctx context.Context, state string) ([]protocol.TaskDTO, error) {
				return call.statusTasks(state)
			}
			snapshot := orchstatus.Build(ctx, opts)
			if jsonOut {
				b, err := json.Marshal(snapshot)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(a.Out, string(b))
				return err
			}
			return renderStatus(a, snapshot)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&jsonOut, "json", false, "Render the snapshot as JSON.")
	f.StringVar(&opts.Session, "session", "claude", "tmux session of the orchestrating agent.")
	f.StringVar(&opts.ChildPrefix, "child-prefix", "claude-", "Name prefix of child agent tmux sessions.")
	f.StringVar(&opts.Transcript, "transcript", "", "Transcript to read when the session's own cannot be found.")
	f.StringVar(&opts.SlackWorkspace, "slack-workspace", "", "Slack workspace subdomain for thread links, such as rough-app.")
	f.StringVar(&opts.Tmux, "tmux", "tmux", "tmux binary; it must be the version the tmux server runs.")
	f.StringVar(&timeout, "timeout", "", "Overall time budget (default 10s).")
	return cmd
}

// statusTasks lists every task in one state, a page of at most 100.
func (a App) statusTasks(state string) ([]protocol.TaskDTO, error) {
	c, err := loadCommandConfig()
	if err != nil {
		return nil, err
	}
	out, err := a.gatewayCall(c, "task.list", "req_read_status_"+state, protocol.TaskListParams{State: &state, Limit: 100}, protocol.CapabilityLocalTasks)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	var page protocol.TaskListDTO
	if err := json.Unmarshal(b, &page); err != nil {
		return nil, err
	}
	// The list leaves out a Slack task's source; its thread link needs it.
	for i := range page.Tasks {
		t := &page.Tasks[i]
		if t.SourceKind != "slack" || t.SlackSource != nil || i >= statusSlackLookups {
			continue
		}
		if full, err := a.statusTask(c, t.TaskID); err == nil && full.SlackSource != nil {
			t.SlackSource = full.SlackSource
		}
	}
	return page.Tasks, nil
}

// statusSlackLookups bounds the task.get calls one status run makes.
const statusSlackLookups = 20

func (a App) statusTask(c config.Config, id string) (protocol.TaskDTO, error) {
	var t protocol.TaskDTO
	out, err := a.gatewayCall(c, "task.get", "req_read_status_task", protocol.TaskSelector{Task: id}, protocol.CapabilityLocalTasks)
	if err != nil {
		return t, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return t, err
	}
	err = json.Unmarshal(b, &t)
	return t, err
}

func renderStatus(a App, s orchstatus.Snapshot) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Orchestrator: %s\n", s.Orchestrator.State)
	for i, n := range s.Now {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "  %s  %s: %s\n", n.At, n.Tool, n.Description)
	}
	if s.LastMessage != nil {
		fmt.Fprintf(&b, "Last message (%s): %s\n", s.LastMessage.At, orchstatus.Cap(orchstatus.OneLine(s.LastMessage.Text), 200))
	}
	fmt.Fprintf(&b, "Children: %d\n", len(s.Children))
	for _, c := range s.Children {
		fmt.Fprintf(&b, "  %s  %s  %s\n", c.Name, c.State, c.Purpose)
	}
	fmt.Fprintf(&b, "Inbox: %d\n", len(s.Inbox))
	for _, t := range s.Inbox {
		fmt.Fprintf(&b, "  %s  %s  %s\n", t.ID, t.State, t.Title)
	}
	fmt.Fprintf(&b, "Queue: %d\n", len(s.Queue))
	for _, t := range s.Queue {
		fmt.Fprintf(&b, "  %s  %s\n", t.ID, t.Title)
	}
	for _, e := range s.Errors {
		fmt.Fprintf(&b, "Error: %s\n", e)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	for i := range lines {
		lines[i] = escapeTerminal(lines[i])
	}
	_, err := fmt.Fprintln(a.Out, strings.Join(lines, "\n"))
	return err
}
