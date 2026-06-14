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

func TestSortVideoMediasPutsHighestQualityFirstAndKeepsItemID(t *testing.T) {
	medias := []videoMediaRow{
		{
			ID:          1,
			UUID:        "low",
			ItemID:      "vl-10",
			Name:        "Movie.720p.mkv",
			FileSize:    100,
			FileSecond:  100,
			PathType:    db.PathTypeURL,
			PathURL:     "https://example.test/low.mkv",
			FileMetadata: `{"streams":[{"codec_type":"video","width":1280,"height":720}],
				"format":{"bit_rate":"2000000"}}`,
		},
		{
			ID:          2,
			UUID:        "high",
			ItemID:      "vl-10",
			Name:        "Movie.2160p.mkv",
			FileSize:    200,
			FileSecond:  100,
			PathType:    db.PathTypeURL,
			PathURL:     "https://example.test/high.mkv",
			FileMetadata: `{"streams":[{"codec_type":"video","width":3840,"height":2160}],
				"format":{"bit_rate":"12000000"}}`,
		},
	}
	sortVideoMedias(medias)
	if medias[0].UUID != "high" {
		t.Fatalf("first media source = %v, want high", medias[0].UUID)
	}
	if got := mediaItemID(medias[0]); got != "vl-10" {
		t.Fatalf("mediaItemID = %v, want vl-10", got)
	}
}

func TestItemPathIsUniquePerItem(t *testing.T) {
	if got, want := itemPath("vl-5"), "/items/vl-5.strm"; got != want {
		t.Fatalf("itemPath = %q, want %q", got, want)
	}
}
