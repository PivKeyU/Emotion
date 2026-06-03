package tmdb

import "testing"

func TestLooksLikeEpisodePlaceholderTitle(t *testing.T) {
	placeholders := []string{
		"E01",
		"EP01",
		"Episode 1",
		"Some Show E01",
		"Some Show EP01",
		"Some Show 第1集",
	}
	for _, title := range placeholders {
		if !looksLikeEpisodePlaceholderTitle(title, 1) {
			t.Fatalf("%q should be treated as an episode placeholder", title)
		}
	}

	if looksLikeEpisodePlaceholderTitle("Winter Is Coming", 1) {
		t.Fatal("real episode title was treated as a placeholder")
	}
}

func TestLooksLikeSeasonPlaceholderTitle(t *testing.T) {
	placeholders := []string{"第 1 季", "第1季", "Season 1", "S1"}
	for _, title := range placeholders {
		if !looksLikeSeasonPlaceholderTitle(title, 1) {
			t.Fatalf("%q should be treated as a season placeholder", title)
		}
	}

	if looksLikeSeasonPlaceholderTitle("Book One: Water", 1) {
		t.Fatal("real season title was treated as a placeholder")
	}
}
