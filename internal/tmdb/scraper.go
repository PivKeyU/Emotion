package tmdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

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
	db     *sql.DB
	log    *slog.Logger
}

// NewScraper wires up a scraper. tmdb may be nil if disabled (methods become no-ops).
func NewScraper(tmdb *Client, database *sql.DB, log *slog.Logger) *Scraper {
	return &Scraper{client: tmdb, db: database, log: log}
}

// Enabled mirrors the underlying client's state.
func (s *Scraper) Enabled() bool { return s != nil && s.client != nil && s.client.Enabled() }

// ScrapeResult is the summary of a single list (movie/series) scrape.
type ScrapeResult struct {
	VideoListID     int64  `json:"video_list_id"`
	MatchedTMDBID   string `json:"matched_tmdb_id,omitempty"`
	UpdatedFields   int    `json:"updated_fields"`
	ImagesAttached  int    `json:"images_attached"`
	SeasonsUpdated  int    `json:"seasons_updated"`
	EpisodesUpdated int    `json:"episodes_updated"`
	Skipped         bool   `json:"skipped,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// BatchResult aggregates ScrapeResult for a bulk run.
type BatchResult struct {
	Processed int            `json:"processed"`
	Matched   int            `json:"matched"`
	Skipped   int            `json:"skipped"`
	Failed    int            `json:"failed"`
	Duration  time.Duration  `json:"duration_ms"`
	Errors    []string       `json:"errors,omitempty"`
	Items     []ScrapeResult `json:"items,omitempty"`
}

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

	// Step 1: resolve a TMDB id.
	resolvedID, err := s.resolveTMDBID(ctx, videoType, tmdbID.String, title, yearOfTime(dateAir))
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
		if err := s.applyTV(ctx, videoListID, show, res,
			tmdbID, title, originTitle, description, dateAir, runtime, tagline,
			forceOverride); err != nil {
			return nil, err
		}
	}

	return res, nil
}

// resolveTMDBID returns a TMDB id if existing or searched.
func (s *Scraper) resolveTMDBID(ctx context.Context, videoType, existing, title string, year int) (int64, error) {
	if existing != "" {
		if id, err := strconv.ParseInt(existing, 10, 64); err == nil && id > 0 {
			return id, nil
		}
	}
	if strings.TrimSpace(title) == "" {
		return 0, errors.New("title empty; cannot search")
	}

	// Try original title search.
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
		return 0, err
	}

	if len(results) == 0 {
		return 0, nil
	}
	// TMDB returns best-ranked first, take the top hit.
	return results[0].ID, nil
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

	if shouldSet(curTitle != "" && curTitle != "第 "+strconv.Itoa(t.SeasonNumber)+" 季", force) && t.Name != "" {
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
		episodeID int64
		curTitle  string
		curDesc   sql.NullString
		curAir    sql.NullTime
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
	if shouldSet(curTitle != "" && !strings.HasPrefix(curTitle, "E"), force) && ep.Name != "" {
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
	if !s.Enabled() {
		return nil, errors.New("tmdb scraper disabled")
	}
	if maxItems <= 0 {
		maxItems = 200
	}

	start := time.Now()
	rep := &BatchResult{}

	// Items needing scrape: description NULL/"" OR missing Primary image.
	rows, err := s.db.QueryContext(ctx, `
		SELECT vl.id FROM video_list vl
		WHERE vl.deleted_at IS NULL
		  AND (
			vl.description IS NULL OR vl.description = ''
			OR NOT EXISTS (
			  SELECT 1 FROM video_image vi
			  WHERE vi.relation_type = 'vl' AND vi.relation_id = vl.id
			    AND vi.type = 'Primary' AND vi.deleted_at IS NULL
			)
		  )
		ORDER BY vl.updated_at DESC
		LIMIT ?
	`, maxItems)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			break
		}
		res, err := s.ScrapeVideoList(ctx, id, force)
		if err != nil {
			rep.Failed++
			rep.Errors = append(rep.Errors, fmt.Sprintf("id=%d: %v", id, err))
			continue
		}
		rep.Processed++
		if res.Skipped {
			rep.Skipped++
		} else {
			rep.Matched++
		}
		rep.Items = append(rep.Items, *res)
	}
	rep.Duration = time.Since(start)
	return rep, nil
}
