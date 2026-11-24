// Package task defines task domain values and text normalization.
package task

import (
	"errors"
	"strings"
	"unicode/utf8"
)

type State string

const (
	// Queued is promised work the agent will pick up later. It does not
	// wake a task watcher until it moves to open.
	Queued  State = "queued"
	Open    State = "open"
	Working State = "working"
	Done    State = "done"
)

func (s State) Valid() bool { return s == Queued || s == Open || s == Working || s == Done }

func CanTransition(from, to State) bool {
	return from.Valid() && to.Valid() && from != to
}

// NormalizeText makes line endings canonical and removes trailing spaces and
// tabs from each line without otherwise changing user content.
func NormalizeText(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", errors.New("task text is not valid UTF-8")
	}
	if strings.IndexByte(text, 0) >= 0 {
		return "", errors.New("task text contains NUL")
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	text = strings.Join(lines, "\n")
	if text == "" {
		return "", errors.New("task text is empty after normalization")
	}
	return text, nil
}

// Named task sources. A feeder labels the tasks it creates with a source
// name, may deduplicate them by an external key, and may link to the original
// item. The names "local" and "slack" are reserved for the built-in sources.
const (
	MaxSourceNameLength  = 32
	MaxExternalKeyLength = 256
	MaxSourceURLLength   = 2048
)

// ValidSourceName reports whether name is 1 to 32 characters of a-z, 0-9,
// and '-', and is not a reserved built-in source name.
func ValidSourceName(name string) bool {
	if len(name) < 1 || len(name) > MaxSourceNameLength || name == "local" || name == "slack" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return false
		}
	}
	return true
}

// ValidExternalKey reports whether key is 1 to 256 bytes of printable ASCII
// without spaces, so it fits on one task watch line unescaped.
func ValidExternalKey(key string) bool {
	return len(key) >= 1 && len(key) <= MaxExternalKeyLength && visibleASCII(key)
}

// ValidSourceURL reports whether link is an http or https URL of at most
// 2048 bytes of printable ASCII without spaces.
func ValidSourceURL(link string) bool {
	if len(link) < 1 || len(link) > MaxSourceURLLength || !visibleASCII(link) {
		return false
	}
	for _, prefix := range []string{"https://", "http://"} {
		if len(link) > len(prefix) && link[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func visibleASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '!' || s[i] > '~' {
			return false
		}
	}
	return true
}
