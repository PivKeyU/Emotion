package handlers

import (
	"testing"

	"github.com/PivKeyU/Emotion/internal/db"
)

func TestDirectStreamURLForRemoteURLUsesEmotionEndpoint(t *testing.T) {
	const upstream = "https://cdn.example.test/movie.mkv?signature=abc123"
	got, addAPIKey := directStreamURLForMedia(videoMediaRow{
		UUID:          "media-1",
		FileContainer: "matroska",
		PathType:      db.PathTypeURL,
		PathURL:       upstream,
	}, "play-session", "token")

	if !addAPIKey {
		t.Fatal("AddApiKeyToDirectStreamUrl = false, want true for Emotion playback endpoint")
	}
	want := "/Videos/media-1/original.mkv?line=&api_key=token"
	if got != want {
		t.Fatalf("DirectStreamUrl = %v, want %q", got, want)
	}
}

func TestDirectStreamURLForLocalSourceUsesEmotionRangeEndpoint(t *testing.T) {
	got, addAPIKey := directStreamURLForMedia(videoMediaRow{
		UUID:          "media-1",
		FileContainer: "matroska",
		PathType:      db.PathTypeLocal,
	}, "play-session", "token")

	if !addAPIKey {
		t.Fatal("AddApiKeyToDirectStreamUrl = false, want true for Emotion playback endpoint")
	}
	want := "/Videos/media-1/original.mkv?line=&api_key=token"
	if got != want {
		t.Fatalf("DirectStreamUrl = %v, want %q", got, want)
	}
}

func TestDirectStreamURLRequiresPlaySession(t *testing.T) {
	got, addAPIKey := directStreamURLForMedia(videoMediaRow{
		UUID:     "media-1",
		PathType: db.PathTypeURL,
		PathURL:  "https://cdn.example.test/movie.mkv",
	}, "", "token")

	if got != nil {
		t.Fatalf("DirectStreamUrl = %v, want nil", got)
	}
	if addAPIKey {
		t.Fatal("AddApiKeyToDirectStreamUrl = true, want false without DirectStreamUrl")
	}
}
