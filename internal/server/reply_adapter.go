package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/build-great-products/cue/internal/extcmd"
	"github.com/build-great-products/cue/internal/protocol"
	"github.com/build-great-products/cue/internal/store"
	"github.com/build-great-products/cue/internal/task"
)

// Reply adapter exit codes (docs/SPEC.md section 6a). Any other ending is
// ambiguous.
const (
	ReplyAdapterExitConfirmed = 0
	ReplyAdapterExitFailed    = 3
)

// errReplyAdapterNotConfigured refuses a reply adapter name that the active
// configuration does not define.
var errReplyAdapterNotConfigured = errors.New("reply adapter is not configured")

// replyAdapterError is a reply adapter run that did not confirm.
type replyAdapterError struct {
	effect store.ReplyAdapterEffect
}

func (e *replyAdapterError) Error() string {
	if e.effect.State == store.SlackEffectAmbiguous {
		return fmt.Sprintf("reply adapter %s outcome is unknown (%s); it is not retried", e.effect.Adapter, e.effect.DiagnosticCode)
	}
	if e.effect.State == store.SlackEffectSubmitted {
		return fmt.Sprintf("reply adapter %s is still running for this reply", e.effect.Adapter)
	}
	return fmt.Sprintf("reply adapter %s failed (%s)", e.effect.Adapter, e.effect.DiagnosticCode)
}

// replyAdapterInput is the JSON document a reply adapter reads on stdin.
type replyAdapterInput struct {
	Version   int                   `json:"version"`
	EffectID  string                `json:"effect_id"`
	RequestID string                `json:"request_id"`
	Adapter   string                `json:"adapter"`
	Task      replyAdapterInputTask `json:"task"`
	Text      string                `json:"text"`
}

type replyAdapterInputTask struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Key    string `json:"key,omitempty"`
	URL    string `json:"url,omitempty"`
}

// replyViaAdapter durably runs a task's reply adapter once for p.Text and,
// with p.Done, completes the task only after the adapter confirmed.
func (s *Server) replyViaAdapter(ctx context.Context, requestID string, p protocol.TaskReplyParams, t store.Task, adapter string) (protocol.TaskReplyDTO, error) {
	configured, ok := s.snapshot().ReplyAdapters[adapter]
	if !ok {
		return protocol.TaskReplyDTO{}, errReplyAdapterNotConfigured
	}
	effect, err := s.store.PrepareReplyAdapterEffect(ctx, requestID, t.ID, adapter, p.Text)
	if err != nil {
		return protocol.TaskReplyDTO{}, err
	}
	if effect.State == store.SlackEffectPrepared {
		effect, err = s.runReplyAdapter(ctx, effect, configured.Command, configured.TimeoutDuration(), requestID, t, p.Text)
		if err != nil {
			return protocol.TaskReplyDTO{}, err
		}
	}
	if effect.State != store.SlackEffectConfirmed {
		return protocol.TaskReplyDTO{}, &replyAdapterError{effect: effect}
	}
	dto := replyAdapterEffectDTO(effect)
	out := protocol.TaskReplyDTO{AdapterEffect: &dto}
	if !p.Done {
		return out, nil
	}
	transition, err := s.store.Transition(ctx, taskReplyDoneRequestID(requestID), t.ID, task.Done, nil)
	if err != nil {
		return protocol.TaskReplyDTO{}, err
	}
	s.projectTransition(ctx, transition)
	m := mutationDTO(transition)
	out.Task = &m
	return out, nil
}

// runReplyAdapter claims a prepared effect (committed as submitted before
// anything runs), runs the adapter once, and records how it ended. The run
// is detached from the client's cancellation: once submitted, its outcome is
// always recorded.
func (s *Server) runReplyAdapter(ctx context.Context, effect store.ReplyAdapterEffect, command string, timeout time.Duration, requestID string, t store.Task, text string) (store.ReplyAdapterEffect, error) {
	claimed, err := s.store.ClaimReplyAdapterEffect(ctx, effect.ID)
	if errors.Is(err, store.ErrSlackEffectTransition) {
		// Another reply with this request ID claimed it first.
		return s.store.GetReplyAdapterEffect(ctx, effect.ID)
	}
	if err != nil {
		return store.ReplyAdapterEffect{}, err
	}
	input := replyAdapterInput{Version: 1, EffectID: claimed.ID, RequestID: requestID, Adapter: claimed.Adapter, Text: text,
		Task: replyAdapterInputTask{ID: t.ID}}
	if t.Source != nil {
		input.Task.Source, input.Task.Key, input.Task.URL = t.Source.Name, t.Source.Key, t.Source.URL
	}
	body, err := json.Marshal(input)
	if err != nil {
		return store.ReplyAdapterEffect{}, err
	}
	detached := context.WithoutCancel(ctx)
	result := extcmd.Run(detached, command, body, timeout)
	outcome := replyAdapterOutcome(result)
	done, err := s.store.CommitReplyAdapterOutcome(detached, claimed.ID, outcome)
	if err != nil {
		return store.ReplyAdapterEffect{}, err
	}
	if done.State != store.SlackEffectConfirmed && s.Log != nil {
		exit := -1
		if done.ExitCode != nil {
			exit = *done.ExitCode
		}
		s.Log.Warn("reply adapter did not confirm", "adapter", done.Adapter, "effect_id", done.ID, "task_id", done.TaskID,
			"state", done.State, "diagnostic", done.DiagnosticCode, "exit_code", exit, "stderr", result.Stderr)
	}
	return done, nil
}

func replyAdapterOutcome(r extcmd.Result) store.ReplyAdapterOutcome {
	switch {
	case !r.Started:
		// It never ran, so it delivered nothing.
		return store.ReplyAdapterOutcome{State: store.SlackEffectFailed, DiagnosticCode: store.ReplyAdapterDiagnosticNotStarted}
	case r.TimedOut:
		return store.ReplyAdapterOutcome{State: store.SlackEffectAmbiguous, DiagnosticCode: store.ReplyAdapterDiagnosticTimeout}
	case r.Signaled:
		return store.ReplyAdapterOutcome{State: store.SlackEffectAmbiguous, DiagnosticCode: store.ReplyAdapterDiagnosticSignal}
	}
	code := r.ExitCode
	switch code {
	case ReplyAdapterExitConfirmed:
		return store.ReplyAdapterOutcome{State: store.SlackEffectConfirmed, ExitCode: &code}
	case ReplyAdapterExitFailed:
		return store.ReplyAdapterOutcome{State: store.SlackEffectFailed, ExitCode: &code, DiagnosticCode: store.ReplyAdapterDiagnosticFailed}
	default:
		return store.ReplyAdapterOutcome{State: store.SlackEffectAmbiguous, ExitCode: &code, DiagnosticCode: store.ReplyAdapterDiagnosticExit}
	}
}

func replyAdapterEffectDTO(e store.ReplyAdapterEffect) protocol.ReplyAdapterEffectDTO {
	out := protocol.ReplyAdapterEffectDTO{EffectID: e.ID, RelatedTaskID: e.TaskID, Adapter: e.Adapter, State: string(e.State),
		AttemptCount: e.AttemptCount, ExitCode: e.ExitCode, DiagnosticCode: e.DiagnosticCode, PreparedAt: stamp(e.PreparedAt)}
	if e.SubmittedAt != nil {
		at := stamp(*e.SubmittedAt)
		out.SubmittedAt = &at
	}
	if e.CompletedAt != nil {
		at := stamp(*e.CompletedAt)
		out.CompletedAt = &at
	}
	return out
}
