package orchstatus

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Action is one tool call from a Claude Code transcript.
type Action struct {
	At          string `json:"at"`
	Tool        string `json:"tool"`
	Description string `json:"description"`
}

// Message is an assistant text message.
type Message struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

// Tail is what the end of a transcript says: its newest tool calls, newest
// first, and its last assistant text.
type Tail struct {
	Actions     []Action
	LastMessage *Message
}

const (
	maxToolLen        = 64
	maxDescriptionLen = 500
	maxMessageLen     = 1000
)

type transcriptLine struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ReadTail reads only the last maxBytes of a transcript (it may be gigabytes)
// and returns up to limit tool calls, newest first, and the last text.
func ReadTail(path string, maxBytes int64, limit int) (Tail, error) {
	f, err := os.Open(path)
	if err != nil {
		return Tail{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Tail{}, err
	}
	start := info.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return Tail{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return Tail{}, err
	}
	if start > 0 {
		// The first line is cut; skip it.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return parseTail(data, limit), nil
}

var assistantMarker = []byte(`"type":"assistant"`)

func parseTail(data []byte, limit int) Tail {
	var actions []Action
	var last *Message
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		// Most lines are tool results and other records; skip them unparsed.
		if !bytes.Contains(line, assistantMarker) {
			continue
		}
		var l transcriptLine
		if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.IsSidechain {
			continue
		}
		var blocks []contentBlock
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			continue
		}
		at := normalizeTime(l.Timestamp)
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				actions = append(actions, Action{At: at, Tool: Clean(b.Name, maxToolLen, true), Description: Clean(describe(b.Name, b.Input), maxDescriptionLen, true)})
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					last = &Message{At: at, Text: Clean(b.Text, maxMessageLen, false)}
				}
			}
		}
	}
	// Newest first, at most limit.
	out := make([]Action, 0, min(limit, len(actions)))
	for i := len(actions) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, actions[i])
	}
	return Tail{Actions: out, LastMessage: last}
}

// describe is a tool call's description input (every Bash call has one),
// else a short summary of what it touched. Never a whole command.
func describe(tool string, raw json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	str := func(key string) string {
		s, _ := in[key].(string)
		return strings.TrimSpace(s)
	}
	if d := str("description"); d != "" {
		return d
	}
	for _, key := range []string{"file_path", "notebook_path", "path"} {
		if p := str(key); p != "" {
			return filepath.Base(p)
		}
	}
	// A call without a description: the start of its command, redacted
	// like everything else.
	if c := str("command"); c != "" {
		first, _, _ := strings.Cut(c, "\n")
		return Cap(Redact(first), 100)
	}
	for _, key := range []string{"skill", "pattern", "query", "url", "subject", "prompt"} {
		if s := str(key); s != "" {
			return s
		}
	}
	return tool
}

// normalizeTime keeps a timestamp as RFC 3339 in UTC, or empty.
func normalizeTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// FirstUserText is the first user message of a transcript, read from at
// most its first maxBytes: the brief a child was started with.
func FirstUserText(path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxBytes))
	sc.Buffer(make([]byte, 0, 64*1024), int(maxBytes))
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"type":"user"`)) {
			continue
		}
		var l transcriptLine
		if json.Unmarshal(line, &l) != nil || l.Type != "user" || l.IsSidechain {
			continue
		}
		var s string
		if json.Unmarshal(l.Message.Content, &s) == nil && strings.TrimSpace(s) != "" {
			return s, nil
		}
		var blocks []contentBlock
		if json.Unmarshal(l.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" && !strings.HasPrefix(strings.TrimSpace(b.Text), "<") {
					return b.Text, nil
				}
			}
		}
	}
	return "", sc.Err()
}
