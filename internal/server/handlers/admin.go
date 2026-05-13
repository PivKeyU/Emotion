package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/importer"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
	"github.com/PivKeyU/Emotion/internal/tmdb"
)

// Admin serves /admin/* endpoints for manual library management.
// All endpoints require the admin API key or an admin user.
type Admin struct {
	db       *db.DB
	cfg      *config.Config
	log      *slog.Logger
	importer *importer.Importer
	tmdb     *tmdb.Client // may be nil
	scraper  *tmdb.Scraper
}

// NewAdmin constructs the handler.
func NewAdmin(database *db.DB, cfg *config.Config, log *slog.Logger) *Admin {
	var (
		client  *tmdb.Client
		scraper *tmdb.Scraper
	)
	if cfg.TMDBAPIKey != "" {
		client = tmdb.NewClient(cfg.TMDBAPIKey, tmdb.WithLanguage(cfg.TMDBLanguage))
		scraper = tmdb.NewScraper(client, database, log)
	}
	return &Admin{
		db:       database,
		cfg:      cfg,
		log:      log,
		importer: importer.New(database, log),
		tmdb:     client,
		scraper:  scraper,
	}
}

func (a *Admin) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if ctxpkg.IsAPIKey(r.Context()) || ctxpkg.IsAdmin(r.Context()) {
		return true
	}
	WriteText(w, http.StatusForbidden, "需要管理员权限")
	return false
}

// LibrariesList returns every library (admin view).
// GET /admin/libraries
func (a *Admin) LibrariesList(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, name, role, created_at FROM library
		 WHERE deleted_at IS NULL ORDER BY id ASC`)
	if err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var (
			id        int64
			name      string
			role      sql.NullString
			createdAt sql.NullTime
		)
		if err := rows.Scan(&id, &name, &role, &createdAt); err != nil {
			continue
		}
		m := map[string]any{"id": id, "name": name, "role": role.String}
		if createdAt.Valid {
			m["created_at"] = createdAt.Time
		}
		out = append(out, m)
	}
	WriteJSON(w, http.StatusOK, out)
}

// libraryCreateBody is the POST /admin/libraries body.
type libraryCreateBody struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// LibraryCreate creates a new library.
// POST /admin/libraries
func (a *Admin) LibraryCreate(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body libraryCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteText(w, http.StatusBadRequest, "invalid body")
		return
	}
	defer r.Body.Close()
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		WriteText(w, http.StatusBadRequest, "name required")
		return
	}
	role := sql.NullString{Valid: body.Role != "", String: body.Role}
	res, err := a.db.ExecContext(r.Context(),
		"INSERT INTO library (name, role) VALUES (?, ?)", body.Name, role)
	if err != nil {
		a.log.Error("library create failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	WriteJSON(w, http.StatusCreated, map[string]any{
		"id":   id,
		"name": body.Name,
		"role": body.Role,
	})
}

// LibraryDelete soft-deletes a library.
// DELETE /admin/libraries/{id}
func (a *Admin) LibraryDelete(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	if _, err := a.db.ExecContext(r.Context(),
		"UPDATE library SET deleted_at = NOW() WHERE id = ?", id); err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteStatus(w, http.StatusNoContent)
}

// AdminMediaItem is a compact catalog row for the visual admin dashboard.
type AdminMediaItem struct {
	ID            int64  `json:"id"`
	ItemID        string `json:"item_id"`
	LibraryID     int64  `json:"library_id"`
	Type          string `json:"type"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title,omitempty"`
	Year          int    `json:"year,omitempty"`
	TMDBID        string `json:"tmdb_id,omitempty"`
	Overview      string `json:"overview,omitempty"`
	PosterURL     string `json:"poster_url,omitempty"`
	BackdropURL   string `json:"backdrop_url,omitempty"`
	MediaCount    int64  `json:"media_count"`
	SeasonCount   int64  `json:"season_count"`
	EpisodeCount  int64  `json:"episode_count"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

// AdminMediaListResponse is returned by GET /admin/media.
type AdminMediaListResponse struct {
	Items []AdminMediaItem `json:"items"`
	Total int64            `json:"total"`
}

// AdminMediaList returns poster-ready media rows for the visual admin dashboard.
// GET /admin/media?library_id=1&type=movie|tv&search=...&limit=60&offset=0
func (a *Admin) AdminMediaList(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	q := r.URL.Query()
	limit := parsePositiveInt(q.Get("limit"), 60)
	if limit > 120 {
		limit = 120
	}
	offset := parsePositiveInt(q.Get("offset"), 0)

	var (
		where []string
		args  []any
	)
	where = append(where, "vl.deleted_at IS NULL")
	if libID := parsePositiveInt64(q.Get("library_id"), 0); libID > 0 {
		where = append(where, "vl.video_library_id = ?")
		args = append(args, libID)
	}
	if typ := strings.TrimSpace(q.Get("type")); typ == db.VideoTypeMovie || typ == db.VideoTypeTV {
		where = append(where, "vl.video_type = ?")
		args = append(args, typ)
	}
	if search := strings.TrimSpace(q.Get("search")); search != "" {
		where = append(where, "(vl.title LIKE ? OR vl.origin_title LIKE ?)")
		like := "%" + search + "%"
		args = append(args, like, like)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int64
	if err := a.db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM video_list vl WHERE "+whereSQL, args...,
	).Scan(&total); err != nil {
		a.log.Error("admin media count failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, limit, offset)
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT
			vl.id,
			vl.video_library_id,
			vl.video_type,
			vl.title,
			COALESCE(vl.origin_title, ''),
			COALESCE(vl.tmdb_id, ''),
			COALESCE(vl.description, ''),
			vl.date_air,
			vl.updated_at,
			COUNT(DISTINCT vm.id) AS media_count,
			COUNT(DISTINCT vs.id) AS season_count,
			COUNT(DISTINCT ve.id) AS episode_count,
			COALESCE(MAX(CASE WHEN vip.type = 'Primary' THEN vip.path_type END), '') AS poster_type,
			COALESCE(MAX(CASE WHEN vip.type = 'Primary' THEN vip.path_url END), '') AS poster_url,
			COALESCE(MAX(CASE WHEN vib.type = 'Backdrop' THEN vib.path_type END), '') AS backdrop_type,
			COALESCE(MAX(CASE WHEN vib.type = 'Backdrop' THEN vib.path_url END), '') AS backdrop_url
		FROM video_list vl
		LEFT JOIN video_media vm
			ON vm.video_list_id = vl.id AND vm.deleted_at IS NULL
		LEFT JOIN video_season vs
			ON vs.video_list_id = vl.id AND vs.deleted_at IS NULL
		LEFT JOIN video_episode ve
			ON ve.video_list_id = vl.id AND ve.deleted_at IS NULL
		LEFT JOIN video_image vip
			ON vip.relation_type = ? AND vip.relation_id = vl.id
			AND vip.type = ? AND vip.deleted_at IS NULL
		LEFT JOIN video_image vib
			ON vib.relation_type = ? AND vib.relation_id = vl.id
			AND vib.type = ? AND vib.deleted_at IS NULL
		WHERE `+whereSQL+`
		GROUP BY vl.id
		ORDER BY vl.updated_at DESC, vl.id DESC
		LIMIT ? OFFSET ?`,
		append([]any{
			emby.ItemIDTypeVideoList, db.ImageTypePrimary,
			emby.ItemIDTypeVideoList, db.ImageTypeBackdrop,
		}, queryArgs...)...,
	)
	if err != nil {
		a.log.Error("admin media list failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	items := []AdminMediaItem{}
	for rows.Next() {
		var (
			item         AdminMediaItem
			originTitle  string
			tmdbID       string
			overview     string
			dateAir      sql.NullTime
			updatedAt    sql.NullTime
			posterType   string
			posterPath   string
			backdropType string
			backdropPath string
		)
		if err := rows.Scan(
			&item.ID, &item.LibraryID, &item.Type, &item.Title,
			&originTitle, &tmdbID, &overview, &dateAir, &updatedAt,
			&item.MediaCount, &item.SeasonCount, &item.EpisodeCount,
			&posterType, &posterPath, &backdropType, &backdropPath,
		); err != nil {
			a.log.Warn("admin media row skipped", "err", err)
			continue
		}
		item.ItemID = emby.ItemID(emby.ItemIDTypeVideoList, item.ID)
		item.OriginalTitle = originTitle
		item.TMDBID = tmdbID
		item.Overview = overview
		if dateAir.Valid {
			item.Year = dateAir.Time.Year()
		}
		if updatedAt.Valid {
			item.UpdatedAt = updatedAt.Time.UTC().Format("2006-01-02T15:04:05Z")
		}
		item.PosterURL = adminImageURL(posterType, posterPath, item.ItemID, db.ImageTypePrimary)
		item.BackdropURL = adminImageURL(backdropType, backdropPath, item.ItemID, db.ImageTypeBackdrop)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		a.log.Error("admin media rows failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	WriteJSON(w, http.StatusOK, AdminMediaListResponse{Items: items, Total: total})
}

type adminMediaSeason struct {
	ID           int64  `json:"id"`
	ItemID       string `json:"item_id"`
	Title        string `json:"title"`
	SeasonNumber int64  `json:"season_number"`
	EpisodeCount int64  `json:"episode_count"`
}

type adminMediaEpisode struct {
	ID            int64  `json:"id"`
	ItemID        string `json:"item_id"`
	Title         string `json:"title"`
	SeasonID      int64  `json:"season_id,omitempty"`
	SeasonNumber  int64  `json:"season_number,omitempty"`
	EpisodeNumber int64  `json:"episode_number"`
	MediaCount    int64  `json:"media_count"`
}

type adminMediaSource struct {
	ID      int64  `json:"id"`
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	PathURL string `json:"path_url,omitempty"`
}

// AdminMediaChildren returns expandable children/details for one video_list.
// GET /admin/media/{id}/children
func (a *Admin) AdminMediaChildren(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	var videoType string
	if err := a.db.QueryRowContext(r.Context(),
		"SELECT video_type FROM video_list WHERE id = ? AND deleted_at IS NULL LIMIT 1", id,
	).Scan(&videoType); err != nil {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	seasons, err := a.adminSeasons(r, id)
	if err != nil {
		a.log.Error("admin seasons failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	episodes, err := a.adminEpisodes(r, id)
	if err != nil {
		a.log.Error("admin episodes failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	sources, err := a.adminSources(r, id)
	if err != nil {
		a.log.Error("admin sources failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"type":     videoType,
		"seasons":  seasons,
		"episodes": episodes,
		"sources":  sources,
	})
}

func (a *Admin) adminSeasons(r *http.Request, videoListID int64) ([]adminMediaSeason, error) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT vs.id, vs.season_number, vs.title, COUNT(ve.id) AS episode_count
		FROM video_season vs
		LEFT JOIN video_episode ve
			ON ve.video_season_id = vs.id AND ve.deleted_at IS NULL
		WHERE vs.video_list_id = ? AND vs.deleted_at IS NULL
		GROUP BY vs.id, vs.season_number, vs.title
		ORDER BY vs.season_number ASC, vs.id ASC`, videoListID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []adminMediaSeason{}
	for rows.Next() {
		var s adminMediaSeason
		if err := rows.Scan(&s.ID, &s.SeasonNumber, &s.Title, &s.EpisodeCount); err != nil {
			return nil, err
		}
		s.ItemID = emby.ItemID(emby.ItemIDTypeVideoSeason, s.ID)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (a *Admin) adminEpisodes(r *http.Request, videoListID int64) ([]adminMediaEpisode, error) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT
			ve.id,
			ve.video_season_id,
			COALESCE(vs.season_number, 0),
			ve.episode_number,
			ve.title,
			COUNT(vm.id) AS media_count
		FROM video_episode ve
		LEFT JOIN video_season vs
			ON vs.id = ve.video_season_id AND vs.deleted_at IS NULL
		LEFT JOIN video_media vm
			ON vm.video_episode_id = ve.id AND vm.deleted_at IS NULL
		WHERE ve.video_list_id = ? AND ve.deleted_at IS NULL
		GROUP BY ve.id, ve.video_season_id, vs.season_number, ve.episode_number, ve.title
		ORDER BY COALESCE(vs.season_number, 0), ve.episode_number, ve.id`, videoListID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []adminMediaEpisode{}
	for rows.Next() {
		var e adminMediaEpisode
		if err := rows.Scan(&e.ID, &e.SeasonID, &e.SeasonNumber, &e.EpisodeNumber, &e.Title, &e.MediaCount); err != nil {
			return nil, err
		}
		e.ItemID = emby.ItemID(emby.ItemIDTypeVideoEpisode, e.ID)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (a *Admin) adminSources(r *http.Request, videoListID int64) ([]adminMediaSource, error) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, uuid, name, COALESCE(path_url, '')
		FROM video_media
		WHERE video_list_id = ? AND deleted_at IS NULL
		ORDER BY id ASC
		LIMIT 50`, videoListID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []adminMediaSource{}
	for rows.Next() {
		var s adminMediaSource
		if err := rows.Scan(&s.ID, &s.UUID, &s.Name, &s.PathURL); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func adminImageURL(pathType, pathURL, itemID, imageType string) string {
	if pathURL == "" {
		return ""
	}
	switch pathType {
	case db.ImagePathTypeTMDB:
		return "https://image.tmdb.org/t/p/w400" + pathURL
	case db.ImagePathTypeDouban, db.ImagePathTypeURL:
		return pathURL
	case db.PathTypeLocal:
		return "/Items/" + itemID + "/Images/" + imageType
	default:
		return pathURL
	}
}

func parsePositiveInt(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return def
	}
	return n
}

func parsePositiveInt64(raw string, def int64) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// scanRequest is the POST body for /admin/library/scan.
type scanRequest struct {
	LibraryID      int64  `json:"library_id"`
	Root           string `json:"root"`
	DefaultType    string `json:"default_type"` // movie | tv | (empty = auto)
	FollowSymlinks bool   `json:"follow_symlinks"`
	DryRun         bool   `json:"dry_run"`
	// Scrape controls TMDB backfill on items touched by this import.
	// Accepts: "" (use server default TMDB_AUTO_SCRAPE), "on", "off", "force".
	Scrape string `json:"scrape"`
}

// LibraryScan runs a synchronous import from a local directory.
//
// POST /admin/library/scan
//
//	{
//	  "library_id": 1,
//	  "root": "/data/movies",
//	  "default_type": "movie",
//	  "scrape": "on"
//	}
//
// Returns a JSON report on completion. Safe to call repeatedly: writes are idempotent.
func (a *Admin) LibraryScan(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body scanRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteText(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	defer r.Body.Close()

	if body.Root == "" {
		WriteText(w, http.StatusBadRequest, "root is required")
		return
	}
	if _, err := os.Stat(body.Root); err != nil {
		WriteText(w, http.StatusBadRequest, "root does not exist: "+body.Root)
		return
	}
	if body.LibraryID <= 0 {
		WriteText(w, http.StatusBadRequest, "library_id is required")
		return
	}

	report, err := a.importer.Run(r.Context(), importer.Options{
		LibraryID:      body.LibraryID,
		Root:           body.Root,
		DefaultType:    body.DefaultType,
		FollowSymlinks: body.FollowSymlinks,
		DryRun:         body.DryRun,
		Logger:         a.log,
	})
	if err != nil {
		a.log.Error("scan failed", "err", err)
		if errors.Is(err, sql.ErrNoRows) {
			WriteStatus(w, http.StatusNotFound)
			return
		}
		WriteText(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Optional TMDB backfill for items touched by this import.
	if !body.DryRun {
		mode := strings.ToLower(strings.TrimSpace(body.Scrape))
		wantScrape := mode == "on" || mode == "force" ||
			(mode == "" && a.cfg.TMDBAutoScrape)
		if wantScrape && a.scraper != nil && a.scraper.Enabled() {
			scrapeResults := make([]*tmdb.ScrapeResult, 0, len(report.TouchedVideoListIDs))
			for _, id := range report.TouchedVideoListIDs {
				res, err := a.scraper.ScrapeVideoList(r.Context(), id, mode == "force")
				if err != nil {
					a.log.Warn("tmdb scrape failed", "video_list_id", id, "err", err)
					continue
				}
				scrapeResults = append(scrapeResults, res)
			}
			WriteJSON(w, http.StatusOK, map[string]any{
				"import": report,
				"tmdb":   scrapeResults,
			})
			return
		}
	}

	WriteJSON(w, http.StatusOK, report)
}

// TMDBRefreshOneRequest is the POST body for refreshing a single item.
type tmdbRefreshRequest struct {
	Force bool `json:"force"`
}

// TMDBRefreshOne refreshes TMDB metadata for one video_list by id.
//
// POST /admin/items/{id}/tmdb/refresh  {"force": false}
func (a *Admin) TMDBRefreshOne(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.scraper == nil || !a.scraper.Enabled() {
		WriteText(w, http.StatusServiceUnavailable, "TMDB disabled: set TMDB_API_KEY")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	var body tmdbRefreshRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	res, err := a.scraper.ScrapeVideoList(r.Context(), id, body.Force)
	if err != nil {
		a.log.Error("tmdb refresh failed", "err", err)
		WriteText(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// TMDBRefreshAllRequest is the POST body for the bulk refresh endpoint.
type tmdbRefreshAllRequest struct {
	Max   int  `json:"max"`
	Force bool `json:"force"`
}

// TMDBRefreshAll scans all items missing metadata and refreshes each.
//
// POST /admin/tmdb/refresh-all  {"max": 200, "force": false}
func (a *Admin) TMDBRefreshAll(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.scraper == nil || !a.scraper.Enabled() {
		WriteText(w, http.StatusServiceUnavailable, "TMDB disabled: set TMDB_API_KEY")
		return
	}
	var body tmdbRefreshAllRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	rep, err := a.scraper.ScrapeAllMissing(r.Context(), body.Max, body.Force)
	if err != nil {
		a.log.Error("tmdb refresh-all failed", "err", err)
		WriteText(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, rep)
}
