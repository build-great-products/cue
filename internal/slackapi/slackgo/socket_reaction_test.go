package slackgo

import (
	"context"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/build-great-products/cue/internal/slackapi"
)

// TestSocketDecodesReactionsAndAcksIgnoredEvents: reaction events on
// messages become reaction envelopes; a reaction on a file and an event type
// Cue does not use are acknowledged and skipped, so the next event is read on
// the same connection instead of the stream failing (which made the runtime
// reconnect on every reaction once the app subscribed to them).
func TestSocketDecodesReactionsAndAcksIgnoredEvents(t *testing.T) {
	t.Parallel()
	acks := make(chan string, 8)
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		for _, request := range []map[string]any{
			eventsAPIRequest("env-unknown", "Ev-unknown", map[string]any{"type": "member_joined_channel", "user": "U012USR", "channel": "C012ABC"}),
			eventsAPIRequest("env-file", "Ev-file", map[string]any{"type": "reaction_added", "user": "U012USR", "reaction": "eyes", "item": map[string]any{"type": "file", "file": "F012"}, "event_ts": "1700000000.000010"}),
			eventsAPIRequest("env-added", "Ev-added", map[string]any{"type": "reaction_added", "user": "U012USR", "reaction": "+1::skin-tone-3", "item_user": "U0OTHER", "item": map[string]any{"type": "message", "channel": "C012ABC", "ts": "1700000000.000003"}, "event_ts": "1700000000.000011"}),
			eventsAPIRequest("env-removed", "Ev-removed", map[string]any{"type": "reaction_removed", "user": "U012USR", "reaction": "eyes", "item": map[string]any{"type": "message", "channel": "G012PRV", "ts": "1700000000.000004"}, "event_ts": "1700000000.000012"}),
		} {
			_ = connection.WriteJSON(request)
		}
		for {
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if connection.ReadJSON(&ack) != nil {
				return
			}
			acks <- ack.EnvelopeID
		}
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()
	stream, err := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket()).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	added, err := stream.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if added.Kind != slackapi.EventReactionAdded || added.EventID != "Ev-added" || added.ChannelID != "C012ABC" || added.MessageTS != "1700000000.000003" ||
		added.UserID != "U012USR" || added.Reaction != "+1::skin-tone-3" || added.ThreadTS != "" || added.EventTS != "1700000000.000011" || added.Ack == nil {
		t.Fatalf("added = %#v", added)
	}
	removed, err := stream.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if removed.Kind != slackapi.EventReactionRemoved || removed.ChannelID != "G012PRV" || removed.Reaction != "eyes" {
		t.Fatalf("removed = %#v", removed)
	}
	// The two ignored envelopes were acknowledged by Receive itself.
	for _, want := range []string{"env-unknown", "env-file"} {
		select {
		case got := <-acks:
			if got != want {
				t.Fatalf("ack = %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s was not acknowledged", want)
		}
	}
}

func TestReactionEnvelopeValidation(t *testing.T) {
	t.Parallel()
	ok := slackapi.Envelope{EnvelopeID: "env", EventID: "Ev", Kind: slackapi.EventReactionAdded, TeamID: "T012ABC", ChannelID: "C012ABC", UserID: "U012USR", MessageTS: "1700000000.000003", EventTS: "1700000000.000004", Reaction: "tada", Ack: slackapi.NewAcknowledgement(func(context.Context) error { return nil })}
	if err := slackapi.ValidateEnvelope(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*slackapi.Envelope){
		"no reaction":   func(e *slackapi.Envelope) { e.Reaction = "" },
		"bad reaction":  func(e *slackapi.Envelope) { e.Reaction = "a b" },
		"with text":     func(e *slackapi.Envelope) { e.Text = "x" },
		"with thread":   func(e *slackapi.Envelope) { e.ThreadTS = "1700000000.000001" },
		"message+emoji": func(e *slackapi.Envelope) { e.Kind = slackapi.EventMessage },
		"no channel":    func(e *slackapi.Envelope) { e.ChannelID = "" },
		"no message ts": func(e *slackapi.Envelope) { e.MessageTS = "" },
	} {
		e := ok
		mutate(&e)
		if slackapi.ValidateEnvelope(e) == nil {
			t.Fatalf("%s: accepted %#v", name, e)
		}
	}
}
