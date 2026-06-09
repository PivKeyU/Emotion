package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const OMDBBaseURL = "https://www.omdbapi.com/"

// OMDBClient talks to OMDb's HTTP API.
type OMDBClient struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

// OMDBOption configures an OMDBClient.
type OMDBOption func(*OMDBClient)

func WithOMDBHTTPClient(h *http.Client) OMDBOption {
	return func(c *OMDBClient) { c.httpClient = h }
}

func WithOMDBBaseURL(baseURL string) OMDBOption {
	return func(c *OMDBClient) { c.baseURL = baseURL }
}

func NewOMDBClient(apiKey string, opts ...OMDBOption) *OMDBClient {
	c := &OMDBClient{
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    OMDBBaseURL,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *OMDBClient) Enabled() bool {
	return c != nil && strings.TrimSpace(c.apiKey) != ""
}

func (c *OMDBClient) GetByIMDB(ctx context.Context, imdbID string) (*BasicMetadata, error) {
	imdbID = strings.TrimSpace(imdbID)
	if imdbID == "" {
		return nil, ErrNotFound
	}
	q := url.Values{}
	q.Set("i", imdbID)
	return c.get(ctx, q)
}

func (c *OMDBClient) SearchByTitle(ctx context.Context, title string, year int, mediaType string) (*BasicMetadata, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, ErrNotFound
	}
	q := url.Values{}
	q.Set("t", title)
	if year > 0 {
		q.Set("y", strconv.Itoa(year))
	}
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "movie":
		q.Set("type", "movie")
	case "tv", "series":
		q.Set("type", "series")
	}
	return c.get(ctx, q)
}

type omdbResponse struct {
	Response string `json:"Response"`
	Error    string `json:"Error"`

	Title    string `json:"Title"`
	Year     string `json:"Year"`
	Rated    string `json:"Rated"`
	Released string `json:"Released"`
	Runtime  string `json:"Runtime"`
	Genre    string `json:"Genre"`
	Plot     string `json:"Plot"`
	Poster   string `json:"Poster"`
	Type     string `json:"Type"`
	IMDBID   string `json:"imdbID"`
}

func (c *OMDBClient) get(ctx context.Context, q url.Values) (*BasicMetadata, error) {
	if !c.Enabled() {
		return nil, errorsDisabled("omdb")
	}
	q.Set("apikey", c.apiKey)
	q.Set("plot", "full")
	q.Set("r", "json")

	reqURL := c.baseURL
	if encoded := q.Encode(); encoded != "" {
		if strings.Contains(reqURL, "?") {
			reqURL += "&" + encoded
		} else {
			reqURL += "?" + encoded
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("omdb: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("omdb: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out omdbResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if strings.EqualFold(out.Response, "false") {
		if out.Error == "" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("omdb: %s", out.Error)
	}
	if strings.TrimSpace(out.Title) == "" {
		return nil, ErrNotFound
	}
	return omdbToBasic(out), nil
}

func omdbToBasic(in omdbResponse) *BasicMetadata {
	poster := strings.TrimSpace(in.Poster)
	if strings.EqualFold(poster, "N/A") {
		poster = ""
	}
	return &BasicMetadata{
		Source:     "omdb",
		ProviderID: strings.TrimSpace(in.IMDBID),
		MediaType:  strings.TrimSpace(in.Type),
		Title:      strings.TrimSpace(in.Title),
		Overview:   strings.TrimSpace(strings.TrimPrefix(in.Plot, "N/A")),
		AirDate:    parseOMDBDate(firstNonEmpty(in.Released, in.Year)),
		Runtime:    parseOMDBRuntime(in.Runtime),
		PosterURL:  poster,
		IMDBID:     strings.TrimSpace(in.IMDBID),
	}
}

func parseOMDBRuntime(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "N/A") {
		return 0
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(fields[0])
	return n
}

func parseOMDBDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "N/A") {
		return time.Time{}
	}
	return parseYearDate(raw)
}

func errorsDisabled(name string) error {
	return fmt.Errorf("%s: api key not configured", name)
}
