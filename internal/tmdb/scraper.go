package tmdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
)

// Scraper fills in missing metadata on existing video_list / video_season /
// video_episode / video_image rows by asking TMDB.
//
// Policy:
//   - We NEVER overwrite a non-null user-provided field. Scraper is purely additive.
//   - When a row already has tmdb_id, we use it directly.
//   - Otherwise we search by title + year; a match requires a year exact match
//     OR (when no year) a title substring overlap.
//   - Images are attached only when there is no existing video_image row of
//     the same type.
//   - All DB work is idempotent so operators can rerun scrape safely.
type Scraper struct {
	client *Client
	db     *db.DB
	log    *slog.Logger
}

// NewScraper wires up a scraper. tmdb may be nil if disabled (methods become no-ops).
func NewScraper(tmdb *Client, database *db.DB, log *slog.Logger) *Scraper {
	return &Scraper{client: tmdb, db: database, log: log}
}

// Enabled mirrors the underlying client's state.
func (s *Scraper) Enabled() bool { return s != nil && s.client != nil && s.client.Enabled() }

// ScrapeResult is the summary of a single list (movie/series) scrape.
type ScrapeResult struct {
	VideoListID     int64  `json:"video_list_id"`
	VideoType       string `json:"video_type,omitempty"`
	Title           string `json:"title,omitempty"`
	MatchedTMDBID   string `json:"matched_tmdb_id,omitempty"`
	MatchedTitle    string `json:"matched_title,omitempty"`
	UpdatedFields   int    `json:"updated_fields"`
	ImagesAttached  int    `json:"images_attached"`
	SeasonsUpdated  int    `json:"seasons_updated"`
	EpisodesUpdated int    `json:"episodes_updated"`
	Failed          bool   `json:"failed,omitempty"`
	Skipped         bool   `json:"skipped,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// BatchResult aggregates ScrapeResult for a bulk run.
type BatchResult struct {
	Total       int            `json:"total"`
	Processed   int            `json:"processed"`
	Active      int            `json:"active"`
	Matched     int            `json:"matched"`
	Skipped     int            `json:"skipped"`
	Failed      int            `json:"failed"`
	Duration    int64          `json:"duration_ms"`
	Errors      []string       `json:"errors,omitempty"`
	ActiveItems []ScrapeResult `json:"active_items,omitempty"`
	Items       []ScrapeResult `json:"items,omitempty"`
}

type ScrapeMissingOptions struct {
	MaxItems  int
	Force     bool
	LibraryID int64
	VideoType string
	Missing   string
	Progress  func(BatchResult)
}

type batchItem struct {
	id        int64
	videoType string
	title     string
}

var (
	tmdbLeadingBracketRe = regexp.MustCompile(`^\s*[\[\(【（{][^\]\)】）}]{1,80}[\]\)】）}]\s*`)
	tmdbTrailingNoiseRe  = regexp.MustCompile(`(?i)\s*[\[\(【（{][^\]\)】）}]*((1080|2160|720)p|4k|8k|hdr|web|bdrip|bluray|x264|x265|hevc|avc|aac|flac|简|繁|字幕|合集|全集|内封|外挂|[12][0-9]{3})[^\]\)】）}]*[\]\)】）}]\s*$`)
	tmdbSeasonSuffixRe   = regexp.MustCompile(`(?i)\s*(第\s*[0-9一二三四五六七八九十百]+\s*[季期]|第\s*[0-9一二三四五六七八九十百]+\s*クール|season\s*\d+|s\d+|part\s*\d+|cour\s*\d+|\d+(st|nd|rd|th)\s*season)\s*$`)
	tmdbYearTokenRe      = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)
	tmdbReleaseNoiseRe   = regexp.MustCompile(`(?i)\b(1080p|2160p|720p|4k|8k|hdr|web[-_. ]?dl|b[dr]rip|bluray|x264|x265|h264|h265|hevc|avc|aac|flac|gb|big5)\b|简繁|简体|繁体|字幕|合集|全集|内封|外挂|无修|NCOP|NCED`)
	tmdbEmptyBracketRe   = regexp.MustCompile(`[\[\(【（{]\s*[\]\)】）}]`)
	tmdbSpaceRe          = regexp.MustCompile(`[\s._\-+~:：/\\|]+`)
)

// ScrapeVideoList refreshes the metadata for a single video_list row.
// When ForceOverride is true, existing non-null fields ARE overwritten.
func (s *Scraper) ScrapeVideoList(ctx context.Context, videoListID int64, forceOverride bool) (*ScrapeResult, error) {
	if !s.Enabled() {
		return nil, errors.New("tmdb scraper disabled: set TMDB_API_KEY")
	}

	res := &ScrapeResult{VideoListID: videoListID}

	var (
		videoType   string
		tmdbID      sql.NullString
		title       string
		originTitle sql.NullString
		description sql.NullString
		dateAir     sql.NullTime
		runtime     sql.NullInt64
		tagline     sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT video_type, tmdb_id, title, origin_title, description, date_air, runtime, tagline
		FROM video_list WHERE id = ? AND deleted_at IS NULL LIMIT 1
	`, videoListID).Scan(&videoType, &tmdbID, &title, &originTitle, &description, &dateAir, &runtime, &tagline)
	if err != nil {
		return nil, fmt.Errorf("load video_list: %w", err)
	}
	res.VideoType = videoType
	res.Title = title

	// Step 1: resolve a TMDB id.
	resolvedID, err := s.resolveTMDBID(ctx, videoType, tmdbID.String, title, originTitle.String, yearOfTime(dateAir))
	if err != nil {
		res.Skipped = true
		res.Reason = err.Error()
		return res, nil
	}
	if resolvedID == 0 {
		res.Skipped = true
		res.Reason = "no TMDB match"
		return res, nil
	}
	res.MatchedTMDBID = strconv.FormatInt(resolvedID, 10)

	// Step 2: fetch details and apply.
	if videoType == db.VideoTypeMovie {
		movie, err := s.client.GetMovie(ctx, resolvedID)
		if err != nil {
			return nil, fmt.Errorf("get movie: %w", err)
		}
		s.fillMovieFallback(ctx, resolvedID, movie)
		res.MatchedTitle = movie.Title
		if err := s.applyMovie(ctx, videoListID, movie, res,
			tmdbID, title, originTitle, description, dateAir, runtime, tagline,
			forceOverride); err != nil {
			return nil, err
		}
	} else {
		show, err := s.client.GetTV(ctx, resolvedID)
		if err != nil {
			return nil, fmt.Errorf("get tv: %w", err)
		}
		s.fillTVFallback(ctx, resolvedID, show)
		res.MatchedTitle = show.Name
		if err := s.applyTV(ctx, videoListID, show, res,
			tmdbID, title, originTitle, description, dateAir, runtime, tagline,
			forceOverride); err != nil {
			return nil, err
		}
	}

	return res, nil
}

// resolveTMDBID returns a TMDB id if existing or searched.
func (s *Scraper) resolveTMDBID(ctx context.Context, videoType, existing, title, originTitle string, year int) (int64, error) {
	if existing != "" {
		if id, err := strconv.ParseInt(existing, 10, 64); err == nil && id > 0 {
			return id, nil
		}
	}
	candidates := tmdbTitleCandidates(title, originTitle)
	if len(candidates) == 0 {
		return 0, errors.New("title empty; cannot search")
	}

	for _, candidate := range candidates {
		id, err := s.searchTMDBCandidate(ctx, videoType, candidate, year)
		if err != nil {
			return 0, err
		}
		if id > 0 {
			return id, nil
		}
	}
	return 0, nil
}

func (s *Scraper) searchTMDBCandidate(ctx context.Context, videoType, title string, year int) (int64, error) {
	years := []int{year}
	if year > 0 {
		years = append(years, 0)
	}
	for _, searchYear := range years {
		results, err := s.searchTMDB(ctx, videoType, title, searchYear)
		if err != nil {
			return 0, err
		}
		if len(results) > 0 {
			return bestTMDBSearchResult(results, title, year).ID, nil
		}
	}
	return 0, nil
}

func (s *Scraper) searchTMDB(ctx context.Context, videoType, title string, year int) ([]SearchResult, error) {
	var (
		results []SearchResult
		err     error
	)
	if videoType == db.VideoTypeMovie {
		results, err = s.client.SearchMovie(ctx, title, year)
	} else {
		results, err = s.client.SearchTV(ctx, title, year)
	}
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Scraper) fillMovieFallback(ctx context.Context, tmdbID int64, movie *Movie) {
	if movie == nil || s.client == nil || strings.EqualFold(s.client.language, "en-US") {
		return
	}
	if strings.TrimSpace(movie.Overview) != "" && strings.TrimSpace(movie.Tagline) != "" {
		return
	}
	fallback, err := s.client.GetMovieWithLanguage(ctx, tmdbID, "en-US")
	if err != nil || fallback == nil {
		return
	}
	if strings.TrimSpace(movie.Overview) == "" {
		movie.Overview = fallback.Overview
	}
	if strings.TrimSpace(movie.Tagline) == "" {
		movie.Tagline = fallback.Tagline
	}
	if strings.TrimSpace(movie.Title) == "" {
		movie.Title = fallback.Title
	}
	if strings.TrimSpace(movie.OriginalTitle) == "" {
		movie.OriginalTitle = fallback.OriginalTitle
	}
	if strings.TrimSpace(movie.ReleaseDate) == "" {
		movie.ReleaseDate = fallback.ReleaseDate
	}
}

func (s *Scraper) fillTVFallback(ctx context.Context, tmdbID int64, show *TVShow) {
	if show == nil || s.client == nil || strings.EqualFold(s.client.language, "en-US") {
		return
	}
	if strings.TrimSpace(show.Overview) != "" && strings.TrimSpace(show.Tagline) != "" {
		return
	}
	fallback, err := s.client.GetTVWithLanguage(ctx, tmdbID, "en-US")
	if err != nil || fallback == nil {
		return
	}
	if strings.TrimSpace(show.Overview) == "" {
		show.Overview = fallback.Overview
	}
	if strings.TrimSpace(show.Tagline) == "" {
		show.Tagline = fallback.Tagline
	}
	if strings.TrimSpace(show.Name) == "" {
		show.Name = fallback.Name
	}
	if strings.TrimSpace(show.OriginalName) == "" {
		show.OriginalName = fallback.OriginalName
	}
	if strings.TrimSpace(show.FirstAirDate) == "" {
		show.FirstAirDate = fallback.FirstAirDate
	}
}

func tmdbTitleCandidates(title, originTitle string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		key := normalizeTMDBTitle(v)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, v)
	}
	for _, raw := range []string{title, originTitle} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		add(raw)
		cleaned := cleanTMDBSearchTitle(raw)
		add(cleaned)
		add(stripTMDBSeasonSuffix(cleaned))
		add(stripTMDBYear(cleaned))
		add(stripTMDBSeasonSuffix(stripTMDBYear(cleaned)))
	}
	return out
}

func cleanTMDBSearchTitle(title string) string {
	title = strings.TrimSpace(title)
	for {
		next := tmdbLeadingBracketRe.ReplaceAllString(title, "")
		if next == title || strings.TrimSpace(next) == "" {
			break
		}
		title = next
	}
	title = tmdbTrailingNoiseRe.ReplaceAllString(title, "")
	title = tmdbReleaseNoiseRe.ReplaceAllString(title, " ")
	title = tmdbEmptyBracketRe.ReplaceAllString(title, " ")
	title = strings.Trim(title, " \t\r\n._-+~:：/\\|")
	title = tmdbSpaceRe.ReplaceAllString(title, " ")
	return strings.TrimSpace(title)
}

func stripTMDBSeasonSuffix(title string) string {
	for {
		next := strings.TrimSpace(tmdbSeasonSuffixRe.ReplaceAllString(title, ""))
		if next == title || next == "" {
			return strings.TrimSpace(title)
		}
		title = next
	}
}

func stripTMDBYear(title string) string {
	out := strings.TrimSpace(tmdbYearTokenRe.ReplaceAllString(title, ""))
	out = tmdbSpaceRe.ReplaceAllString(out, " ")
	return strings.TrimSpace(out)
}

func bestTMDBSearchResult(results []SearchResult, query string, year int) SearchResult {
	best := results[0]
	bestScore := tmdbSearchScore(best, query, year)
	for _, candidate := range results[1:] {
		score := tmdbSearchScore(candidate, query, year)
		if score > bestScore || (score == bestScore && candidate.VoteAverage > best.VoteAverage) {
			best = candidate
			bestScore = score
		}
	}
	return best
}

func tmdbSearchScore(result SearchResult, query string, year int) int {
	q := normalizeTMDBTitle(query)
	score := 0
	for _, title := range []string{result.Title, result.Name, result.OriginalTitle, result.OriginalName} {
		t := normalizeTMDBTitle(title)
		if t == "" || q == "" {
			continue
		}
		switch {
		case t == q:
			score = max(score, 100)
		case strings.Contains(t, q) || strings.Contains(q, t):
			score = max(score, 80)
		default:
			score = max(score, tmdbTokenOverlapScore(q, t))
		}
	}
	if year > 0 {
		resultYear := tmdbResultYear(result)
		if resultYear == year {
			score += 20
		} else if resultYear > 0 && absInt(resultYear-year) <= 1 {
			score += 10
		}
	}
	return score
}

func tmdbTokenOverlapScore(a, b string) int {
	aa := strings.Fields(a)
	bb := strings.Fields(b)
	if len(aa) == 0 || len(bb) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, token := range aa {
		set[token] = true
	}
	hit := 0
	for _, token := range bb {
		if set[token] {
			hit++
		}
	}
	if hit == 0 {
		return 0
	}
	denom := len(aa)
	if len(bb) > denom {
		denom = len(bb)
	}
	return 60 * hit / denom
}

func normalizeTMDBTitle(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	var b strings.Builder
	lastSpace := false
	for _, r := range title {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func tmdbResultYear(result SearchResult) int {
	if y := yearOfDateString(result.ReleaseDate); y > 0 {
		return y
	}
	return yearOfDateString(result.FirstAirDate)
}

func yearOfDateString(raw string) int {
	if len(raw) < 4 {
		return 0
	}
	y, err := strconv.Atoi(raw[:4])
	if err != nil {
		return 0
	}
	return y
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// applyMovie writes TMDB fields onto video_list and video_image.
func (s *Scraper) applyMovie(
	ctx context.Context, videoListID int64, m *Movie, res *ScrapeResult,
	curTmdb sql.NullString, curTitle string,
	curOrigin, curDesc sql.NullString, curAir sql.NullTime,
	curRuntime sql.NullInt64, curTagline sql.NullString,
	force bool,
) error {
	updates, args := buildVideoListUpdates(
		curTmdb, curTitle, curOrigin, curDesc, curAir, curRuntime, curTagline,
		strconv.FormatInt(m.ID, 10), m.Title, m.OriginalTitle,
		m.Overview, parseDateOrZero(m.ReleaseDate), m.Runtime, m.Tagline,
		force,
	)
	if len(updates) > 0 {
		stmt := "UPDATE video_list SET " + strings.Join(updates, ", ") + " WHERE id = ?"
		args = append(args, videoListID)
		if _, err := s.db.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("update video_list: %w", err)
		}
		res.UpdatedFields = len(updates)
	}

	res.ImagesAttached += s.attachImages(ctx, emby.ItemIDTypeVideoList, videoListID, m.PosterPath, m.BackdropPath)
	return nil
}

// applyTV writes TMDB fields plus optional seasons/episodes.
func (s *Scraper) applyTV(
	ctx context.Context, videoListID int64, t *TVShow, res *ScrapeResult,
	curTmdb sql.NullString, curTitle string,
	curOrigin, curDesc sql.NullString, curAir sql.NullTime,
	curRuntime sql.NullInt64, curTagline sql.NullString,
	force bool,
) error {
	runtime := 0
	if len(t.EpisodeRuntime) > 0 {
		runtime = t.EpisodeRuntime[0]
	}
	updates, args := buildVideoListUpdates(
		curTmdb, curTitle, curOrigin, curDesc, curAir, curRuntime, curTagline,
		strconv.FormatInt(t.ID, 10), t.Name, t.OriginalName,
		t.Overview, parseDateOrZero(t.FirstAirDate), runtime, t.Tagline,
		force,
	)
	if len(updates) > 0 {
		stmt := "UPDATE video_list SET " + strings.Join(updates, ", ") + " WHERE id = ?"
		args = append(args, videoListID)
		if _, err := s.db.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("update video_list: %w", err)
		}
		res.UpdatedFields = len(updates)
	}
	res.ImagesAttached += s.attachImages(ctx, emby.ItemIDTypeVideoList, videoListID, t.PosterPath, t.BackdropPath)

	// Sync existing seasons/episodes from TMDB. We only touch rows already in
	// the DB; we don't synthesize new seasons from TMDB because the source of
	// truth for "what exists" is the operator's filesystem.
	res.SeasonsUpdated, res.EpisodesUpdated = s.syncSeasonsAndEpisodes(ctx, videoListID, t, force)
	return nil
}

// syncSeasonsAndEpisodes iterates DB season rows and updates them from TMDB.
func (s *Scraper) syncSeasonsAndEpisodes(ctx context.Context, videoListID int64, t *TVShow, force bool) (seasonsUpdated, episodesUpdated int) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, season_number FROM video_season WHERE video_list_id = ? AND deleted_at IS NULL",
		videoListID)
	if err != nil {
		s.log.Warn("list seasons failed", "err", err)
		return
	}
	defer rows.Close()

	type dbSeason struct {
		id  int64
		num int
	}
	var seasons []dbSeason
	for rows.Next() {
		var se dbSeason
		if err := rows.Scan(&se.id, &se.num); err == nil {
			seasons = append(seasons, se)
		}
	}

	for _, se := range seasons {
		// Find matching TMDB season summary.
		var tmdbSeason *Season
		for i := range t.Seasons {
			if t.Seasons[i].SeasonNumber == se.num {
				tmdbSeason = &t.Seasons[i]
				break
			}
		}
		if tmdbSeason == nil {
			continue
		}
		if err := s.updateSeason(ctx, se.id, tmdbSeason, force); err != nil {
			s.log.Warn("update season failed", "season_id", se.id, "err", err)
			continue
		}
		seasonsUpdated++

		// Fetch episode details.
		detail, err := s.client.GetSeason(ctx, t.ID, se.num)
		if err != nil {
			s.log.Warn("tmdb get season failed", "err", err)
			continue
		}
		for _, ep := range detail.Episodes {
			n, err := s.updateEpisode(ctx, se.id, ep, force)
			if err != nil {
				s.log.Warn("update episode failed", "err", err)
				continue
			}
			if n > 0 {
				episodesUpdated++
			}
		}
	}
	return
}

// updateSeason refreshes a single season row, attaches poster.
func (s *Scraper) updateSeason(ctx context.Context, seasonID int64, t *Season, force bool) error {
	var (
		curTitle string
		curDesc  sql.NullString
		curAir   sql.NullTime
	)
	_ = s.db.QueryRowContext(ctx,
		"SELECT title, description, date_air FROM video_season WHERE id = ?", seasonID,
	).Scan(&curTitle, &curDesc, &curAir)

	updates := []string{}
	args := []any{}

	if shouldSet(curTitle != "" && !looksLikeSeasonPlaceholderTitle(curTitle, t.SeasonNumber), force) && t.Name != "" {
		updates = append(updates, "title = ?")
		args = append(args, t.Name)
	}
	if shouldSet(curDesc.Valid && curDesc.String != "", force) && t.Overview != "" {
		updates = append(updates, "description = ?")
		args = append(args, t.Overview)
	}
	if shouldSet(curAir.Valid, force) {
		if d := parseDateOrZero(t.AirDate); !d.IsZero() {
			updates = append(updates, "date_air = ?")
			args = append(args, d)
		}
	}
	if len(updates) > 0 {
		stmt := "UPDATE video_season SET " + strings.Join(updates, ", ") + " WHERE id = ?"
		args = append(args, seasonID)
		if _, err := s.db.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
	}
	s.attachImages(ctx, emby.ItemIDTypeVideoSeason, seasonID, t.PosterPath, "")
	return nil
}

// updateEpisode refreshes a single episode row. Returns 1 if the episode row
// existed and was considered for update; 0 if we skipped it entirely.
func (s *Scraper) updateEpisode(ctx context.Context, seasonID int64, ep Episode, force bool) (int, error) {
	var (
		episodeID  int64
		curTitle   string
		curDesc    sql.NullString
		curAir     sql.NullTime
		curRuntime sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, title, description, date_air, runtime
		FROM video_episode
		WHERE video_season_id = ? AND episode_number = ? AND deleted_at IS NULL
		LIMIT 1
	`, seasonID, ep.EpisodeNumber).Scan(&episodeID, &curTitle, &curDesc, &curAir, &curRuntime)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	updates := []string{}
	args := []any{}
	if shouldSet(curTitle != "" && !looksLikeEpisodePlaceholderTitle(curTitle, ep.EpisodeNumber), force) && ep.Name != "" {
		updates = append(updates, "title = ?")
		args = append(args, ep.Name)
	}
	if shouldSet(curDesc.Valid && curDesc.String != "", force) && ep.Overview != "" {
		updates = append(updates, "description = ?")
		args = append(args, ep.Overview)
	}
	if shouldSet(curAir.Valid, force) {
		if d := parseDateOrZero(ep.AirDate); !d.IsZero() {
			updates = append(updates, "date_air = ?")
			args = append(args, d)
		}
	}
	if shouldSet(curRuntime.Valid && curRuntime.Int64 > 0, force) && ep.Runtime > 0 {
		updates = append(updates, "runtime = ?")
		args = append(args, ep.Runtime)
	}
	if len(updates) > 0 {
		stmt := "UPDATE video_episode SET " + strings.Join(updates, ", ") + " WHERE id = ?"
		args = append(args, episodeID)
		if _, err := s.db.ExecContext(ctx, stmt, args...); err != nil {
			return 0, err
		}
	}
	s.attachImages(ctx, emby.ItemIDTypeVideoEpisode, episodeID, ep.StillPath, "")
	return 1, nil
}

// attachImages inserts Primary and optionally Backdrop video_image rows from
// TMDB paths. Returns how many new rows were created (existing rows untouched).
func (s *Scraper) attachImages(ctx context.Context, relType string, relID int64, posterPath, backdropPath string) int {
	n := 0
	if posterPath != "" {
		if s.ensureImage(ctx, "Primary", relType, relID, db.ImagePathTypeTMDB, posterPath) {
			n++
		}
	}
	if backdropPath != "" {
		if s.ensureImage(ctx, "Backdrop", relType, relID, db.ImagePathTypeTMDB, backdropPath) {
			n++
		}
	}
	return n
}

// ensureImage inserts a new row iff there isn't one already. Returns true
// if inserted.
func (s *Scraper) ensureImage(ctx context.Context, imgType, relType string, relID int64, pathType, pathURL string) bool {
	var existing int64
	_ = s.db.QueryRowContext(ctx,
		"SELECT id FROM video_image WHERE relation_type = ? AND relation_id = ? AND type = ? AND deleted_at IS NULL LIMIT 1",
		relType, relID, imgType,
	).Scan(&existing)
	if existing > 0 {
		return false
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO video_image (type, relation_type, relation_id, path_type, path_url)
		VALUES (?, ?, ?, ?, ?)
	`, imgType, relType, relID, pathType, pathURL)
	return err == nil
}

// buildVideoListUpdates decides which columns to touch, honoring the "don't
// overwrite operator data" rule unless force is set.
func buildVideoListUpdates(
	curTmdb sql.NullString, curTitle string, curOrigin, curDesc sql.NullString,
	curAir sql.NullTime, curRuntime sql.NullInt64, curTagline sql.NullString,
	newTmdb, newTitle, newOrigin, newOverview string, newAir time.Time,
	newRuntime int, newTagline string,
	force bool,
) ([]string, []any) {
	updates := []string{}
	args := []any{}

	if shouldSet(curTmdb.Valid && curTmdb.String != "", force) && newTmdb != "" {
		updates = append(updates, "tmdb_id = ?")
		args = append(args, newTmdb)
	}
	// Title is a special case: we preserve the operator's choice by default,
	// but do update when it's empty or obviously a placeholder basename.
	if shouldSet(curTitle != "" && !looksLikePlaceholderTitle(curTitle), force) && newTitle != "" {
		updates = append(updates, "title = ?")
		args = append(args, newTitle)
	}
	if shouldSet(curOrigin.Valid && curOrigin.String != "", force) && newOrigin != "" {
		updates = append(updates, "origin_title = ?")
		args = append(args, newOrigin)
	}
	if shouldSet(curDesc.Valid && curDesc.String != "", force) && newOverview != "" {
		updates = append(updates, "description = ?")
		args = append(args, newOverview)
	}
	if shouldSet(curAir.Valid, force) && !newAir.IsZero() {
		updates = append(updates, "date_air = ?")
		args = append(args, newAir)
	}
	if shouldSet(curRuntime.Valid && curRuntime.Int64 > 0, force) && newRuntime > 0 {
		updates = append(updates, "runtime = ?")
		args = append(args, newRuntime)
	}
	if shouldSet(curTagline.Valid && curTagline.String != "", force) && newTagline != "" {
		updates = append(updates, "tagline = ?")
		args = append(args, newTagline)
	}
	return updates, args
}

// shouldSet returns true when we should update a column:
//   - the column is currently empty (hasValue=false), OR
//   - force is explicitly set.
func shouldSet(hasValue, force bool) bool {
	if force {
		return true
	}
	return !hasValue
}

// looksLikePlaceholderTitle detects titles that came from filename-only
// extraction, which we'd prefer to overwrite with a real TMDB title.
// Heuristic: all-ASCII + underscores/dashes is likely a placeholder.
func looksLikePlaceholderTitle(s string) bool {
	if strings.ContainsAny(s, "_") {
		return true
	}
	// Purely dashes/digits/ASCII is probably a filename stem, not a real title.
	for _, r := range s {
		if r > 0x7F {
			return false
		}
	}
	return true
}

func looksLikeSeasonPlaceholderTitle(title string, seasonNumber int) bool {
	s := strings.TrimSpace(title)
	if s == "" {
		return true
	}
	compact := strings.ToLower(strings.Join(strings.Fields(s), ""))
	n := strconv.Itoa(seasonNumber)
	return compact == "第"+n+"季" ||
		compact == "season"+n ||
		compact == "s"+n ||
		(seasonNumber == 0 && (compact == "specials" || compact == "special"))
}

func looksLikeEpisodePlaceholderTitle(title string, episodeNumber int) bool {
	s := strings.TrimSpace(title)
	if s == "" {
		return true
	}
	compact := strings.ToLower(strings.Join(strings.Fields(s), ""))
	n := strconv.Itoa(episodeNumber)
	n2 := fmt.Sprintf("%02d", episodeNumber)
	if compact == "e"+n || compact == "e"+n2 ||
		compact == "ep"+n || compact == "ep"+n2 ||
		compact == "episode"+n || compact == "episode"+n2 ||
		compact == "第"+n+"集" || compact == "第"+n2+"集" {
		return true
	}
	lower := strings.ToLower(s)
	for _, suffix := range []string{
		" e" + n, " e" + n2,
		" ep" + n, " ep" + n2,
		" episode " + n, " episode " + n2,
		" 第" + n + "集", " 第" + n2 + "集",
	} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// parseDateOrZero parses TMDB's YYYY-MM-DD date, returning zero for empty/bad.
func parseDateOrZero(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// yearOfTime extracts year from a sql.NullTime, 0 when null/zero.
func yearOfTime(nt sql.NullTime) int {
	if !nt.Valid || nt.Time.IsZero() {
		return 0
	}
	return nt.Time.Year()
}

// ScrapeAllMissing iterates every video_list that has nil description or
// missing primary image, and scrapes each. Controlled by a hard cap to
// protect against exhausting the TMDB quota on large libraries.
func (s *Scraper) ScrapeAllMissing(ctx context.Context, maxItems int, force bool) (*BatchResult, error) {
	return s.ScrapeMissing(ctx, ScrapeMissingOptions{MaxItems: maxItems, Force: force})
}

// ScrapeMissing iterates video_list rows that are missing metadata or artwork.
// Missing accepts: "", "any", "poster", "info", "unscraped".
func (s *Scraper) ScrapeMissing(ctx context.Context, opts ScrapeMissingOptions) (*BatchResult, error) {
	if !s.Enabled() {
		return nil, errors.New("tmdb scraper disabled")
	}
	start := time.Now()
	rep := &BatchResult{}

	where := []string{"vl.deleted_at IS NULL"}
	args := []any{}
	if opts.LibraryID > 0 {
		where = append(where, "vl.video_library_id = ?")
		args = append(args, opts.LibraryID)
	}
	if opts.VideoType == db.VideoTypeMovie || opts.VideoType == db.VideoTypeTV {
		where = append(where, "vl.video_type = ?")
		args = append(args, opts.VideoType)
	}
	switch strings.ToLower(strings.TrimSpace(opts.Missing)) {
	case "poster":
		where = append(where, missingPosterSQL())
	case "info":
		where = append(where, missingInfoSQL())
	case "unscraped":
		where = append(where, "(vl.tmdb_id IS NULL OR vl.tmdb_id = '')")
	default:
		where = append(where, "("+missingInfoSQL()+" OR "+missingPosterSQL()+")")
	}
	limitSQL := ""
	if opts.MaxItems > 0 {
		limitSQL = " LIMIT ?"
		args = append(args, opts.MaxItems)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT vl.id, vl.video_type, vl.title FROM video_list vl
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY vl.updated_at DESC`+limitSQL+`
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []batchItem
	for rows.Next() {
		var item batchItem
		if err := rows.Scan(&item.id, &item.videoType, &item.title); err == nil {
			ids = append(ids, item)
		}
	}

	s.scrapeBatch(ctx, ids, opts.Force, rep, opts.Progress)
	rep.Duration = time.Since(start).Milliseconds()
	return rep, nil
}

func missingInfoSQL() string {
	return "(vl.tmdb_id IS NULL OR vl.tmdb_id = '' OR vl.description IS NULL OR vl.description = '' OR vl.date_air IS NULL)"
}

func missingPosterSQL() string {
	return `NOT EXISTS (
		SELECT 1 FROM video_image vi
		WHERE vi.relation_type = 'vl' AND vi.relation_id = vl.id
		  AND vi.type = 'Primary' AND vi.deleted_at IS NULL
	)`
}

func (s *Scraper) scrapeBatch(ctx context.Context, ids []batchItem, force bool, rep *BatchResult, progress func(BatchResult)) {
	const workers = 8
	jobs := make(chan batchItem)
	events := make(chan scrapeBatchEvent)
	rep.Total = len(ids)
	if progress != nil {
		progress(*rep)
	}

	var wg sync.WaitGroup
	workerCount := workers
	if len(ids) < workerCount {
		workerCount = len(ids)
	}
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				started := item
				events <- scrapeBatchEvent{Started: &started}
				res, err := s.ScrapeVideoList(ctx, item.id, force)
				if err != nil {
					events <- scrapeBatchEvent{Result: &ScrapeResult{
						VideoListID: item.id,
						VideoType:   item.videoType,
						Title:       item.title,
						Failed:      true,
						Reason:      err.Error(),
					}}
					continue
				}
				if res == nil {
					events <- scrapeBatchEvent{Result: &ScrapeResult{
						VideoListID: item.id,
						VideoType:   item.videoType,
						Title:       item.title,
						Skipped:     true,
						Reason:      "empty result",
					}}
					continue
				}
				events <- scrapeBatchEvent{Result: res}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, item := range ids {
			if ctx.Err() != nil {
				return
			}
			jobs <- item
		}
	}()
	go func() {
		wg.Wait()
		close(events)
	}()

	active := map[int64]ScrapeResult{}
	for event := range events {
		if event.Started != nil {
			active[event.Started.id] = ScrapeResult{
				VideoListID: event.Started.id,
				VideoType:   event.Started.videoType,
				Title:       event.Started.title,
			}
			rep.ActiveItems = activeScrapeItems(active)
			rep.Active = len(rep.ActiveItems)
			if progress != nil {
				progress(*rep)
			}
			continue
		}
		if event.Result == nil {
			continue
		}
		res := *event.Result
		delete(active, res.VideoListID)
		rep.Processed++
		if res.Failed {
			rep.Failed++
			rep.Errors = append(rep.Errors, fmt.Sprintf("id=%d %s: %s", res.VideoListID, res.Title, res.Reason))
		} else if res.Skipped {
			rep.Skipped++
		} else {
			rep.Matched++
		}
		rep.Items = append(rep.Items, res)
		rep.ActiveItems = activeScrapeItems(active)
		rep.Active = len(rep.ActiveItems)
		if progress != nil {
			progress(*rep)
		}
	}
	if err := ctx.Err(); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		if progress != nil {
			progress(*rep)
		}
	}
}

type scrapeBatchEvent struct {
	Started *batchItem
	Result  *ScrapeResult
}

func activeScrapeItems(active map[int64]ScrapeResult) []ScrapeResult {
	if len(active) == 0 {
		return nil
	}
	out := make([]ScrapeResult, 0, len(active))
	for _, item := range active {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VideoListID < out[j].VideoListID })
	return out
}
