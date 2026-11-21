// Package eventhooks tells configured commands about Slack message and
// reaction events. It is notification only: it never decides what becomes a
// task, runs off the Socket Mode path through a bounded queue, and drops,
// logs and never retries what it cannot deliver.
package eventhooks

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/build-great-products/cue/internal/config"
	"github.com/build-great-products/cue/internal/extcmd"
)

const (
	// QueueSize bounds events waiting for the worker. A full queue drops the
	// new event.
	QueueSize = 256
	// seenSize bounds the event IDs remembered to drop Slack redeliveries.
	seenSize = 4096
)

// Event is the JSON document a hook reads on stdin.
type Event struct {
	Version int `json:"version"`
	// Kind is message, message_changed, message_deleted, reaction_added or
	// reaction_removed.
	Kind    string `json:"kind"`
	EventID string `json:"event_id"`
	TeamID  string `json:"team_id"`
	Channel string `json:"channel"`
	// TS is the message the event is about: the new, edited or deleted
	// message, or the message reacted to.
	TS string `json:"ts"`
	// ThreadTS is the thread root of a reply, and empty for a top-level
	// message and for every reaction (Slack does not say).
	ThreadTS string `json:"thread_ts,omitempty"`
	// User is the author of a message, or who reacted. Empty for some bot
	// and deleted messages.
	User     string `json:"user,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	Reaction string `json:"reaction,omitempty"`
}

// Stats are process-local counters for gateway status.
type Stats struct {
	Configured int    `json:"configured"`
	Queued     uint64 `json:"queued"`
	Delivered  uint64 `json:"delivered"`
	Failed     uint64 `json:"failed"`
	Dropped    uint64 `json:"dropped"`
	Duplicates uint64 `json:"duplicates"`
}

// Runner owns the queue and its single worker.
type Runner struct {
	hooks func() map[string]config.EventHook
	log   *slog.Logger
	queue chan Event
	run   func(context.Context, string, []byte, config.EventHook) extcmd.Result

	mu       sync.Mutex
	seen     map[string]struct{}
	seenRing []string
	next     int

	queued, delivered, failed, dropped, duplicates atomic.Uint64
	startOnce                                      sync.Once
	done                                           chan struct{}
}

// New makes a runner that reads the active hooks from hooks on every event,
// so a configuration reload applies to the next event.
func New(hooks func() map[string]config.EventHook, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		hooks: hooks, log: log, queue: make(chan Event, QueueSize),
		seen: make(map[string]struct{}, seenSize), seenRing: make([]string, seenSize),
		done: make(chan struct{}),
		run: func(ctx context.Context, command string, stdin []byte, hook config.EventHook) extcmd.Result {
			return extcmd.Run(ctx, command, stdin, hook.TimeoutDuration())
		},
	}
}

// Start runs the worker until ctx ends. Events already queued then are
// dropped.
func (r *Runner) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		go func() {
			defer close(r.done)
			for {
				select {
				case <-ctx.Done():
					return
				case e := <-r.queue:
					r.deliver(ctx, e)
				}
			}
		}()
	})
}

// Wait returns once a started worker has stopped.
func (r *Runner) Wait() { <-r.done }

// Notify queues e for every hook that wants its channel. It never blocks:
// with no hook for the channel it does nothing, and with a full queue it
// drops e. A repeated event ID (Slack's redelivery) is dropped too.
func (r *Runner) Notify(e Event) {
	if r == nil || !r.wanted(e.Channel) {
		return
	}
	if !r.firstSighting(e.EventID) {
		r.duplicates.Add(1)
		return
	}
	e.Version = 1
	select {
	case r.queue <- e:
		r.queued.Add(1)
	default:
		r.dropped.Add(1)
		r.log.Warn("event hook queue full; event dropped", "kind", e.Kind, "channel", e.Channel, "ts", e.TS)
	}
}

// Stats returns the counters.
func (r *Runner) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	return Stats{Configured: len(r.hooks()), Queued: r.queued.Load(), Delivered: r.delivered.Load(), Failed: r.failed.Load(), Dropped: r.dropped.Load(), Duplicates: r.duplicates.Load()}
}

func (r *Runner) wanted(channel string) bool {
	for _, hook := range r.hooks() {
		if hook.Wants(channel) {
			return true
		}
	}
	return false
}

func (r *Runner) firstSighting(id string) bool {
	if id == "" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.seen[id]; ok {
		return false
	}
	if old := r.seenRing[r.next]; old != "" {
		delete(r.seen, old)
	}
	r.seenRing[r.next] = id
	r.seen[id] = struct{}{}
	r.next = (r.next + 1) % len(r.seenRing)
	return true
}

func (r *Runner) deliver(ctx context.Context, e Event) {
	body, err := json.Marshal(e)
	if err != nil {
		return
	}
	hooks := r.hooks()
	names := make([]string, 0, len(hooks))
	for name := range hooks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		hook := hooks[name]
		if !hook.Wants(e.Channel) {
			continue
		}
		result := r.run(ctx, hook.Command, body, hook)
		if result.Started && !result.TimedOut && !result.Signaled && result.ExitCode == 0 {
			r.delivered.Add(1)
			continue
		}
		r.failed.Add(1)
		r.log.Warn("event hook failed", "hook", name, "kind", e.Kind, "channel", e.Channel, "ts", e.TS,
			"started", result.Started, "timed_out", result.TimedOut, "signaled", result.Signaled,
			"exit_code", result.ExitCode, "stderr", result.Stderr)
	}
}
