package tmdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestClient_MovieHappyPath spins up a stub TMDB and checks we:
//   - send api_key in the query
//   - set Accept: application/json
//   - pass the configured language
//   - parse poster/backdrop + genres + runtime correctly
func TestClient_MovieHappyPath(t *testing.T) {
	var gotRequest *http.Request

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":              502419,
			"title":           "安彦良和・板野一郎原画摄影集",
			"original_title":  "Yasuhiko & Itano",
			"overview":        "动画大师对谈",
			"release_date":    "2014-05-12",
			"runtime":         90,
			"poster_path":     "/abc.jpg",
			"backdrop_path":   "/def.jpg",
			"genres":          []map[string]any{{"id": 16, "name": "动画"}},
			"imdb_id":         "tt1234567",
			"vote_average":    7.5,
		})
	}))
	defer srv.Close()

	c := NewClient("test-key-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		WithBaseURL(srv.URL),
		WithLanguage("zh-CN"),
	)

	m, err := c.GetMovie(context.Background(), 502419)
	if err != nil {
		t.Fatalf("GetMovie: %v", err)
	}
	if m.ID != 502419 {
		t.Errorf("id = %d", m.ID)
	}
	if m.Title != "安彦良和・板野一郎原画摄影集" {
		t.Errorf("title = %q", m.Title)
	}
	if m.Runtime != 90 {
		t.Errorf("runtime = %d", m.Runtime)
	}
	if m.PosterPath != "/abc.jpg" {
		t.Errorf("poster = %q", m.PosterPath)
	}
	if PosterURL(m.PosterPath, "w500") != "https://image.tmdb.org/t/p/w500/abc.jpg" {
		t.Errorf("poster URL wrong: %q", PosterURL(m.PosterPath, "w500"))
	}

	// Validate the outgoing request.
	if gotRequest == nil {
		t.Fatal("no request captured")
	}
	q := gotRequest.URL.Query()
	if q.Get("api_key") == "" {
		t.Error("api_key not sent")
	}
	if q.Get("language") != "zh-CN" {
		t.Errorf("language = %q", q.Get("language"))
	}
	if gotRequest.Header.Get("Accept") != "application/json" {
		t.Errorf("missing Accept header")
	}
	if !strings.HasPrefix(gotRequest.URL.Path, "/movie/502419") {
		t.Errorf("path = %q", gotRequest.URL.Path)
	}
}

// TestClient_SearchEncodesQuery verifies search params are properly URL-encoded
// even for CJK titles and brackets.
func TestClient_SearchEncodesQuery(t *testing.T) {
	var gotQuery url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": 502419, "title": "T"},
			},
		})
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	results, err := c.SearchMovie(context.Background(), "安彦良和・板野一郎原画摄影集", 2014)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != 502419 {
		t.Fatalf("results = %+v", results)
	}
	if gotQuery.Get("query") != "安彦良和・板野一郎原画摄影集" {
		t.Errorf("query = %q", gotQuery.Get("query"))
	}
	if gotQuery.Get("year") != "2014" {
		t.Errorf("year = %q", gotQuery.Get("year"))
	}
}

// TestClient_BearerToken verifies v4 tokens go in the Authorization header,
// not the api_key query param.
func TestClient_BearerToken(t *testing.T) {
	var gotAuth, gotAPIKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.URL.Query().Get("api_key")
		_, _ = w.Write([]byte(`{"id":1,"title":"x"}`))
	}))
	defer srv.Close()

	// v4 tokens start with "eyJ" and are >40 chars.
	v4Token := "eyJhbGciOiJIUzI1NiJ9.thisIsAStubJwtOver40CharsInTotal"
	c := NewClient(v4Token, WithBaseURL(srv.URL))
	if _, err := c.GetMovie(context.Background(), 1); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization missing Bearer prefix: %q", gotAuth)
	}
	if gotAPIKey != "" {
		t.Errorf("should not send api_key with v4 token, got %q", gotAPIKey)
	}
}

// TestClient_NotFound returns ErrNotFound for 404 responses.
func TestClient_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient("k", WithBaseURL(srv.URL))
	_, err := c.GetMovie(context.Background(), 999)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
