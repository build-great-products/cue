// Package format rewrites outgoing Slack mrkdwn before Cue sends it.
//
// A Formatter applies an ordered list of rules. Each rule rewrites only plain
// text: inline code, code blocks, Slack angle-bracket constructs (links, user
// and channel mentions, special mentions), and bare URLs are protected, and so
// is every link an earlier rule produced. The CLI formats text before it frames
// the request, so the gateway and the ledger see the formatted text.
package format

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Rule kinds accepted in config.toml.
const (
	KindGitHubLinks = "github_links"
	KindTicketLinks = "ticket_links"
)

// Defaults used when config.toml has no [formatting] table.
const (
	DefaultGitHubRepo   = "build-great-products/rough.app"
	DefaultTicketPrefix = "factory"
	DefaultTicketURL    = "https://factory.rough.app/t/{number}"
)

// RuleConfig is one [[formatting.rules]] entry. Which fields apply depends on
// Kind; Validate names any field set for the wrong kind.
type RuleConfig struct {
	Kind string `toml:"kind" json:"kind"`
	// github_links: the owner/name a bare #N links to.
	Repo string `toml:"repo" json:"repo,omitempty"`
	// github_links: words that mark #N as another tracker's number, so
	// "factory #12" is left alone. nil means the default (["factory"]); an
	// explicit empty list ignores none.
	IgnoreAfter []string `toml:"ignore_after" json:"ignore_after,omitempty"`
	// ticket_links: the word before #N, matched case-insensitively.
	Prefix string `toml:"prefix" json:"prefix,omitempty"`
	// ticket_links: the link target; {number} is replaced by N.
	URL string `toml:"url" json:"url,omitempty"`
}

// DefaultRules is the rule list used when config.toml does not set one.
func DefaultRules() []RuleConfig {
	return []RuleConfig{
		{Kind: KindTicketLinks, Prefix: DefaultTicketPrefix, URL: DefaultTicketURL},
		{Kind: KindGitHubLinks, Repo: DefaultGitHubRepo},
	}
}

// MaxRules bounds the configured rule list.
const MaxRules = 16

var (
	ownerPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	prefixPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,31}$`)
)

// Validate checks a rule list without building it.
func Validate(rules []RuleConfig) error {
	_, err := New(rules)
	return err
}

// Formatter applies its rules in order.
type Formatter struct {
	rules []rule
}

type rule interface {
	// apply rewrites one plain-text segment. Its output is a list of pieces;
	// linked pieces are protected from later rules.
	apply(text string) []piece
}

type piece struct {
	text      string
	protected bool
}

// New builds a Formatter from validated rule configs. An empty list formats
// nothing.
func New(rules []RuleConfig) (*Formatter, error) {
	if len(rules) > MaxRules {
		return nil, fmt.Errorf("formatting.rules: at most %d rules", MaxRules)
	}
	f := &Formatter{}
	for i, rc := range rules {
		name := fmt.Sprintf("formatting.rules[%d]", i)
		switch rc.Kind {
		case KindGitHubLinks:
			if rc.Prefix != "" || rc.URL != "" {
				return nil, fmt.Errorf("%s: prefix and url apply only to %s", name, KindTicketLinks)
			}
			owner, repo, ok := splitRepo(rc.Repo)
			if !ok {
				return nil, fmt.Errorf("%s.repo must be OWNER/NAME, for example %s", name, DefaultGitHubRepo)
			}
			ignore := rc.IgnoreAfter
			if ignore == nil {
				ignore = []string{DefaultTicketPrefix}
			}
			for _, w := range ignore {
				if !prefixPattern.MatchString(w) {
					return nil, fmt.Errorf("%s.ignore_after entries must be a letter then up to 31 of A-Z, a-z, 0-9, -", name)
				}
			}
			f.rules = append(f.rules, githubRule{owner: owner, repo: repo, ignoreAfter: ignore})
		case KindTicketLinks:
			if rc.Repo != "" || rc.IgnoreAfter != nil {
				return nil, fmt.Errorf("%s: repo and ignore_after apply only to %s", name, KindGitHubLinks)
			}
			if !prefixPattern.MatchString(rc.Prefix) {
				return nil, fmt.Errorf("%s.prefix must be a letter then up to 31 of A-Z, a-z, 0-9, -", name)
			}
			if err := checkTicketURL(rc.URL); err != nil {
				return nil, fmt.Errorf("%s.url %w", name, err)
			}
			f.rules = append(f.rules, ticketRule{prefix: rc.Prefix, url: rc.URL})
		default:
			return nil, fmt.Errorf("%s.kind must be %q or %q", name, KindGitHubLinks, KindTicketLinks)
		}
	}
	return f, nil
}

func splitRepo(value string) (string, string, bool) {
	owner, repo, ok := strings.Cut(value, "/")
	if !ok || !ownerPattern.MatchString(owner) || !repoPattern.MatchString(repo) || repo == "." || repo == ".." {
		return "", "", false
	}
	return owner, repo, true
}

func checkTicketURL(value string) error {
	if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
		return fmt.Errorf("must be an http(s) URL containing {number}")
	}
	if strings.Count(value, "{number}") != 1 {
		return fmt.Errorf("must contain {number} exactly once")
	}
	for _, r := range value {
		if r <= ' ' || r == '|' || r == '<' || r == '>' || r == 0x7f {
			return fmt.Errorf("must not contain spaces, control characters, |, <, or >")
		}
	}
	return nil
}

// Format applies every rule to text and returns the result.
func (f *Formatter) Format(text string) string {
	if f == nil || len(f.rules) == 0 {
		return text
	}
	pieces := split(text)
	for _, r := range f.rules {
		next := make([]piece, 0, len(pieces))
		for _, p := range pieces {
			if p.protected {
				next = append(next, p)
				continue
			}
			next = append(next, r.apply(p.text)...)
		}
		pieces = next
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, p := range pieces {
		b.WriteString(p.text)
	}
	return b.String()
}

// split divides mrkdwn into plain and protected pieces. Protected: ``` code
// blocks (an unclosed block runs to the end), `inline code` on one line,
// <...> on one line (links, mentions, <!here>), and bare URLs.
func split(text string) []piece {
	var out []piece
	plainStart := 0
	flush := func(end int) {
		if end > plainStart {
			out = append(out, piece{text: text[plainStart:end]})
		}
	}
	i := 0
	for i < len(text) {
		var end int
		switch {
		case strings.HasPrefix(text[i:], "```"):
			if j := strings.Index(text[i+3:], "```"); j >= 0 {
				end = i + 3 + j + 3
			} else {
				end = len(text)
			}
		case text[i] == '`':
			if j := strings.IndexAny(text[i+1:], "`\n"); j >= 0 && text[i+1+j] == '`' && j > 0 {
				end = i + 1 + j + 1
			}
		case text[i] == '<':
			if j := strings.IndexAny(text[i+1:], ">\n"); j >= 0 && text[i+1+j] == '>' {
				end = i + 1 + j + 1
			}
		default:
			if n := urlAt(text, i); n > 0 {
				end = i + n
			}
		}
		if end > i {
			flush(i)
			out = append(out, piece{text: text[i:end], protected: true})
			i = end
			plainStart = i
			continue
		}
		i++
	}
	flush(len(text))
	return out
}

var urlPattern = regexp.MustCompile(`^(?i:[a-z][a-z0-9+.-]*://|www\.|mailto:)[^\s<>]+`)

// urlAt returns the length of a bare URL starting at text[i], or 0. A URL
// must start at a word boundary.
func urlAt(text string, i int) int {
	c := text[i]
	if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		return 0
	}
	if i > 0 {
		prev, _ := utf8.DecodeLastRuneInString(text[:i])
		if isWord(prev) {
			return 0
		}
	}
	loc := urlPattern.FindStringIndex(text[i:])
	if loc == nil {
		return 0
	}
	return loc[1]
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// refPattern matches every candidate #N reference: an optional owner/repo, or
// an optional word and at most one space, then #digits. Boundaries are checked
// in code because Go's regexp has no lookaround.
var refPattern = regexp.MustCompile(`(?:([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._-]+)|([A-Za-z][A-Za-z0-9-]*)( ?))?#([0-9]+)`)

type ref struct {
	start, end  int // the whole match
	hashStart   int // index of '#'
	owner, repo string
	word        string // a word before #N, if any
	spaced      bool   // one space between word and #N
	number      string
	// startOK: the match (owner or word) starts on a boundary. hashOK: '#'
	// itself does. endOK: the number does not run on into a word.
	startOK, hashOK, endOK bool
}

// refs finds candidate references in plain text with their boundaries.
func refs(text string) []ref {
	var out []ref
	for _, m := range refPattern.FindAllStringSubmatchIndex(text, -1) {
		r := ref{start: m[0], end: m[1], hashStart: m[10] - 1, number: text[m[10]:m[11]]}
		if m[2] >= 0 {
			r.owner, r.repo = text[m[2]:m[3]], text[m[4]:m[5]]
		}
		if m[6] >= 0 {
			r.word = text[m[6]:m[7]]
			r.spaced = m[9] > m[8]
		}
		r.startOK = before(text, r.start)
		r.hashOK = before(text, r.hashStart)
		r.endOK = after(text, r.end)
		out = append(out, r)
	}
	return out
}

// before reports whether a reference may start at i: at the start of text or
// after a character that cannot be part of a word, path, entity, or channel.
func before(text string, i int) bool {
	if i == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:i])
	if isWord(prev) {
		return false
	}
	switch prev {
	case '&', '#', '/', '@', '\\', '-', '.', '_':
		// "_" is allowed only as the mrkdwn italic marker: "_#123_".
		if prev == '_' && (i < 2 || !isWordBefore(text, i-1)) {
			return true
		}
		return false
	}
	return true
}

func isWordBefore(text string, i int) bool {
	if i == 0 {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:i])
	return isWord(prev)
}

// after reports whether a reference may end at i: digits must not run on into
// a word (a colour like #123abc, a channel like #2024-planning, #123.4).
func after(text string, i int) bool {
	if i >= len(text) {
		return true
	}
	next, size := utf8.DecodeRuneInString(text[i:])
	if isWord(next) {
		return false
	}
	switch next {
	case '-', '_', '.', '/', '@', '#':
		if i+size >= len(text) {
			return true
		}
		follow, _ := utf8.DecodeRuneInString(text[i+size:])
		return !isWord(follow) && follow != '#'
	}
	return true
}

// githubRule links #N (3 to 5 digits) to repo and owner/repo#N to that repo.
type githubRule struct {
	owner, repo string
	ignoreAfter []string
}

func (g githubRule) apply(text string) []piece {
	var out []piece
	last := 0
	for _, r := range refs(text) {
		if !r.endOK {
			continue
		}
		start, owner, repo, label := r.start, g.owner, g.repo, ""
		if r.owner != "" {
			if !r.startOK || len(r.number) > 7 || strings.HasPrefix(r.number, "0") {
				continue
			}
			if _, _, ok := splitRepo(r.owner + "/" + r.repo); !ok {
				continue
			}
			owner, repo = r.owner, r.repo
			label = r.owner + "/" + r.repo + "#" + r.number
		} else {
			// "factory #12" belongs to another tracker. Any other word stays
			// plain text ahead of the link; "word#123" fails hashOK.
			if r.word != "" && r.startOK && g.ignored(r.word) {
				continue
			}
			if !r.hashOK {
				continue
			}
			if n := len(r.number); n < 3 || n > 5 {
				continue
			}
			start = r.hashStart
			label = "#" + r.number
		}
		out = appendPlain(out, text[last:start])
		out = append(out, piece{text: "<https://github.com/" + owner + "/" + repo + "/pull/" + r.number + "|" + label + ">", protected: true})
		last = r.end
	}
	return appendPlain(out, text[last:])
}

func (g githubRule) ignored(word string) bool {
	for _, w := range g.ignoreAfter {
		if strings.EqualFold(w, word) {
			return true
		}
	}
	return false
}

// ticketRule links "PREFIX #N" and "PREFIX#N" to url with N filled in.
type ticketRule struct {
	prefix, url string
}

func (t ticketRule) apply(text string) []piece {
	var out []piece
	last := 0
	for _, r := range refs(text) {
		if !r.startOK || !r.endOK || r.owner != "" || !strings.EqualFold(r.word, t.prefix) {
			continue
		}
		n, err := strconv.ParseUint(r.number, 10, 32)
		if err != nil {
			continue
		}
		number := strconv.FormatUint(n, 10)
		out = appendPlain(out, text[last:r.start])
		out = append(out, piece{text: "<" + strings.Replace(t.url, "{number}", number, 1) + "|" + r.word + " #" + number + ">", protected: true})
		last = r.end
	}
	return appendPlain(out, text[last:])
}

func appendPlain(out []piece, text string) []piece {
	if text == "" {
		return out
	}
	return append(out, piece{text: text})
}
