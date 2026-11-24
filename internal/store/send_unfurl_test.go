package store

import (
	"context"
	"errors"
	"testing"
)

// TestSendEffectRecordsUnfurlAndReplayConflicts: a send records its unfurl
// choice, a replay with the same request ID and the same choice returns the
// original effect, and a replay with the other choice is a conflict, exactly
// like a replay with different text.
func TestSendEffectRecordsUnfurlAndReplayConflicts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	text := "see https://example.com"

	quiet := prepareEffect(t, s, "send-quiet", SlackEffectIntent{Kind: SlackEffectSend, Text: text})
	loud := prepareEffect(t, s, "send-loud", SlackEffectIntent{Kind: SlackEffectSend, Text: text, Unfurl: true})
	if quiet.Unfurl || !loud.Unfurl {
		t.Fatalf("recorded unfurl quiet=%v loud=%v", quiet.Unfurl, loud.Unfurl)
	}
	if quiet.PayloadHash == loud.PayloadHash {
		t.Fatal("unfurl choice does not change the payload hash")
	}
	// The established v1 payload hash is kept for a send that lets Slack
	// unfurl, which is how every send before this change was posted.
	if want := hash("slack.effect.payload.v1", "send", effectTestTarget.TeamID, effectTestTarget.ChannelID, "", "", text, ""); loud.PayloadHash != want {
		t.Fatalf("unfurl send hash=%s want %s", loud.PayloadHash, want)
	}
	var column any
	if err := s.DB().QueryRow("SELECT unfurl FROM slack_effects WHERE effect_id=?", quiet.ID).Scan(&column); err != nil || column != int64(0) {
		t.Fatalf("quiet unfurl column=%#v err=%v", column, err)
	}
	if err := s.DB().QueryRow("SELECT unfurl FROM slack_effects WHERE effect_id=?", loud.ID).Scan(&column); err != nil || column != int64(1) {
		t.Fatalf("loud unfurl column=%#v err=%v", column, err)
	}

	replay, err := s.PrepareSlackEffect(ctx, "send-quiet", effectTestTarget, SlackEffectIntent{Kind: SlackEffectSend, Text: text})
	if err != nil || replay.EffectID != quiet.ID {
		t.Fatalf("same-input replay=%+v err=%v", replay, err)
	}
	if _, err := s.PrepareSlackEffect(ctx, "send-quiet", effectTestTarget, SlackEffectIntent{Kind: SlackEffectSend, Text: text, Unfurl: true}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("replay with unfurl err=%v, want request conflict", err)
	}
	if _, err := s.PrepareSlackEffect(ctx, "send-loud", effectTestTarget, SlackEffectIntent{Kind: SlackEffectSend, Text: text}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("replay without unfurl err=%v, want request conflict", err)
	}
	var count int
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects").Scan(&count); err != nil || count != 2 {
		t.Fatalf("effects=%d err=%v", count, err)
	}
}

func TestUnfurlOnlyAppliesToSends(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, intent := range []SlackEffectIntent{
		{Kind: SlackEffectEdit, TargetMessageTS: "1700000000.000001", Text: "x", Unfurl: true},
		{Kind: SlackEffectReact, TargetMessageTS: "1700000000.000001", Emoji: "eyes", Unfurl: true},
		{Kind: SlackEffectDelete, TargetMessageTS: "1700000000.000001", OperatorReason: "cleanup", Unfurl: true},
	} {
		if _, err := s.PrepareSlackEffect(ctx, "unfurl-"+string(intent.Kind), effectTestTarget, intent); err == nil {
			t.Fatalf("%s accepted unfurl", intent.Kind)
		}
	}
	react := prepareEffect(t, s, "react-plain", SlackEffectIntent{Kind: SlackEffectReact, TargetMessageTS: "1700000000.000001", Emoji: "eyes"})
	var column any
	if err := s.DB().QueryRow("SELECT unfurl FROM slack_effects WHERE effect_id=?", react.ID).Scan(&column); err != nil || column != nil {
		t.Fatalf("react unfurl column=%#v err=%v", column, err)
	}
	if _, err := s.DB().Exec("UPDATE slack_effects SET unfurl=0 WHERE effect_id=?", react.ID); err == nil {
		t.Fatal("schema accepted unfurl on a reaction")
	}
	send := prepareEffect(t, s, "send-plain", SlackEffectIntent{Kind: SlackEffectSend, Text: "x"})
	if _, err := s.DB().Exec("UPDATE slack_effects SET unfurl=2 WHERE effect_id=?", send.ID); err == nil {
		t.Fatal("schema accepted unfurl=2")
	}
}

func TestRetryKeepsUnfurl(t *testing.T) {
	s := testStore(t)
	for _, unfurl := range []bool{false, true} {
		suffix := map[bool]string{false: "quiet", true: "loud"}[unfurl]
		failed := prepareEffect(t, s, "retry-src-"+suffix, SlackEffectIntent{Kind: SlackEffectSend, Text: "x", Unfurl: unfurl})
		submitAndFailEffect(t, s, failed.ID)
		result, err := s.RetrySlackEffect(context.Background(), "retry-"+suffix, failed.ID, "again", false)
		if err != nil {
			t.Fatal(err)
		}
		successor, err := s.GetSlackEffect(context.Background(), result.EffectID)
		if err != nil || successor.Unfurl != unfurl || successor.PayloadHash != failed.PayloadHash {
			t.Fatalf("successor=%+v err=%v; want unfurl=%v", successor, err, unfurl)
		}
	}
}

// TestOpenUpgradeV13RecordsExistingSendsAsUnfurled: sends recorded before
// schema 14 were posted without unfurl parameters, so they record unfurl=1,
// and other effects keep NULL.
func TestOpenUpgradeV13RecordsExistingSendsAsUnfurled(t *testing.T) {
	path := ledgerAtVersion(t, 13,
		`INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,text,payload_hash,state,attempt_count,prepared_at_us,submitted_at_us,completed_at_us,response_message_ts,created_at_us,updated_at_us) VALUES('abcdefghijklmnopqrstuv','v13-send','send','T1','C1','old post','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','confirmed',1,1,1,1,'1700000000.000001',1,1)`,
		`INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,target_message_ts,emoji,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES('bcdefghijklmnopqrstuvw','v13-react','react','T1','C1','1700000000.000001','eyes','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','prepared',0,1,1,1)`,
	)
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open v13: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion || SchemaVersion < 14 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	send, err := s.GetSlackEffect(context.Background(), "abcdefghijklmnopqrstuv")
	if err != nil || !send.Unfurl || send.Text != "old post" {
		t.Fatalf("send after upgrade=%+v err=%v", send, err)
	}
	var column any
	if err := s.DB().QueryRow("SELECT unfurl FROM slack_effects WHERE effect_id='bcdefghijklmnopqrstuvw'").Scan(&column); err != nil || column != nil {
		t.Fatalf("react unfurl column=%#v err=%v", column, err)
	}
}
