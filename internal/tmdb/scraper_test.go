package tmdb

import "testing"

func TestTMDBTitleCandidates_CleansAnimeReleaseNames(t *testing.T) {
	got := tmdbTitleCandidates("[ANi] 葬送的芙莉莲 Season 2 [1080P][简繁]", "")
	want := []string{
		"[ANi] 葬送的芙莉莲 Season 2 [1080P][简繁]",
		"葬送的芙莉莲 Season 2",
		"葬送的芙莉莲",
	}
	for _, v := range want {
		if !containsString(got, v) {
			t.Fatalf("missing candidate %q in %#v", v, got)
		}
	}
}

func TestTMDBTitleCandidates_UsesOriginTitle(t *testing.T) {
	got := tmdbTitleCandidates("迷宫饭 第1季", "Dungeon Meshi")
	for _, v := range []string{"迷宫饭 第1季", "迷宫饭", "Dungeon Meshi"} {
		if !containsString(got, v) {
			t.Fatalf("missing candidate %q in %#v", v, got)
		}
	}
}

func TestBestTMDBSearchResult_PrefersTitleAndYear(t *testing.T) {
	got := bestTMDBSearchResult([]SearchResult{
		{ID: 1, Name: "Different Show", FirstAirDate: "2024-01-01", VoteAverage: 9.8},
		{ID: 2, Name: "Dungeon Meshi", FirstAirDate: "2024-01-04", VoteAverage: 8.0},
	}, "Dungeon Meshi", 2024)
	if got.ID != 2 {
		t.Fatalf("id = %d, want 2", got.ID)
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
