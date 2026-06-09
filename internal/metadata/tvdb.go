package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const TVDBBaseURL = "https://api4.thetvdb.com/v4"

// TVDBClient talks to TheTVDB API v4.
type TVDBClient struct {
	apiKey     string
	pin        string
	httpClient *http.Client
	baseURL    string

	mu           sync.Mutex
	token        string
	tokenExpires time.Time
}

type TVDBOption func(*TVDBClient)

func WithTVDBHTTPClient(h *http.Client) TVDBOption {
	return func(c *TVDBClient) { c.httpClient = h }
}

func WithTVDBBaseURL(baseURL string) TVDBOption {
	return func(c *TVDBClient) { c.baseURL = strings.TrimRight(baseURL, "/") }
}

func NewTVDBClient(apiKey, pin string, opts ...TVDBOption) *TVDBClient {
	c := &TVDBClient{
		apiKey:     strings.TrimSpace(apiKey),
		pin:        strings.TrimSpace(pin),
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    TVDBBaseURL,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *TVDBClient) Enabled() bool {
	return c != nil && strings.TrimSpace(c.apiKey) != ""
}

func (c *TVDBClient) GetSeriesByID(ctx context.Context, tvdbID string) (*BasicMetadata, error) {
	tvdbID = strings.TrimSpace(tvdbID)
	if tvdbID == "" {
		return nil, ErrNotFound
	}
	items, err := c.getBasicList(ctx, "/series/"+url.PathEscape(tvdbID), nil)
	if err != nil {
		return nil, err
	}
	return firstBasic(items, "series")
}

func (c *TVDBClient) FindByIMDB(ctx context.Context, imdbID string) ([]BasicMetadata, error) {
	imdbID = strings.TrimSpace(imdbID)
	if imdbID == "" {
		return nil, ErrNotFound
	}
	return c.getBasicList(ctx, "/search/remoteid/"+url.PathEscape(imdbID), nil)
}

func (c *TVDBClient) SearchSeriesByTitle(ctx context.Context, title string, year int) ([]BasicMetadata, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, ErrNotFound
	}
	q := url.Values{}
	q.Set("query", title)
	q.Set("type", "series")
	q.Set("limit", "5")
	if year > 0 {
		q.Set("year", strconv.Itoa(year))
	}
	return c.getBasicList(ctx, "/search", q)
}

func firstBasic(items []BasicMetadata, preferredType string) (*BasicMetadata, error) {
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	preferredType = strings.ToLower(strings.TrimSpace(preferredType))
	if preferredType != "" {
		for i := range items {
			if strings.EqualFold(items[i].MediaType, preferredType) {
				return &items[i], nil
			}
		}
	}
	return &items[0], nil
}

func (c *TVDBClient) getBasicList(ctx context.Context, path string, q url.Values) ([]BasicMetadata, error) {
	if !c.Enabled() {
		return nil, errorsDisabled("tvdb")
	}
	if err := c.ensureToken(ctx); err != nil {
		return nil, err
	}
	reqURL := c.baseURL + path
	if encoded := q.Encode(); encoded != "" {
		reqURL += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.currentToken())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tvdb %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("tvdb %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	items := parseTVDBBasics(envelope.Data)
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return items, nil
}

func (c *TVDBClient) ensureToken(ctx context.Context) error {
	c.mu.Lock()
	if c.token != "" && time.Now().Before(c.tokenExpires) {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	body := map[string]string{"apikey": c.apiKey}
	if c.pin != "" {
		body["pin"] = c.pin
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/login", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("tvdb login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("tvdb login: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if strings.TrimSpace(out.Data.Token) == "" {
		return fmt.Errorf("tvdb login: empty token")
	}
	c.mu.Lock()
	c.token = out.Data.Token
	c.tokenExpires = time.Now().Add(23 * time.Hour)
	c.mu.Unlock()
	return nil
}

func (c *TVDBClient) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func parseTVDBBasics(raw json.RawMessage) []BasicMetadata {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '[' {
		var arr []map[string]json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			return nil
		}
		out := make([]BasicMetadata, 0, len(arr))
		for _, item := range arr {
			out = append(out, tvdbBasicsFromMap(item)...)
		}
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return tvdbBasicsFromMap(obj)
}

func tvdbBasicsFromMap(item map[string]json.RawMessage) []BasicMetadata {
	var out []BasicMetadata
	for _, key := range []string{"series", "movie", "episode"} {
		if raw, ok := item[key]; ok && len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) == nil {
				out = append(out, tvdbBasicFromFlatMap(nested, key))
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	basic := tvdbBasicFromFlatMap(item, "")
	if basic.Title == "" && basic.TVDBID == "" && basic.IMDBID == "" {
		return nil
	}
	return []BasicMetadata{basic}
}

func tvdbBasicFromFlatMap(item map[string]json.RawMessage, fallbackType string) BasicMetadata {
	mediaType := stringField(item, "type")
	if mediaType == "" {
		mediaType = stringField(item, "primary_type")
	}
	if mediaType == "" {
		mediaType = fallbackType
	}
	tvdbID := firstNonEmpty(
		stringField(item, "tvdb_id"),
		stringField(item, "id"),
		idFromObjectID(stringField(item, "objectID")),
		idFromObjectID(stringField(item, "object_id")),
	)
	title := firstNonEmpty(
		stringField(item, "name"),
		stringField(item, "title"),
		stringField(item, "translatedName"),
	)
	overview := firstNonEmpty(
		stringField(item, "overview"),
		stringField(item, "overviewTranslations"),
		stringFromStringMap(item, "overviews"),
	)
	poster := firstNonEmpty(
		stringField(item, "image_url"),
		stringField(item, "image"),
	)
	air := parseYearDate(firstNonEmpty(
		stringField(item, "first_air_time"),
		stringField(item, "firstAired"),
		stringField(item, "aired"),
		stringField(item, "year"),
	))
	imdbID := firstNonEmpty(
		stringField(item, "imdb_id"),
		remoteID(item, "imdb"),
	)
	return BasicMetadata{
		Source:     "tvdb",
		ProviderID: tvdbID,
		MediaType:  strings.TrimSpace(mediaType),
		Title:      title,
		Overview:   overview,
		AirDate:    air,
		PosterURL:  poster,
		IMDBID:     imdbID,
		TVDBID:     tvdbID,
	}
}

func stringField(item map[string]json.RawMessage, key string) string {
	raw, ok := item[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}

func stringFromStringMap(item map[string]json.RawMessage, key string) string {
	raw, ok := item[key]
	if !ok {
		return ""
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, lang := range []string{"zho", "zh", "zh-CN", "eng", "en"} {
		if strings.TrimSpace(m[lang]) != "" {
			return strings.TrimSpace(m[lang])
		}
	}
	for _, value := range m {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func remoteID(item map[string]json.RawMessage, source string) string {
	raw, ok := item["remoteIds"]
	if !ok {
		raw = item["remote_ids"]
	}
	if len(raw) == 0 {
		return ""
	}
	var ids []map[string]json.RawMessage
	if json.Unmarshal(raw, &ids) != nil {
		return ""
	}
	source = strings.ToLower(source)
	for _, id := range ids {
		name := strings.ToLower(firstNonEmpty(
			stringField(id, "sourceName"),
			stringField(id, "source_name"),
			stringField(id, "type"),
		))
		if !strings.Contains(name, source) {
			continue
		}
		return firstNonEmpty(stringField(id, "id"), stringField(id, "value"))
	}
	return ""
}

func idFromObjectID(objectID string) string {
	objectID = strings.TrimSpace(objectID)
	if objectID == "" {
		return ""
	}
	if idx := strings.LastIndex(objectID, "-"); idx >= 0 && idx+1 < len(objectID) {
		return objectID[idx+1:]
	}
	return ""
}
