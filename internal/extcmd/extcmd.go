// Package extcmd runs a configured external command (a reply adapter or an
// event hook) with no shell: an absolute path, no arguments, a JSON document
// on stdin, a deadline, and its own process group so that a timeout kills
// everything it started.
package extcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// MaxStderrBytes bounds the stderr kept from one run, for diagnostics.
const MaxStderrBytes = 2048

// Result is how one run ended. Exactly one of Started=false, TimedOut,
// Signaled, or an ExitCode applies.
type Result struct {
	// Started is false when the command could not be started at all, so it
	// did nothing.
	Started  bool
	TimedOut bool
	// Signaled is true when the process ended by a signal other than the
	// deadline's kill.
	Signaled bool
	ExitCode int
	// Stderr is the start of the command's stderr, with control characters
	// replaced, at most MaxStderrBytes.
	Stderr   string
	Duration time.Duration
	Err      error
}

// Run starts path with stdin as its standard input and waits at most
// timeout. Stdout is discarded. The environment is the gateway's.
func Run(ctx context.Context, path string, stdin []byte, timeout time.Duration) Result {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, path)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = io.Discard
	stderr := &boundedBuffer{limit: MaxStderrBytes}
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The whole group: a script's children (curl) must not outlive it.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// After a kill, stop waiting for pipes held open by stray descendants.
	cmd.WaitDelay = 2 * time.Second
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{Err: err, Duration: time.Since(started)}
	}
	err := cmd.Wait()
	out := Result{Started: true, Stderr: clean(stderr.String()), Duration: time.Since(started), Err: err}
	if runCtx.Err() != nil && ctx.Err() == nil {
		out.TimedOut = true
		return out
	}
	if ctx.Err() != nil {
		// The caller gave up (gateway shutdown): the outcome is unknown.
		out.TimedOut = true
		return out
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			out.Signaled = true
			return out
		}
		out.ExitCode = exitErr.ExitCode()
		return out
	}
	if err != nil {
		// Wait failed for another reason (for example the WaitDelay ran
		// out): treat it as unknown.
		out.Signaled = true
		return out
	}
	return out
}

type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

func clean(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return '?'
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}
