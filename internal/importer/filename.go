package importer

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Regexes compiled once at package load.
var (
	// Provider id tag like [tmdb=502419] or [tmdbid-502419] or [imdb=tt1234567].
	// Case-insensitive, accepts = or -.
	reProviderTag = regexp.MustCompile(`(?i)\[(tmdb|tmdbid|imdb|imdbid|tvdb|tvdbid)\s*[=\-:]\s*([a-z0-9]+)\]`)
	// "Movie Title (2023)" or "Movie Title [2023]"
	reTitleYear = regexp.MustCompile(`^(?P<title>.+?)[\s._]*[\(\[](?P<year>(19|20)\d{2})[\)\]]`)
	// "Show.Name.S01E02" / "Show Name - S01E02" / "Show.Name.1x02"
	reSxxExx = regexp.MustCompile(`(?i)[Ss](\d{1,2})[\s._]*[Ee](\d{1,4})`)
	reNxN    = regexp.MustCompile(`(?i)(?:^|[\s._\-])(\d{1,2})x(\d{1,4})(?:[\s._\-]|$)`)
	// "Season 1" / "season01" / "第一季" / "第 1 季"
	reSeasonWord    = regexp.MustCompile(`(?i)season[\s._\-]*(\d{1,3})`)
	reSeasonChinese = regexp.MustCompile(`第\s*(\d{1,3})\s*[季部]`)
	// Chinese number one through ten for 第X季
	chineseSeasonMap = map[string]int{
		"一": 1, "二": 2, "三": 3, "四": 4, "五": 5,
		"六": 6, "七": 7, "八": 8, "九": 9, "十": 10,
	}
	reSeasonChineseNum = regexp.MustCompile(`第([一二三四五六七八九十])[季部]`)
	// Episode-only fallback for flat drops like "01 - Episode Name.mkv", "EP02.mkv"
	reEpisodeOnly = regexp.MustCompile(`(?i)(?:^|[\s._\-])(?:ep?|e|episode|集|第)?[\s._\-]*(\d{1,4})(?:\s|\.|_|-|集|$)`)
	// "E01" alone
	reJustE = regexp.MustCompile(`(?i)(?:^|[\s._\-])[Ee](\d{1,4})(?:[\s._\-]|$)`)
)

// ParsedName is the best-guess metadata from a filename or folder name.
type ParsedName struct {
	Title   string
	Year    int
	Season  int // 0 when unknown
	Episode int // 0 when unknown
	// Provider-assigned ids found in "[tmdb=N]" / "[imdb=ttN]" / "[tvdb=N]" tags.
	TMDBID string
	IMDBID string
	TVDBID string
}

// IsEpisode reports whether we parsed any season/episode hint.
func (p ParsedName) IsEpisode() bool { return p.Episode > 0 }

// ParseFilename extracts a guess from a single path segment (no directory context).
// Strips extension automatically.
func ParseFilename(name string) ParsedName {
	base := name
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return parseBase(base)
}

// ParseSeasonFolder extracts "Season N" from a folder name.
// Returns 0 when the folder doesn't look like a season folder.
func ParseSeasonFolder(folder string) int {
	if m := reSeasonWord.FindStringSubmatch(folder); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	if m := reSeasonChineseNum.FindStringSubmatch(folder); len(m) == 2 {
		if n, ok := chineseSeasonMap[m[1]]; ok {
			return n
		}
	}
	if m := reSeasonChinese.FindStringSubmatch(folder); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	// "Specials" is by convention season 0.
	if strings.EqualFold(strings.TrimSpace(folder), "specials") ||
		strings.Contains(folder, "特别篇") || strings.Contains(folder, "花絮") {
		return 0
	}
	return -1
}

// parseBase operates on the extensionless basename.
func parseBase(s string) ParsedName {
	p := ParsedName{}

	// 0. Extract any [tmdb=...] / [imdb=...] / [tvdb=...] tags, then strip them
	//    from the working string so they don't confuse the year / title regexes.
	for _, m := range reProviderTag.FindAllStringSubmatch(s, -1) {
		kind := strings.ToLower(m[1])
		val := m[2]
		switch kind {
		case "tmdb", "tmdbid":
			p.TMDBID = val
		case "imdb", "imdbid":
			p.IMDBID = val
		case "tvdb", "tvdbid":
			p.TVDBID = val
		}
	}
	s = reProviderTag.ReplaceAllString(s, "")

	// 1. Try SxxExx / NxN — strongest signal for TV.
	if m := reSxxExx.FindStringSubmatch(s); len(m) == 3 {
		p.Season, _ = strconv.Atoi(m[1])
		p.Episode, _ = strconv.Atoi(m[2])
		p.Title = cleanTitle(reSxxExx.Split(s, 2)[0])
		p.Year = extractYear(s)
		return p
	}
	if m := reNxN.FindStringSubmatch(s); len(m) == 3 {
		p.Season, _ = strconv.Atoi(m[1])
		p.Episode, _ = strconv.Atoi(m[2])
		p.Title = cleanTitle(reNxN.Split(s, 2)[0])
		p.Year = extractYear(s)
		return p
	}

	// 2. Explicit episode-only pattern: "E01", "EP01".
	if m := reJustE.FindStringSubmatch(s); len(m) == 2 {
		p.Episode, _ = strconv.Atoi(m[1])
		p.Title = cleanTitle(reJustE.Split(s, 2)[0])
		p.Year = extractYear(s)
		return p
	}

	// 3. Title (Year) — movie shape.
	if m := reTitleYear.FindStringSubmatch(s); len(m) >= 3 {
		p.Title = cleanTitle(m[1])
		p.Year, _ = strconv.Atoi(m[2])
		return p
	}

	// 4. Pure digit prefix (flat drops like "01 名字.mkv" or "02 -Ep name.mkv").
	if m := reEpisodeOnly.FindStringSubmatch(s); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		if n > 0 && n < 2000 {
			// Treat as episode only if it's a reasonable episode number; leave Season=0
			// so caller can infer from parent folder.
			p.Episode = n
			// Don't put the number in the title.
			p.Title = cleanTitle(reEpisodeOnly.ReplaceAllString(s, ""))
			p.Year = extractYear(s)
			return p
		}
	}

	// 5. Fallback: entire name is the title.
	p.Title = cleanTitle(s)
	p.Year = extractYear(s)
	return p
}

// extractYear finds the first 4-digit year 1900-2099 in the string, or 0.
var reAnyYear = regexp.MustCompile(`(19|20)\d{2}`)

func extractYear(s string) int {
	if m := reAnyYear.FindString(s); m != "" {
		if n, err := strconv.Atoi(m); err == nil {
			return n
		}
	}
	return 0
}

// cleanTitle normalizes common separators into spaces and trims.
func cleanTitle(s string) string {
	// Replace dots, underscores, and multiple dashes with spaces.
	s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	// Collapse multiple spaces.
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	// Drop leading/trailing punctuation.
	s = strings.Trim(s, " -[](){}")
	return s
}

// looksLikePlaceholder detects titles that came from a generic-looking filename
// ("01", "Part 1", short ASCII tokens) and would benefit from being replaced
// with a better candidate from the parent folder.
func looksLikePlaceholder(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	if len(s) <= 3 {
		return true
	}
	return false
}
