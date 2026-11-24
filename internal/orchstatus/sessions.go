package orchstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Pane is one tmux session's first pane.
type Pane struct {
	Session      string
	PID          int
	Created      time.Time
	Path         string
	StartCommand string
}

// ClaudeSession is what Claude Code records about a running session in
// ~/.claude/sessions/<pid>.json.
type ClaudeSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
	Status    string `json:"status"`
}

// Runner runs a command and returns its stdout; tests replace it.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

const paneFormat = "#{session_name}\t#{pane_pid}\t#{session_created}\t#{pane_current_path}\t#{pane_start_command}"

// ListPanes lists the first pane of every tmux session. No tmux server is
// not an error: there is nothing running.
func ListPanes(ctx context.Context, run Runner, tmux string) ([]Pane, error) {
	out, err := run(ctx, tmux, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		// No server (no socket, or none running) is nothing running. Any
		// other failure, such as a client older than the server ("server
		// exited unexpectedly"), is an error.
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr := string(exit.Stderr)
			if strings.Contains(stderr, "no server running") || strings.Contains(stderr, "error connecting") {
				return nil, nil
			}
			return nil, fmt.Errorf("%s list-panes: %s", tmux, strings.TrimSpace(stderr))
		}
		return nil, err
	}
	seen := map[string]bool{}
	var panes []Pane
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.SplitN(line, "\t", 5)
		if len(f) < 5 || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		pid, _ := strconv.Atoi(f[1])
		created, _ := strconv.ParseInt(f[2], 10, 64)
		panes = append(panes, Pane{Session: f[0], PID: pid, Created: time.Unix(created, 0).UTC(), Path: f[3], StartCommand: f[4]})
	}
	sort.Slice(panes, func(i, j int) bool { return panes[i].Session < panes[j].Session })
	return panes, nil
}

// CapturePane is the visible text of a session's pane. The "=" makes the
// name exact: tmux would otherwise match "claude" to "claude-foo".
func CapturePane(ctx context.Context, run Runner, tmux, session string) (string, error) {
	out, err := run(ctx, tmux, "capture-pane", "-p", "-t", "="+session+":")
	return string(out), err
}

// ReadClaudeSession reads ~/.claude/sessions/<pid>.json.
func ReadClaudeSession(dir string, pid int) (*ClaudeSession, error) {
	b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(pid)+".json"))
	if err != nil {
		return nil, err
	}
	var s ClaudeSession
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

var slugUnsafe = regexp.MustCompile(`[^A-Za-z0-9]`)

// ProjectDir is where Claude Code keeps transcripts for a working
// directory: every character other than a letter or digit becomes "-".
func ProjectDir(projectsDir, cwd string) string {
	return filepath.Join(projectsDir, slugUnsafe.ReplaceAllString(cwd, "-"))
}

// TranscriptPath is a session's transcript, if it exists.
func TranscriptPath(projectsDir string, s *ClaudeSession) string {
	if s == nil || s.SessionID == "" || s.Cwd == "" {
		return ""
	}
	p := filepath.Join(ProjectDir(projectsDir, s.Cwd), s.SessionID+".jsonl")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// NewestTranscript is the most recently modified *.jsonl in dir.
func NewestTranscript(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(bestTime) {
			best, bestTime = filepath.Join(dir, e.Name()), info.ModTime()
		}
	}
	return best
}

// Working decides whether a Claude Code session is doing something: its
// session file says busy or running a shell, or its status line (the last
// lines of the pane) shows a running turn, shell or monitor.
func Working(s *ClaudeSession, pane string) bool {
	if s != nil && (s.Status == "busy" || s.Status == "shell") {
		return true
	}
	lines := strings.Split(strings.TrimRight(pane, "\n "), "\n")
	tail := 0
	for i := len(lines) - 1; i >= 0 && tail < 4; i-- {
		l := strings.ToLower(strings.TrimSpace(lines[i]))
		if l == "" {
			continue
		}
		tail++
		if strings.Contains(l, "esc to interrupt") || strings.Contains(l, "shell still running") {
			return true
		}
		// "1 shell", "5 monitors": only on the status line, whose parts are
		// separated by " · ", so a message that mentions a monitor does not count.
		if strings.Contains(l, " · ") && (strings.Contains(l, "monitor") || shellCount.MatchString(l)) {
			return true
		}
	}
	return false
}

var shellCount = regexp.MustCompile(`\b\d+ shells?\b`)

var briefCat = regexp.MustCompile(`\$\(cat\s+["']?([^"')\\]+)`)

// BriefPath is the file a child's start command reads its brief from:
// claude ... "$(cat /path/brief.md)".
func BriefPath(startCommand string) string {
	m := briefCat.FindStringSubmatch(startCommand)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// Purpose is the first meaningful line of a brief: not blank, not a rule or
// a code fence, without heading or emphasis marks.
func Purpose(brief string) string {
	for _, line := range strings.Split(brief, "\n") {
		l := strings.TrimSpace(line)
		l = strings.TrimLeft(l, "#>*-_ \t")
		l = strings.TrimRight(l, "*_ \t")
		if l == "" || strings.HasPrefix(l, "```") || strings.HasPrefix(l, "<") {
			continue
		}
		return l
	}
	return ""
}

var (
	factoryTicketURL = regexp.MustCompile(`factory\.rough\.app/t/(\d+)`)
	factoryTicketRef = regexp.MustCompile(`(?i)\b(?:factory(?: ticket)?|ticket)\s+#(\d+)`)
	factoryEventRef  = regexp.MustCompile(`\bFACTORY_EVENT\s+ticket=(\d+)`)
	githubPR         = regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/(\d+)`)
	statusPR         = regexp.MustCompile(`\bPR #(\d+)\b`)
)

// Tickets finds factory ticket numbers mentioned in text.
func Tickets(text string) []int {
	set := map[int]bool{}
	for _, re := range []*regexp.Regexp{factoryTicketURL, factoryTicketRef, factoryEventRef} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				set[n] = true
			}
		}
	}
	return sortedInts(set, 20)
}

// PR is a pull request a child is linked to.
type PR struct {
	Number int     `json:"number"`
	URL    *string `json:"url"`
}

// PRs finds pull requests: full GitHub URLs in the brief, and "PR #N" in
// the pane's status line, which belongs to the child's own repo.
func PRs(brief, pane, repo string) []PR {
	byNumber := map[int]*string{}
	var order []int
	add := func(n int, url *string) {
		if existing, ok := byNumber[n]; ok {
			if existing == nil && url != nil {
				byNumber[n] = url
			}
			return
		}
		byNumber[n] = url
		order = append(order, n)
	}
	for _, m := range statusPR.FindAllStringSubmatch(lastLines(pane, 4), -1) {
		n, _ := strconv.Atoi(m[1])
		var url *string
		if repo != "" {
			u := "https://github.com/" + repo + "/pull/" + m[1]
			url = &u
		}
		add(n, url)
	}
	for _, m := range githubPR.FindAllStringSubmatch(brief, -1) {
		n, _ := strconv.Atoi(m[2])
		u := m[0]
		add(n, &u)
	}
	out := []PR{}
	for _, n := range order {
		if len(out) == 20 {
			break
		}
		out = append(out, PR{Number: n, URL: byNumber[n]})
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n "), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var remoteRepo = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?$`)

// RepoOf is owner/repo of a checkout's origin on GitHub, or "".
func RepoOf(ctx context.Context, run Runner, dir string) string {
	out, err := run(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	m := remoteRepo.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return ""
	}
	return m[1]
}

func sortedInts(set map[int]bool, max int) []int {
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Ints(out)
	if len(out) > max {
		out = out[:max]
	}
	return out
}
