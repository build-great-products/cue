// Package slackgo implements Cue's SDK-free Slack contracts using the
// pinned slack-go release and an owned Gorilla Socket Mode transport.
package slackgo

import "github.com/build-great-products/cue/internal/slackapi"

const sdkVersion = "v0.27.0"

// Version reports the adapter and pinned SDK version for status/doctor output.
func Version() string { return "slack-go/" + sdkVersion }

var (
	_ slackapi.Web          = (*Web)(nil)
	_ slackapi.FileTransfer = (*Web)(nil)
	_ slackapi.Socket       = (*Socket)(nil)
)
