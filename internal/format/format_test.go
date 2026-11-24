package format

import (
	"strings"
	"testing"
)

const (
	pr      = "https://github.com/build-great-products/rough.app/pull/"
	factory = "https://factory.rough.app/t/"
)

func link(n string) string { return "<" + pr + n + "|#" + n + ">" }

func TestDefaultRules(t *testing.T) {
	f, err := New(DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, in, want string
	}{
		// Bare references.
		{"bare", "#2909", link("2909")},
		{"sentence", "see #2909 for details", "see " + link("2909") + " for details"},
		{"three digits", "#123", link("123")},
		{"five digits", "#12345", link("12345")},
		{"two digits", "#12", "#12"},
		{"six digits", "#123456", "#123456"},
		{"several", "#123, #456 and #789.", link("123") + ", " + link("456") + " and " + link("789") + "."},
		{"start of line", "first\n#321 second", "first\n" + link("321") + " second"},
		{"word before", "PR #2909 merged", "PR " + link("2909") + " merged"},
		{"after dot word", "x.see #2909", "x.see " + link("2909")},
		{"sentence end", "Fixed in #2909.", "Fixed in " + link("2909") + "."},
		{"question", "is it #2909?", "is it " + link("2909") + "?"},
		{"colon", "PR:#2909", "PR:" + link("2909")},

		// Slack mrkdwn around a reference.
		{"bold", "*#123*", "*" + link("123") + "*"},
		{"italic", "_#123_", "_" + link("123") + "_"},
		{"strike", "~#123~", "~" + link("123") + "~"},
		{"parens", "(#123)", "(" + link("123") + ")"},
		{"brackets", "[#123]", "[" + link("123") + "]"},
		{"quotes", `"#123"`, `"` + link("123") + `"`},
		{"blockquote", "> #123 broke it", "> " + link("123") + " broke it"},
		{"list", "• #123\n- #456", "• " + link("123") + "\n- " + link("456")},
		{"bold sentence", "*Shipped:* #123", "*Shipped:* " + link("123")},

		// owner/repo references.
		{"qualified", "build-great-products/cue#12", "<https://github.com/build-great-products/cue/pull/12|build-great-products/cue#12>"},
		{"qualified long", "acme/web.app#1234567", "<https://github.com/acme/web.app/pull/1234567|acme/web.app#1234567>"},
		{"qualified in parens", "(acme/api#7)", "(<https://github.com/acme/api/pull/7|acme/api#7>)"},
		{"qualified leading zero", "acme/api#07", "acme/api#07"},

		// Factory tickets.
		{"factory spaced", "factory #123", "<" + factory + "123|factory #123>"},
		{"factory joined", "factory#123", "<" + factory + "123|factory #123>"},
		{"factory short", "factory #7 and #2909", "<" + factory + "7|factory #7> and " + link("2909")},
		{"factory capital", "Factory #45 is done", "<" + factory + "45|Factory #45> is done"},
		{"factory bold", "*factory #45*", "*<" + factory + "45|factory #45>*"},
		{"not factory word", "myfactory #2909", "myfactory " + link("2909")},

		// Left alone.
		{"inline code", "run `git show #2909` now", "run `git show #2909` now"},
		{"code block", "```\nfix #2909\n```\nthen #2910", "```\nfix #2909\n```\nthen " + link("2910")},
		{"unclosed code block", "```\nfix #2909", "```\nfix #2909"},
		{"unclosed backtick", "a ` b #2909", "a ` b " + link("2909")},
		{"slack link", "<https://example.com|#2909>", "<https://example.com|#2909>"},
		{"slack link label text", "<https://example.com|see #2909> and #2910", "<https://example.com|see #2909> and " + link("2910")},
		{"url fragment", "https://example.com/page#2909", "https://example.com/page#2909"},
		{"url in sentence", "see https://github.com/a/b/pull/1#issuecomment-2909 and #2910", "see https://github.com/a/b/pull/1#issuecomment-2909 and " + link("2910")},
		{"www url", "www.example.com/#2909", "www.example.com/#2909"},
		{"schemeless path", "example.com/foo#2909", "example.com/foo#2909"},
		{"channel mention", "<#C0BAQM6JEAJ> and <#C0BAQM6JEAJ|general>", "<#C0BAQM6JEAJ> and <#C0BAQM6JEAJ|general>"},
		{"user mention", "<@U012BOT> #2909", "<@U012BOT> " + link("2909")},
		{"special mention", "<!here>", "<!here>"},
		{"plain channel name", "#general and #2024-planning", "#general and #2024-planning"},
		{"colour letters", "#fff and #a1b2c3", "#fff and #a1b2c3"},
		{"colour digits first", "#1a2b3c and #123abc", "#1a2b3c and #123abc"},
		{"colour six digits", "#123456", "#123456"},
		{"no hash", "2909 tests passed", "2909 tests passed"},
		{"word joined", "abc#2909", "abc#2909"},
		{"double hash", "##2909", "##2909"},
		{"html entity", "&#2909;", "&#2909;"},
		{"path", "dir/#2909", "dir/#2909"},
		{"decimal", "#2909.5", "#2909.5"},
		{"already linked twice", "<" + pr + "2909|#2909>", "<" + pr + "2909|#2909>"},
		{"empty", "", ""},
		{"unicode", "修正 #2909 ✅", "修正 " + link("2909") + " ✅"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.Format(tc.in); got != tc.want {
				t.Fatalf("Format(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			// Formatting is idempotent: a second pass changes nothing.
			if got := f.Format(tc.want); got != tc.want {
				t.Fatalf("second pass changed %q to %q", tc.want, got)
			}
		})
	}
}

func TestConfiguredRules(t *testing.T) {
	cases := []struct {
		name  string
		rules []RuleConfig
		in    string
		want  string
	}{
		{"no rules", []RuleConfig{}, "#2909", "#2909"},
		{"other repo", []RuleConfig{{Kind: KindGitHubLinks, Repo: "acme/api"}}, "#2909", "<https://github.com/acme/api/pull/2909|#2909>"},
		{"github only still skips factory", []RuleConfig{{Kind: KindGitHubLinks, Repo: "acme/api"}}, "factory #2909 and factory#2910", "factory #2909 and factory#2910"},
		{"ignore none", []RuleConfig{{Kind: KindGitHubLinks, Repo: "acme/api", IgnoreAfter: []string{}}}, "factory #2909", "factory <https://github.com/acme/api/pull/2909|#2909>"},
		{"ignore custom", []RuleConfig{{Kind: KindGitHubLinks, Repo: "acme/api", IgnoreAfter: []string{"jira"}}}, "JIRA #2909", "JIRA #2909"},
		{"ticket only", []RuleConfig{{Kind: KindTicketLinks, Prefix: "jira", URL: "https://jira.example.com/browse/X-{number}"}}, "jira #12 and #2909", "<https://jira.example.com/browse/X-12|jira #12> and #2909"},
		{"github before ticket", []RuleConfig{{Kind: KindGitHubLinks, Repo: "acme/api"}, {Kind: KindTicketLinks, Prefix: "factory", URL: factory + "{number}"}}, "factory #2909", "<" + factory + "2909|factory #2909>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := New(tc.rules)
			if err != nil {
				t.Fatal(err)
			}
			if got := f.Format(tc.in); got != tc.want {
				t.Fatalf("Format(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestInvalidRules(t *testing.T) {
	cases := []struct {
		name  string
		rules []RuleConfig
		want  string
	}{
		{"unknown kind", []RuleConfig{{Kind: "emoji"}}, "kind must be"},
		{"missing repo", []RuleConfig{{Kind: KindGitHubLinks}}, "repo must be OWNER/NAME"},
		{"bad repo", []RuleConfig{{Kind: KindGitHubLinks, Repo: "acme/api/x"}}, "repo must be OWNER/NAME"},
		{"github with url", []RuleConfig{{Kind: KindGitHubLinks, Repo: "acme/api", URL: "https://x/{number}"}}, "apply only to ticket_links"},
		{"bad ignore", []RuleConfig{{Kind: KindGitHubLinks, Repo: "acme/api", IgnoreAfter: []string{"two words"}}}, "ignore_after"},
		{"ticket no prefix", []RuleConfig{{Kind: KindTicketLinks, URL: "https://x/{number}"}}, "prefix must be"},
		{"ticket no placeholder", []RuleConfig{{Kind: KindTicketLinks, Prefix: "jira", URL: "https://x/"}}, "{number} exactly once"},
		{"ticket not http", []RuleConfig{{Kind: KindTicketLinks, Prefix: "jira", URL: "javascript:{number}"}}, "http(s)"},
		{"ticket pipe", []RuleConfig{{Kind: KindTicketLinks, Prefix: "jira", URL: "https://x/{number}|y"}}, "must not contain"},
		{"ticket with repo", []RuleConfig{{Kind: KindTicketLinks, Prefix: "jira", URL: "https://x/{number}", Repo: "a/b"}}, "apply only to github_links"},
		{"too many", make([]RuleConfig, MaxRules+1), "at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.rules)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want containing %q", err, tc.want)
			}
		})
	}
}
