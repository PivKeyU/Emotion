// Package importer parses NFO and STRM files and imports them into Next-Emby.
//
// The NFO format follows Emby/Jellyfin/Kodi conventions. We are intentionally
// lenient: unknown tags are ignored, and fields may appear in any order.
package importer

import (
	"encoding/xml"
	"os"
	"strconv"
	"strings"
	"time"
)

// NFOKind is what the NFO describes.
type NFOKind string

// Supported NFO root element names.
const (
	NFOKindMovie   NFOKind = "movie"
	NFOKindTVShow  NFOKind = "tvshow"
	NFOKindSeason  NFOKind = "season"
	NFOKindEpisode NFOKind = "episodedetails"
	NFOKindUnknown NFOKind = ""
)

// Thumb mirrors Kodi's <thumb aspect="poster|landscape|..." preview="...">URL</thumb>.
type Thumb struct {
	Aspect string `xml:"aspect,attr"`
	URL    string `xml:",chardata"`
}

// Fanart mirrors Kodi's <fanart><thumb>...</thumb></fanart>.
type Fanart struct {
	Thumbs []Thumb `xml:"thumb"`
}

// Actor mirrors <actor><name>...</name><role>...</role></actor>.
type Actor struct {
	Name  string `xml:"name"`
	Role  string `xml:"role"`
	Order int    `xml:"order"`
	Thumb string `xml:"thumb"`
}

// UniqueID mirrors <uniqueid type="tmdb">123</uniqueid>.
type UniqueID struct {
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr"`
	Value   string `xml:",chardata"`
}

// NFO is the union of every NFO variant we understand.
// Fields not present in a given document remain empty.
type NFO struct {
	XMLName xml.Name

	// Identity
	Title         string `xml:"title"`
	OriginalTitle string `xml:"originaltitle"`
	SortTitle     string `xml:"sorttitle"`
	ShowTitle     string `xml:"showtitle"` // episode NFOs
	Plot          string `xml:"plot"`
	Outline       string `xml:"outline"`
	Tagline       string `xml:"tagline"`
	Year          int    `xml:"year"`
	Runtime       int    `xml:"runtime"` // minutes
	Premiered     string `xml:"premiered"`
	Aired         string `xml:"aired"`
	ReleaseDate   string `xml:"releasedate"`

	// Numbering (season/episode)
	Season  int `xml:"season"`
	Episode int `xml:"episode"`

	// Media classification
	Genres  []string `xml:"genre"`
	Tags    []string `xml:"tag"`
	Studios []string `xml:"studio"`
	Country []string `xml:"country"`
	MPAA    string   `xml:"mpaa"`

	// People
	Directors []string `xml:"director"`
	Writers   []string `xml:"credits"`
	Actors    []Actor  `xml:"actor"`

	// Identifiers
	ID        string     `xml:"id"`
	TMDBID    string     `xml:"tmdbid"`
	IMDBID    string     `xml:"imdbid"`
	TVDBID    string     `xml:"tvdbid"`
	UniqueIDs []UniqueID `xml:"uniqueid"`

	// Artwork
	Thumbs []Thumb `xml:"thumb"`
	Fanart Fanart  `xml:"fanart"`
	Art    struct {
		Poster string `xml:"poster"`
		Fanart string `xml:"fanart"`
	} `xml:"art"`
}

// Kind returns the NFO variety based on the XML root name.
func (n *NFO) Kind() NFOKind {
	switch strings.ToLower(n.XMLName.Local) {
	case "movie":
		return NFOKindMovie
	case "tvshow":
		return NFOKindTVShow
	case "season":
		return NFOKindSeason
	case "episodedetails":
		return NFOKindEpisode
	}
	return NFOKindUnknown
}

// Tmdb returns the TMDB id, preferring <uniqueid type="tmdb"> over <tmdbid>.
func (n *NFO) Tmdb() string {
	for _, u := range n.UniqueIDs {
		if strings.EqualFold(u.Type, "tmdb") && u.Value != "" {
			return u.Value
		}
	}
	if n.TMDBID != "" {
		return n.TMDBID
	}
	return ""
}

// Imdb returns the IMDB id similarly.
func (n *NFO) Imdb() string {
	for _, u := range n.UniqueIDs {
		if strings.EqualFold(u.Type, "imdb") && u.Value != "" {
			return u.Value
		}
	}
	return n.IMDBID
}

// Tvdb returns the TVDB id similarly.
func (n *NFO) Tvdb() string {
	for _, u := range n.UniqueIDs {
		if strings.EqualFold(u.Type, "tvdb") && u.Value != "" {
			return u.Value
		}
	}
	return n.TVDBID
}

// AirDate returns a parsed premiere/aired/release date, or zero.
// Tries multiple tags in order: premiered > aired > releasedate > year.
func (n *NFO) AirDate() time.Time {
	for _, s := range []string{n.Premiered, n.Aired, n.ReleaseDate} {
		if t, ok := parseDate(s); ok {
			return t
		}
	}
	if n.Year > 0 {
		return time.Date(n.Year, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Time{}
}

// Description returns the best description: plot > outline > tagline.
func (n *NFO) Description() string {
	for _, s := range []string{n.Plot, n.Outline, n.Tagline} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// PosterURL returns a primary image URL. Preference: <art><poster>, then
// <thumb aspect="poster">, then any <thumb>.
func (n *NFO) PosterURL() string {
	if n.Art.Poster != "" {
		return n.Art.Poster
	}
	for _, t := range n.Thumbs {
		if strings.EqualFold(t.Aspect, "poster") && t.URL != "" {
			return strings.TrimSpace(t.URL)
		}
	}
	if len(n.Thumbs) > 0 {
		return strings.TrimSpace(n.Thumbs[0].URL)
	}
	return ""
}

// BackdropURL returns a backdrop/fanart URL. Preference: <art><fanart>,
// <fanart><thumb>, or any <thumb aspect="landscape">.
func (n *NFO) BackdropURL() string {
	if n.Art.Fanart != "" {
		return n.Art.Fanart
	}
	for _, t := range n.Fanart.Thumbs {
		if t.URL != "" {
			return strings.TrimSpace(t.URL)
		}
	}
	for _, t := range n.Thumbs {
		if strings.EqualFold(t.Aspect, "landscape") && t.URL != "" {
			return strings.TrimSpace(t.URL)
		}
	}
	return ""
}

// ParseNFO reads an NFO file.
func ParseNFO(path string) (*NFO, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseNFOBytes(data)
}

// ParseNFOBytes parses raw XML bytes.
func ParseNFOBytes(data []byte) (*NFO, error) {
	data = stripBOM(data)
	var n NFO
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	dec.CharsetReader = lenientCharsetReader
	if err := dec.Decode(&n); err != nil {
		return nil, err
	}
	return &n, nil
}

// parseDate accepts YYYY-MM-DD and a few common variants.
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"2006-01-02",
		"2006-01-02 15:04:05",
		"2006/01/02",
		"01/02/2006",
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	// Year only as last resort.
	if n, err := strconv.Atoi(s); err == nil && n > 1800 && n < 2200 {
		return time.Date(n, 1, 1, 0, 0, 0, 0, time.UTC), true
	}
	return time.Time{}, false
}

// stripBOM drops a UTF-8 BOM if present.
func stripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}
