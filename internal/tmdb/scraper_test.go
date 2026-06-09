package tmdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PivKeyU/Emotion/internal/db"
)

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

func TestResolveTMDBID_UsesIMDBBeforeTitleSearch(t *testing.T) {
	searchCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/find/tt1234567" {
			if got := r.URL.Query().Get("external_source"); got != "imdb_id" {
				t.Fatalf("external_source = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"movie_results": []map[string]any{{"id": 42, "title": "External Match"}},
			})
			return
		}
		if r.URL.Path == "/search/movie" {
			searchCalled = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer srv.Close()

	client := NewClient("test-key", WithBaseURL(srv.URL), WithRateLimit(0))
	scraper := NewScraper(client, nil, nil)
	got, err := scraper.resolveTMDBID(context.Background(), db.VideoTypeMovie, "", "tt1234567", "", "Wrong Title", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 42 || got.Source != "imdb" {
		t.Fatalf("resolution = %+v", got)
	}
	if searchCalled {
		t.Fatal("title search should not run after imdb external id matched")
	}
}

func TestResolveTMDBID_UsesTVDBBeforeTitleSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/find/121361" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("external_source"); got != "tvdb_id" {
			t.Fatalf("external_source = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tv_results": []map[string]any{{"id": 99, "name": "TVDB Match"}},
		})
	}))
	defer srv.Close()

	client := NewClient("test-key", WithBaseURL(srv.URL), WithRateLimit(0))
	scraper := NewScraper(client, nil, nil)
	got, err := scraper.resolveTMDBID(context.Background(), db.VideoTypeTV, "", "", "121361", "Wrong Title", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 99 || got.Source != "tvdb" {
		t.Fatalf("resolution = %+v", got)
	}
}

func TestResolveTMDBIDByTitleExcluding_SkipsBadTMDBID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/tv" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("query"); got != "使徒行者2" {
			t.Fatalf("query = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": 197977, "name": "使徒行者2", "first_air_date": "2017-09-18", "vote_average": 9.9},
				{"id": 12345, "name": "使徒行者2", "first_air_date": "2017-09-18", "vote_average": 7.5},
			},
		})
	}))
	defer srv.Close()

	client := NewClient("test-key", WithBaseURL(srv.URL), WithRateLimit(0))
	scraper := NewScraper(client, nil, nil)
	got, err := scraper.resolveTMDBIDByTitleExcluding(context.Background(), db.VideoTypeTV, 197977, "使徒行者2", "", 2017)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 12345 || got.Source != "tmdb_search_retry" {
		t.Fatalf("resolution = %+v", got)
	}
}

func TestFetchAndApplyTMDB_NotFoundIsRecoverable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tv/197977" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient("test-key", WithBaseURL(srv.URL), WithRateLimit(0))
	scraper := NewScraper(client, nil, nil)
	err := scraper.fetchAndApplyTMDB(context.Background(), 1, 197977, db.VideoTypeTV, &ScrapeResult{},
		sql.NullString{}, sql.NullString{}, sql.NullString{}, "使徒行者2",
		sql.NullString{}, sql.NullString{}, sql.NullTime{}, sql.NullInt64{}, sql.NullString{}, false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected recoverable ErrNotFound, got %v", err)
	}
}

func TestApplyTitleProviderHints_ReadsCurlyTMDBTag(t *testing.T) {
	var tmdbID, imdbID, tvdbID sql.NullString
	applyTitleProviderHints(&tmdbID, &imdbID, &tvdbID, "移动的枪口 (2014) {tmdb-121504}")
	if !tmdbID.Valid || tmdbID.String != "121504" {
		t.Fatalf("tmdbID = %+v", tmdbID)
	}
	if imdbID.Valid || tvdbID.Valid {
		t.Fatalf("unexpected imdb/tvdb: %+v %+v", imdbID, tvdbID)
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
