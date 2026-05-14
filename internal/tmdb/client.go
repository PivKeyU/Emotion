// Package tmdb provides a minimal TMDB (The Movie Database) API v3 client
// focused on fetching the fields Emotion needs for metadata backfill:
// title / overview / air date / runtime / poster / backdrop / genres / ids.
//
// Docs: https://developer.themoviedb.org/reference/
//
// We intentionally avoid heavy dependency on generated clients. The set of
// calls is small and the response shapes are stable enough to pin by hand.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BaseURL is TMDB's v3 API root.
const BaseURL = "https://api.themoviedb.org/3"

// ImageBaseURL is the TMDB image CDN. Poster paths coming back from the API
// are relative like "/abc123.jpg" and must be prepended with this + a size.
const ImageBaseURL = "https://image.tmdb.org/t/p"

// Client talks to TMDB.
type Client struct {
	apiKey      string // v3 API key OR bearer token (v4)
	useBearer   bool
	httpClient  *http.Client
	baseURL     string
	language    string
	rateLimiter <-chan time.Time
	rateTicker  *time.Ticker
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithBaseURL overrides the API root (useful for tests).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// WithLanguage overrides the language parameter (default: zh-CN).
func WithLanguage(lang string) Option {
	return func(c *Client) { c.language = lang }
}

// WithRateLimit sets the client-wide request pacing. Values <= 0 disable the
// local limiter and leave throttling to TMDB/server responses.
func WithRateLimit(requestsPerSecond int) Option {
	return func(c *Client) {
		if c.rateTicker != nil {
			c.rateTicker.Stop()
			c.rateTicker = nil
		}
		if requestsPerSecond <= 0 {
			c.rateLimiter = nil
			return
		}
		interval := time.Second / time.Duration(requestsPerSecond)
		if interval <= 0 {
			interval = time.Millisecond
		}
		t := time.NewTicker(interval)
		c.rateTicker = t
		c.rateLimiter = t.C
	}
}

// NewClient constructs a TMDB client.
//
// apiKey may be either:
//   - a legacy v3 API key (32 hex chars) → sent as ?api_key=...
//   - a v4 bearer token (starts with "eyJ") → sent as Authorization header.
//
// We autodetect based on length/shape.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    BaseURL,
		language:   "zh-CN",
	}
	// v4 bearer tokens are JWTs (start with eyJ) and much longer than 32 chars.
	if len(c.apiKey) > 40 && strings.HasPrefix(c.apiKey, "eyJ") {
		c.useBearer = true
	}
	// Default rate limit: keep comfortably below TMDB's documented 50/sec
	// while avoiding slow one-by-one library scrapes.
	t := time.NewTicker(50 * time.Millisecond)
	c.rateTicker = t
	c.rateLimiter = t.C

	for _, o := range opts {
		o(c)
	}
	return c
}

// Enabled reports whether an API key was configured.
func (c *Client) Enabled() bool { return c != nil && c.apiKey != "" }

// Close releases local resources owned by the client.
func (c *Client) Close() {
	if c != nil && c.rateTicker != nil {
		c.rateTicker.Stop()
		c.rateTicker = nil
	}
}

// --- response types (only fields we use) ---

// Movie is TMDB's movie details endpoint shape.
type Movie struct {
	ID               int64   `json:"id"`
	Title            string  `json:"title"`
	OriginalTitle    string  `json:"original_title"`
	OriginalLanguage string  `json:"original_language"`
	Overview         string  `json:"overview"`
	Tagline          string  `json:"tagline"`
	ReleaseDate      string  `json:"release_date"`
	Runtime          int     `json:"runtime"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	Genres           []Genre `json:"genres"`
	IMDBID           string  `json:"imdb_id"`
	VoteAverage      float64 `json:"vote_average"`
}

// TVShow is TMDB's tv/{id} details shape.
type TVShow struct {
	ID               int64    `json:"id"`
	Name             string   `json:"name"`
	OriginalName     string   `json:"original_name"`
	OriginalLanguage string   `json:"original_language"`
	Overview         string   `json:"overview"`
	Tagline          string   `json:"tagline"`
	FirstAirDate     string   `json:"first_air_date"`
	EpisodeRuntime   []int    `json:"episode_run_time"`
	PosterPath       string   `json:"poster_path"`
	BackdropPath     string   `json:"backdrop_path"`
	Genres           []Genre  `json:"genres"`
	Seasons          []Season `json:"seasons"`
	ExternalIDs      struct {
		IMDBID string `json:"imdb_id"`
		TVDBID int64  `json:"tvdb_id"`
	} `json:"external_ids"`
}

// Season is a single season inside a TVShow.Seasons array.
type Season struct {
	ID           int64  `json:"id"`
	SeasonNumber int    `json:"season_number"`
	Name         string `json:"name"`
	Overview     string `json:"overview"`
	AirDate      string `json:"air_date"`
	EpisodeCount int    `json:"episode_count"`
	PosterPath   string `json:"poster_path"`
}

// SeasonDetail is the tv/{id}/season/{n} response shape (with episodes).
type SeasonDetail struct {
	Season
	Episodes []Episode `json:"episodes"`
}

// Episode is a TV episode.
type Episode struct {
	ID            int64  `json:"id"`
	SeasonNumber  int    `json:"season_number"`
	EpisodeNumber int    `json:"episode_number"`
	Name          string `json:"name"`
	Overview      string `json:"overview"`
	AirDate       string `json:"air_date"`
	Runtime       int    `json:"runtime"`
	StillPath     string `json:"still_path"`
}

// Genre is a simple id/name pair.
type Genre struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// SearchResult is a single row from /search/movie or /search/tv.
type SearchResult struct {
	ID            int64   `json:"id"`
	Title         string  `json:"title"` // movies
	Name          string  `json:"name"`  // tv shows
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	Overview      string  `json:"overview"`
	ReleaseDate   string  `json:"release_date"`   // movies
	FirstAirDate  string  `json:"first_air_date"` // tv
	PosterPath    string  `json:"poster_path"`
	BackdropPath  string  `json:"backdrop_path"`
	MediaType     string  `json:"media_type"`
	VoteAverage   float64 `json:"vote_average"`
}

// --- endpoints ---

// GetMovie fetches /movie/{id}.
func (c *Client) GetMovie(ctx context.Context, tmdbID int64) (*Movie, error) {
	var m Movie
	if err := c.do(ctx, fmt.Sprintf("/movie/%d", tmdbID), nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// GetTV fetches /tv/{id}. append_to_response=external_ids lets us grab IMDB/TVDB
// with a single call.
func (c *Client) GetTV(ctx context.Context, tmdbID int64) (*TVShow, error) {
	var t TVShow
	q := url.Values{}
	q.Set("append_to_response", "external_ids")
	if err := c.do(ctx, fmt.Sprintf("/tv/%d", tmdbID), q, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetSeason fetches /tv/{id}/season/{n}.
func (c *Client) GetSeason(ctx context.Context, tmdbID int64, seasonNumber int) (*SeasonDetail, error) {
	var s SeasonDetail
	if err := c.do(ctx, fmt.Sprintf("/tv/%d/season/%d", tmdbID, seasonNumber), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SearchMovie queries /search/movie. year may be 0.
func (c *Client) SearchMovie(ctx context.Context, query string, year int) ([]SearchResult, error) {
	q := url.Values{}
	q.Set("query", query)
	if year > 0 {
		q.Set("year", strconv.Itoa(year))
	}
	var resp struct {
		Results []SearchResult `json:"results"`
	}
	if err := c.do(ctx, "/search/movie", q, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// SearchTV queries /search/tv. year may be 0.
func (c *Client) SearchTV(ctx context.Context, query string, year int) ([]SearchResult, error) {
	q := url.Values{}
	q.Set("query", query)
	if year > 0 {
		q.Set("first_air_date_year", strconv.Itoa(year))
	}
	var resp struct {
		Results []SearchResult `json:"results"`
	}
	if err := c.do(ctx, "/search/tv", q, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// FindByIMDB looks up a movie or TV show by IMDB id.
func (c *Client) FindByIMDB(ctx context.Context, imdbID string) (movies, tvs []SearchResult, err error) {
	q := url.Values{}
	q.Set("external_source", "imdb_id")
	var resp struct {
		MovieResults []SearchResult `json:"movie_results"`
		TVResults    []SearchResult `json:"tv_results"`
	}
	if err := c.do(ctx, "/find/"+imdbID, q, &resp); err != nil {
		return nil, nil, err
	}
	return resp.MovieResults, resp.TVResults, nil
}

// do executes a GET against TMDB, decoding into dst.
func (c *Client) do(ctx context.Context, path string, q url.Values, dst any) error {
	if !c.Enabled() {
		return errors.New("tmdb: no api key configured")
	}
	if c.rateLimiter != nil {
		select {
		case <-c.rateLimiter:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if q == nil {
		q = url.Values{}
	}
	if c.language != "" && q.Get("language") == "" {
		q.Set("language", c.language)
	}
	if !c.useBearer {
		q.Set("api_key", c.apiKey)
	}

	reqURL := c.baseURL + path
	if encoded := q.Encode(); encoded != "" {
		reqURL += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.useBearer {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("tmdb %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("tmdb %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if dst == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// ErrNotFound means TMDB returned 404 for the requested resource.
var ErrNotFound = errors.New("tmdb: not found")

// PosterURL returns a fully-qualified poster URL at the given size (e.g. "w500").
// Returns empty when posterPath is empty.
func PosterURL(posterPath, size string) string {
	if posterPath == "" {
		return ""
	}
	if size == "" {
		size = "w500"
	}
	return ImageBaseURL + "/" + size + posterPath
}
