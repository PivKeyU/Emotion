package metadata

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned when a provider has no matching metadata.
var ErrNotFound = errors.New("metadata: not found")

// BasicMetadata is the shared minimal shape used by fallback providers.
type BasicMetadata struct {
	Source        string
	ProviderID    string
	MediaType     string
	Title         string
	OriginalTitle string
	Overview      string
	Tagline       string
	AirDate       time.Time
	Runtime       int
	PosterURL     string
	BackdropURL   string
	IMDBID        string
	TVDBID        string
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseYearDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02",
		"2006-01-02 15:04:05",
		"2006/01/02",
		"02 Jan 2006",
		"2 Jan 2006",
		"Jan 02, 2006",
		"January 02, 2006",
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	if len(raw) >= 4 {
		if y, err := strconv.Atoi(raw[:4]); err == nil && y >= 1800 && y <= 2200 {
			return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
		}
	}
	return time.Time{}
}
