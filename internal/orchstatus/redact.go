package orchstatus

import (
	"regexp"
	"strings"
	"unicode"
)

// Redacted replaces whatever looked like a secret.
const Redacted = "[redacted]"

var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM blocks, whole.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), Redacted},
	// Authorization headers and bearer tokens.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 " + Redacted},
	// key=value and key: value for names that hold secrets, quoted or not.
	{regexp.MustCompile(`(?i)\b([A-Za-z0-9_-]*(?:token|secret|password|passwd|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret|authorization|cookie|session[_-]?key|signing[_-]?key)[A-Za-z0-9_-]*)(["']?\s*[:=]\s*["']?)[^\s"',;&]+`), "$1$2" + Redacted},
	// Credentials in URLs.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/:@]+:[^\s/@]+@`), "$1" + Redacted + "@"},
	// Well-known token shapes.
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{8,}`), Redacted},
	{regexp.MustCompile(`\bxapp-[A-Za-z0-9-]{8,}`), Redacted},
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`), Redacted},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), Redacted},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), Redacted},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), Redacted},
	// Long hex: keys, digests, commit hashes.
	{regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`), Redacted},
}

// base64ish finds long runs that may be base64; looksRandom decides.
var base64ish = regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}={0,2}`)

// Redact strips anything that looks like a secret from s.
func Redact(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return base64ish.ReplaceAllStringFunc(s, func(m string) string {
		if looksRandom(m) {
			return Redacted
		}
		return m
	})
}

// looksRandom tells a key or a base64 blob from a path or an identifier: it
// mixes upper case, lower case and digits, and is not mostly path segments.
func looksRandom(s string) bool {
	var upper, lower, digit int
	for _, r := range s {
		switch {
		case unicode.IsUpper(r):
			upper++
		case unicode.IsLower(r):
			lower++
		case unicode.IsDigit(r):
			digit++
		}
	}
	if upper < 3 || lower < 3 || digit < 3 {
		return false
	}
	// A path like src/lib/Foo2/Bar3/Baz4 has many short segments.
	if strings.Count(s, "/") >= 3 {
		segments := strings.Split(s, "/")
		short := 0
		for _, seg := range segments {
			if len(seg) <= 24 {
				short++
			}
		}
		if short == len(segments) {
			return false
		}
	}
	return true
}

// Cap shortens s to at most max UTF-16 code units (what JavaScript's
// string length counts, so factory's limits hold), ending in an ellipsis
// when it was cut.
func Cap(s string, max int) string {
	if utf16Len(s) <= max {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if n+w > max-1 {
			break
		}
		b.WriteRune(r)
		n += w
	}
	return strings.TrimRight(b.String(), " \t\n") + "…"
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// OneLine collapses whitespace runs, newlines included, to single spaces.
func OneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Clean is what every field goes through: redacted, optionally one line,
// then capped.
func Clean(s string, max int, oneLine bool) string {
	s = Redact(s)
	if oneLine {
		s = OneLine(s)
	} else {
		s = strings.TrimSpace(s)
	}
	return Cap(s, max)
}
