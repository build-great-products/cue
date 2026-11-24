package app

import (
	"fmt"
	"strings"

	"github.com/build-great-products/cue/internal/config"
	"github.com/build-great-products/cue/internal/format"
)

// outgoing carries the flags that control how text Cue posts is prepared.
type outgoing struct {
	raw    bool // --raw: send the text exactly as given
	dryRun bool // --dry-run: print the text that would be sent and stop
}

// formatOutgoing applies config.toml's formatting rules to text Cue is about
// to post, unless raw. Formatting runs in the CLI before the request is
// framed, so the gateway, the ledger, and request-ID replays all see the
// formatted text.
func formatOutgoing(c config.Config, text string, raw bool) (string, error) {
	if raw || text == "" {
		return text, nil
	}
	f, err := format.New(c.FormatRules())
	if err != nil {
		return "", err
	}
	return f.Format(text), nil
}

// renderDryRun prints the text that would be posted. Human output is the
// exact text (with a final newline added when missing); --json adds whether
// formatting changed it.
func (a App) renderDryRun(original, text string, raw, jsonOut bool) error {
	if jsonOut {
		return a.render(map[string]any{"dry_run": true, "raw": raw, "formatted": text != original, "text": text}, true)
	}
	if strings.HasSuffix(text, "\n") {
		_, err := fmt.Fprint(a.Out, text)
		return err
	}
	_, err := fmt.Fprintln(a.Out, text)
	return err
}
