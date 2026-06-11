package handlers

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsPlaybackStopEventDetectsStoppedRouteWithoutEventName(t *testing.T) {
	req := httptest.NewRequest("POST", "/emby/Sessions/Playing/Stopped", nil)

	if !isPlaybackStopEvent(req, "") {
		t.Fatalf("stopped route was not detected as playback stop")
	}
}

func TestIsPlaybackStopEventDetectsEventName(t *testing.T) {
	req := httptest.NewRequest("POST", "/emby/Sessions/Playing/Progress", nil)

	if !isPlaybackStopEvent(req, "PlaybackStopped") {
		t.Fatalf("PlaybackStopped event was not detected as playback stop")
	}
}

func TestActivityDurationsUsesStoppedAtNotSweepTime(t *testing.T) {
	startedAt := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	lastProgressAt := startedAt.Add(time.Minute)
	sess := &liveSession{
		StartedAt:      startedAt,
		LastProgressAt: lastProgressAt,
	}

	playDur, pauseDur := activityDurations(sess, sess.LastProgressAt)

	if playDur != 60 {
		t.Fatalf("playDur = %d, want 60", playDur)
	}
	if pauseDur != 0 {
		t.Fatalf("pauseDur = %d, want 0", pauseDur)
	}
}

func TestActivityDurationsCapsPauseAtPlayDuration(t *testing.T) {
	startedAt := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	sess := &liveSession{
		StartedAt:   startedAt,
		PausedAccum: 2 * time.Minute,
	}

	playDur, pauseDur := activityDurations(sess, startedAt.Add(time.Minute))

	if playDur != 60 {
		t.Fatalf("playDur = %d, want 60", playDur)
	}
	if pauseDur != 60 {
		t.Fatalf("pauseDur = %d, want 60", pauseDur)
	}
}
