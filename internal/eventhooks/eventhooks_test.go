package eventhooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/build-great-products/cue/internal/config"
	"github.com/build-great-products/cue/internal/extcmd"
)

func hookScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeliversToMatchingHooksOnceAndInOrder(t *testing.T) {
	out := filepath.Join(t.TempDir(), "events")
	hooks := map[string]config.EventHook{
		"team":  {Command: hookScript(t, "cat >> "+out+"; echo >> "+out), Channels: []string{"C0TEAM"}},
		"other": {Command: hookScript(t, "exit 1"), Channels: []string{"C0OTHER"}},
	}
	r := New(func() map[string]config.EventHook { return hooks }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	r.Notify(Event{Kind: "message", EventID: "Ev1", Channel: "C0TEAM", TS: "1700000000.000001", User: "U1"})
	r.Notify(Event{Kind: "message", EventID: "Ev1", Channel: "C0TEAM", TS: "1700000000.000001", User: "U1"}) // redelivery
	r.Notify(Event{Kind: "reaction_added", EventID: "Ev2", Channel: "C0TEAM", TS: "1700000000.000001", User: "U2", Reaction: "+1"})
	r.Notify(Event{Kind: "message", EventID: "Ev3", Channel: "C0NOBODY", TS: "1700000000.000002"})
	waitFor(t, "two deliveries", func() bool { return r.Stats().Delivered == 2 })
	b, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("hook saw %q", b)
	}
	var first, second Event
	if json.Unmarshal([]byte(lines[0]), &first) != nil || json.Unmarshal([]byte(lines[1]), &second) != nil {
		t.Fatalf("not JSON: %q", b)
	}
	if first.Version != 1 || first.Kind != "message" || first.TS != "1700000000.000001" || second.Kind != "reaction_added" || second.Reaction != "+1" || second.ThreadTS != "" {
		t.Fatalf("events = %+v %+v", first, second)
	}
	if s := r.Stats(); s.Duplicates != 1 || s.Failed != 0 || s.Queued != 2 || s.Configured != 2 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestFailuresAreCountedAndNotRetried(t *testing.T) {
	count := filepath.Join(t.TempDir(), "runs")
	hooks := map[string]config.EventHook{"team": {Command: hookScript(t, "echo run >> "+count+"; echo boom >&2; exit 4")}}
	r := New(func() map[string]config.EventHook { return hooks }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	r.Notify(Event{Kind: "message", EventID: "Ev1", Channel: "C0TEAM", TS: "1700000000.000001"})
	waitFor(t, "a failure", func() bool { return r.Stats().Failed == 1 })
	time.Sleep(200 * time.Millisecond)
	if b, _ := os.ReadFile(count); string(b) != "run\n" {
		t.Fatalf("runs = %q", b)
	}
}

func TestAFullQueueDropsWithoutBlocking(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	hooks := map[string]config.EventHook{"slow": {Command: "/unused"}}
	r := New(func() map[string]config.EventHook { return hooks }, nil)
	r.run = func(context.Context, string, []byte, config.EventHook) extcmd.Result {
		<-release
		return extcmd.Result{Started: true}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer once.Do(func() { close(release) })
	start := time.Now()
	for i := 0; i < QueueSize+20; i++ {
		r.Notify(Event{Kind: "message", EventID: "Ev" + string(rune('a'+i%26)) + time.Now().Format("150405.000000000"), Channel: "C0TEAM", TS: "1700000000.000001"})
	}
	if time.Since(start) > time.Second {
		t.Fatal("Notify blocked")
	}
	if s := r.Stats(); s.Dropped == 0 || s.Queued > QueueSize+1 {
		t.Fatalf("stats = %+v", s)
	}
	once.Do(func() { close(release) })
}

func TestNoHooksMeansNothingQueued(t *testing.T) {
	r := New(func() map[string]config.EventHook { return nil }, nil)
	r.Notify(Event{Kind: "message", EventID: "Ev1", Channel: "C0TEAM"})
	if s := r.Stats(); s.Queued != 0 || s.Dropped != 0 {
		t.Fatalf("stats = %+v", s)
	}
}
