package app

import (
	"fmt"
	"strings"

	"github.com/build-great-products/cue/internal/config"
	"github.com/build-great-products/cue/internal/identity"
	"github.com/build-great-products/cue/internal/protocol"
	"github.com/build-great-products/cue/internal/slackapi"
	"github.com/build-great-products/cue/internal/task"
)

// taskOrigin carries task create's optional named source flags. ReplySlack
// is the raw CHANNEL:THREAD_TS value of --reply-slack. State, Title, and
// Notes are task create's --state, --title, and --notes; TitleSet and
// NotesSet record that the flag was given.
type taskOrigin struct {
	Source, Key, URL, ReplySlack, ReplyVia string
	State, Title, Notes                    string
	TitleSet, NotesSet                     bool
}

// createBody reads task create's text: --title (with optional --notes) or
// exactly one of --text, --text-file, and --stdin. The stored text is the
// title, then a blank line and the notes when given.
func (a App) createBody(origin taskOrigin, text, textFile string, textSet, textFileSet, stdin bool) (string, error) {
	if origin.NotesSet && !origin.TitleSet {
		return "", usage("--notes requires --title")
	}
	if !origin.TitleSet {
		return a.readCommandTextSource(text, textFile, textSet, textFileSet, stdin)
	}
	if textSet || textFileSet || stdin {
		return "", usage("--title cannot be combined with --text, --text-file, or --stdin")
	}
	if strings.TrimSpace(origin.Title) == "" {
		return "", usage("--title must not be empty")
	}
	if strings.ContainsAny(origin.Title, "\r\n") {
		return "", usage("--title must be one line; put details in --notes")
	}
	body := origin.Title
	if origin.NotesSet {
		if strings.TrimSpace(origin.Notes) == "" {
			return "", usage("--notes must not be empty")
		}
		body += "\n\n" + origin.Notes
	}
	return validateCommandText(body)
}

// parseReplySlack splits and validates a --reply-slack CHANNEL:THREAD_TS value.
func parseReplySlack(value string) (*protocol.SlackThreadDTO, error) {
	channel, threadTS, ok := strings.Cut(value, ":")
	if !ok || slackapi.ValidateThreadTarget(channel, threadTS) != nil {
		return nil, usage("--reply-slack must be CHANNEL:THREAD_TS with a C or G channel ID and a Slack timestamp such as 1712345678.123456")
	}
	return &protocol.SlackThreadDTO{ChannelID: channel, ThreadTS: threadTS}, nil
}

// taskTyped binds Cobra's already-parsed task leaves directly to protocol DTOs.
// It retains the established task request IDs, text-file safety, and
// mutation JSON envelope.
func (a App) taskTyped(command, selector, text, textFile string, textSet, textFileSet, stdin bool, state string, all bool, limit int, note string, done, unfurl bool, requestID, cursor string, jsonOut bool, origin taskOrigin, send outgoing) error {
	mutation := command == "create" || command == "update" || command == "open" || command == "working" || command == "done" || command == "queue" || command == "reply"
	var params any

	switch command {
	case "create":
		body, err := a.createBody(origin, text, textFile, textSet, textFileSet, stdin)
		if err != nil {
			return err
		}
		if origin.State != "" && origin.State != "open" && origin.State != "queued" {
			return usage("--state must be open or queued")
		}
		if origin.Source == "" && (origin.Key != "" || origin.URL != "" || origin.ReplySlack != "" || origin.ReplyVia != "") {
			return usage("--key, --url, --reply-slack, and --reply-via require --source")
		}
		if origin.ReplySlack != "" && origin.ReplyVia != "" {
			return usage("--reply-slack and --reply-via are mutually exclusive")
		}
		if origin.ReplyVia != "" && !task.ValidSourceName(origin.ReplyVia) {
			return usage("--reply-via must be 1 to 32 characters of a-z, 0-9, and '-'")
		}
		if origin.Source != "" && !task.ValidSourceName(origin.Source) {
			return usage("--source must be 1 to 32 characters of a-z, 0-9, and '-', and not local or slack")
		}
		if origin.Key != "" && !task.ValidExternalKey(origin.Key) {
			return usage("--key must be 1 to 256 printable ASCII characters without spaces")
		}
		if origin.URL != "" && !task.ValidSourceURL(origin.URL) {
			return usage("--url must be an http or https URL of at most 2048 printable ASCII characters without spaces")
		}
		create := protocol.TaskCreateParams{Text: body, Source: origin.Source, Key: origin.Key, URL: origin.URL, ReplyVia: origin.ReplyVia}
		if origin.State == "queued" {
			// Open is the default and stays off the wire, so an open create
			// keeps its shape and works against any gateway.
			create.State = "queued"
		}
		if origin.ReplySlack != "" {
			if create.ReplySlack, err = parseReplySlack(origin.ReplySlack); err != nil {
				return err
			}
		}
		params = create
	case "update":
		body, err := a.readCommandTextSource(text, textFile, textSet, textFileSet, stdin)
		if err != nil {
			return err
		}
		params = protocol.TaskUpdateParams{Task: selector, Text: body}
	case "show":
		params = protocol.TaskSelector{Task: selector}
	case "list":
		if state != "" && all {
			return usage("--state and --all are mutually exclusive")
		}
		var selectedState *string
		if state != "" {
			if !task.State(state).Valid() {
				return usage("invalid --state")
			}
			selectedState = &state
		}
		params = protocol.TaskListParams{State: selectedState, IncludeDone: all, Limit: limit, Cursor: cursor}
	case "open", "working", "done", "queue":
		// The legacy flag parser always supplied a note pointer, including for
		// an omitted --note; retain that precise wire DTO shape.
		target := command
		if command == "queue" {
			target = string(task.Queued)
		}
		params = protocol.TaskTransitionParams{Task: selector, State: target, Note: &note}
	case "reply":
		original, err := a.readCommandTextSource(text, textFile, textSet, textFileSet, stdin)
		if err != nil {
			return err
		}
		fc, err := loadCommandConfig()
		if err != nil {
			return err
		}
		body, err := formatOutgoing(fc, original, send.raw)
		if err != nil {
			return err
		}
		if send.dryRun {
			return a.renderDryRun(original, body, send.raw, jsonOut)
		}
		params = protocol.TaskReplyParams{Task: selector, Text: body, Done: done, Unfurl: unfurl}
	default:
		return fmt.Errorf("unbound typed task command %q", command)
	}

	if !mutation && requestID != "" {
		return usage("--request-id is mutation-only")
	}
	if mutation && requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		requestID = id
	}
	if !mutation {
		requestID = "req_read" + strings.ReplaceAll(command, ".", "_")
	}

	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	if err := checkTaskFrame(c, command, requestID, params, textFileSet, stdin, a.Version); err != nil {
		return err
	}
	capabilities := []string{protocol.CapabilityLocalTasks}
	if command == "reply" {
		// Every reply needs slack_unfurl_v1, not only --unfurl: an older
		// gateway would let Slack unfurl a reply Cue now posts without.
		capabilities = []string{protocol.CapabilityTaskReply, protocol.CapabilitySlackUnfurl}
	}
	if origin.Source != "" {
		capabilities = append(capabilities, protocol.CapabilityTaskSources)
	}
	if origin.ReplySlack != "" {
		capabilities = append(capabilities, protocol.CapabilityTaskReplyTargets)
	}
	if origin.ReplyVia != "" {
		capabilities = append(capabilities, protocol.CapabilityTaskReplyAdapters)
	}
	if command == "queue" || origin.State == "queued" || (command == "list" && state == string(task.Queued)) {
		capabilities = append(capabilities, protocol.CapabilityTaskQueued)
	}
	out, err := a.gatewayCall(c, taskProtocolCommand(command), requestID, params, capabilities...)
	if err != nil {
		return mutationJSONError(err, mutation, jsonOut, requestID)
	}
	if mutation {
		out = map[string]any{"request_id": requestID, "result": out}
	}
	if command == "list" {
		continuation := "cue task list"
		if state != "" {
			continuation += " --state " + shellQuote(state)
		} else if all {
			continuation += " --all"
		}
		if limit == 0 {
			limit = 20
		}
		continuation += fmt.Sprintf(" --limit %d", limit)
		return a.renderPage(out, jsonOut, continuation)
	}
	return a.render(out, jsonOut)
}

// checkTaskFrame preserves the legacy text-file diagnostic for ordinary task
// mutations. Reply historically guarded every text source, so it must preflight
// literal text too before a client handshake can misclassify it as unavailable.
func checkTaskFrame(c config.Config, command, requestID string, params any, textFile, stdin bool, version string) error {
	if command != "reply" && !textFile && !stdin {
		return nil
	}
	request := protocol.Request{ProtocolVersion: protocol.Version, RequestID: requestID, ClientKind: "cli", ClientVersion: version, Command: taskProtocolCommand(command), Params: mustJSON(params)}
	encoded, err := protocol.EncodeRequest(request)
	if err != nil {
		return err
	}
	if len(encoded) <= c.LocalProtocolMaximumFrame {
		return nil
	}
	if command == "reply" || stdin {
		return usage("request exceeds the protocol frame limit")
	}
	return usage("--text-file exceeds the protocol frame limit")
}

func taskProtocolCommand(command string) string {
	switch command {
	case "show":
		return "task.get"
	case "open", "working", "done", "queue":
		return "task.transition"
	default:
		return "task." + command
	}
}
