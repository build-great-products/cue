package extcmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunOutcomes(t *testing.T) {
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "stdin")
	if r := Run(ctx, script(t, "cat > "+out+"; echo fine >&2"), []byte(`{"a":1}`), 5*time.Second); !r.Started || r.ExitCode != 0 || r.TimedOut || r.Signaled || r.Stderr != "fine" {
		t.Fatalf("success = %+v", r)
	}
	if b, _ := os.ReadFile(out); string(b) != `{"a":1}` {
		t.Fatalf("stdin = %q", b)
	}
	if r := Run(ctx, script(t, "echo no >&2; exit 3"), nil, 5*time.Second); r.ExitCode != 3 || r.Stderr != "no" {
		t.Fatalf("exit 3 = %+v", r)
	}
	if r := Run(ctx, script(t, "kill -TERM $$"), nil, 5*time.Second); !r.Signaled {
		t.Fatalf("signal = %+v", r)
	}
	if r := Run(ctx, filepath.Join(t.TempDir(), "missing"), nil, time.Second); r.Started || r.Err == nil {
		t.Fatalf("missing = %+v", r)
	}
	if r := Run(ctx, script(t, strings.Repeat("printf xxxxxxxxxx >&2;", 400)), nil, 5*time.Second); len(r.Stderr) != MaxStderrBytes {
		t.Fatalf("stderr length = %d", len(r.Stderr))
	}
}

func TestRunKillsTheGroupOnTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	start := time.Now()
	r := Run(context.Background(), script(t, "(sleep 2; touch "+marker+") & sleep 30"), nil, 300*time.Millisecond)
	if !r.TimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout = %+v after %s", r, time.Since(start))
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a child of the timed-out command kept running")
	}
}
