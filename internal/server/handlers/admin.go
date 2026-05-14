package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

	scanMu   sync.Mutex
	scanJobs map[string]*scanJob

	watchMu   sync.Mutex
	watchJobs map[string]*watchJob
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
		db:        database,
		cfg:       cfg,
		log:       log,
		importer:  importer.New(database, log),
		tmdb:      client,
		scraper:   scraper,
		scanJobs:  map[string]*scanJob{},
		watchJobs: map[string]*watchJob{},
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

type adminFileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Type  string `json:"type"`
	Size  int64  `json:"size,omitempty"`
	Media bool   `json:"media,omitempty"`
}

// FilesBrowse lists container/server directories for the admin dashboard.
// GET /admin/files?path=/data
func (a *Admin) FilesBrowse(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path = "/data"
	}
	clean := filepath.Clean(path)
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		WriteText(w, http.StatusBadRequest, "directory not found: "+clean)
		return
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		WriteText(w, http.StatusForbidden, err.Error())
		return
	}
	out := []adminFileEntry{}
	if parent := filepath.Dir(clean); parent != clean {
		out = append(out, adminFileEntry{Name: "..", Path: parent, Type: "dir"})
	}
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		full := filepath.Join(clean, ent.Name())
		item := adminFileEntry{Name: ent.Name(), Path: full}
		if ent.IsDir() {
			item.Type = "dir"
		} else {
			item.Type = "file"
			item.Media = adminLooksMedia(ent.Name())
			if info, err := ent.Info(); err == nil {
				item.Size = info.Size()
			}
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type == "dir"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	WriteJSON(w, http.StatusOK, map[string]any{
		"path":    clean,
		"entries": out,
	})
}

func adminLooksMedia(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".mp4", ".m4v", ".ts", ".avi", ".mov", ".wmv", ".flv", ".webm", ".iso", ".rmvb",
		".strm", ".nfo", ".jpg", ".jpeg", ".png", ".webp", ".srt", ".ass", ".vtt", ".ssa", ".sub":
		return true
	}
	return false
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

type scanJob struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at,omitempty"`
	Progress   importer.Progress `json:"progress"`
	Result     any               `json:"result,omitempty"`
	Error      string            `json:"error,omitempty"`
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

	result, err := a.runScanJob(r.Context(), body, nil)
	if err != nil {
		a.log.Error("scan failed", "err", err)
		if errors.Is(err, sql.ErrNoRows) {
			WriteStatus(w, http.StatusNotFound)
			return
		}
		WriteText(w, http.StatusInternalServerError, err.Error())
		return
	}

	WriteJSON(w, http.StatusOK, result)
}

// LibraryScanStart starts an asynchronous scan job.
// POST /admin/library/scan/start
func (a *Admin) LibraryScanStart(w http.ResponseWriter, r *http.Request) {
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

	id := randomScanID()
	job := &scanJob{
		ID:        id,
		Status:    "running",
		StartedAt: time.Now(),
		Progress: importer.Progress{
			Stage:   "queued",
			Current: body.Root,
			Report:  importer.Report{StartedAt: time.Now()},
		},
	}
	a.scanMu.Lock()
	a.scanJobs[id] = job
	a.scanMu.Unlock()

	go func() {
		result, err := a.runScanJob(context.Background(), body, func(p importer.Progress) {
			a.updateScanJob(id, func(j *scanJob) {
				j.Progress = p
			})
		})
		a.updateScanJob(id, func(j *scanJob) {
			j.FinishedAt = time.Now()
			if err != nil {
				j.Status = "failed"
				j.Error = err.Error()
				j.Progress.Stage = "failed"
				return
			}
			j.Status = "done"
			j.Result = result
			j.Progress.Stage = "done"
		})
	}()

	WriteJSON(w, http.StatusAccepted, job)
}

// LibraryScanStatus returns the latest status for an async scan job.
// GET /admin/library/scan/{id}
func (a *Admin) LibraryScanStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "id")
	a.scanMu.Lock()
	job := a.cloneScanJob(a.scanJobs[id])
	a.scanMu.Unlock()
	if job == nil {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	WriteJSON(w, http.StatusOK, job)
}

func (a *Admin) runScanJob(ctx context.Context, body scanRequest, progress func(importer.Progress)) (any, error) {
	report, err := a.importer.Run(ctx, importer.Options{
		LibraryID:      body.LibraryID,
		Root:           body.Root,
		DefaultType:    body.DefaultType,
		FollowSymlinks: body.FollowSymlinks,
		DryRun:         body.DryRun,
		Logger:         a.log,
		Progress:       progress,
	})
	if err != nil {
		return nil, err
	}

	if !body.DryRun {
		mode := strings.ToLower(strings.TrimSpace(body.Scrape))
		wantScrape := mode == "on" || mode == "force" ||
			(mode == "" && a.cfg.TMDBAutoScrape)
		if wantScrape && a.scraper != nil && a.scraper.Enabled() {
			scrapeResults := make([]*tmdb.ScrapeResult, 0, len(report.TouchedVideoListIDs))
			for _, id := range report.TouchedVideoListIDs {
				res, err := a.scraper.ScrapeVideoList(ctx, id, mode == "force")
				if err != nil {
					a.log.Warn("tmdb scrape failed", "video_list_id", id, "err", err)
					continue
				}
				scrapeResults = append(scrapeResults, res)
			}
			return map[string]any{
				"import": report,
				"tmdb":   scrapeResults,
			}, nil
		}
	}

	return report, nil
}

func (a *Admin) updateScanJob(id string, fn func(*scanJob)) {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	if job := a.scanJobs[id]; job != nil {
		fn(job)
	}
}

func (a *Admin) cloneScanJob(job *scanJob) *scanJob {
	if job == nil {
		return nil
	}
	out := *job
	return &out
}

func randomScanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

type watchRequest struct {
	scanRequest
	IntervalSeconds int `json:"interval_seconds"`
}

type watchJob struct {
	ID              string    `json:"id"`
	Status          string    `json:"status"`
	StartedAt       time.Time `json:"started_at"`
	LastCheckedAt   time.Time `json:"last_checked_at,omitempty"`
	LastChangedAt   time.Time `json:"last_changed_at,omitempty"`
	LastScanID      string    `json:"last_scan_id,omitempty"`
	ChangeCount     int       `json:"change_count"`
	IntervalSeconds int       `json:"interval_seconds"`
	Root            string    `json:"root"`
	Error           string    `json:"error,omitempty"`
	cancel          context.CancelFunc
	snapshot        string
}

// LibraryWatchStart starts a polling directory watcher. It performs one full
// scan immediately, then triggers an incremental idempotent scan whenever file
// names, sizes, or mtimes under the root change.
// POST /admin/library/watch/start
func (a *Admin) LibraryWatchStart(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body watchRequest
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
	interval := body.IntervalSeconds
	if interval <= 0 {
		interval = 30
	}
	if interval < 5 {
		interval = 5
	}

	id := randomScanID()
	ctx, cancel := context.WithCancel(context.Background())
	job := &watchJob{
		ID:              id,
		Status:          "running",
		StartedAt:       time.Now(),
		IntervalSeconds: interval,
		Root:            body.Root,
		cancel:          cancel,
	}
	a.watchMu.Lock()
	a.watchJobs[id] = job
	a.watchMu.Unlock()

	go a.runWatchJob(ctx, id, scanRequest{
		LibraryID:      body.LibraryID,
		Root:           body.Root,
		DefaultType:    body.DefaultType,
		FollowSymlinks: body.FollowSymlinks,
		DryRun:         body.DryRun,
		Scrape:         body.Scrape,
	}, time.Duration(interval)*time.Second)
	WriteJSON(w, http.StatusAccepted, a.publicWatchJob(job))
}

// LibraryWatchStatus returns all watcher jobs or one watcher by id.
// GET /admin/library/watch or /admin/library/watch/{id}
func (a *Admin) LibraryWatchStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "id")
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	if id != "" {
		job := a.publicWatchJob(a.watchJobs[id])
		if job == nil {
			WriteStatus(w, http.StatusNotFound)
			return
		}
		WriteJSON(w, http.StatusOK, job)
		return
	}
	out := []any{}
	for _, job := range a.watchJobs {
		out = append(out, a.publicWatchJob(job))
	}
	WriteJSON(w, http.StatusOK, out)
}

// LibraryWatchStop stops a watcher.
// DELETE /admin/library/watch/{id}
func (a *Admin) LibraryWatchStop(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "id")
	a.watchMu.Lock()
	job := a.watchJobs[id]
	if job != nil {
		job.Status = "stopped"
		if job.cancel != nil {
			job.cancel()
		}
	}
	a.watchMu.Unlock()
	if job == nil {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	WriteStatus(w, http.StatusNoContent)
}

func (a *Admin) runWatchJob(ctx context.Context, id string, req scanRequest, interval time.Duration) {
	trigger := func() {
		scanID := randomScanID()
		a.updateWatchJob(id, func(j *watchJob) {
			j.LastScanID = scanID
		})
		job := &scanJob{
			ID:        scanID,
			Status:    "running",
			StartedAt: time.Now(),
			Progress:  importer.Progress{Stage: "queued", Current: req.Root, Report: importer.Report{StartedAt: time.Now()}},
		}
		a.scanMu.Lock()
		a.scanJobs[scanID] = job
		a.scanMu.Unlock()
		result, err := a.runScanJob(ctx, req, func(p importer.Progress) {
			a.updateScanJob(scanID, func(j *scanJob) { j.Progress = p })
		})
		a.updateScanJob(scanID, func(j *scanJob) {
			j.FinishedAt = time.Now()
			if err != nil {
				j.Status = "failed"
				j.Error = err.Error()
				j.Progress.Stage = "failed"
				return
			}
			j.Status = "done"
			j.Result = result
			j.Progress.Stage = "done"
		})
		if err != nil {
			a.updateWatchJob(id, func(j *watchJob) { j.Error = err.Error() })
		}
	}

	trigger()
	snap, err := directorySnapshot(req.Root)
	a.updateWatchJob(id, func(j *watchJob) {
		j.LastCheckedAt = time.Now()
		if err != nil {
			j.Error = err.Error()
			return
		}
		j.snapshot = snap
	})

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap, err := directorySnapshot(req.Root)
			changed := false
			a.updateWatchJob(id, func(j *watchJob) {
				j.LastCheckedAt = time.Now()
				if err != nil {
					j.Error = err.Error()
					return
				}
				changed = j.snapshot != "" && j.snapshot != snap
				j.snapshot = snap
				if changed {
					j.ChangeCount++
					j.LastChangedAt = time.Now()
					j.Error = ""
				}
			})
			if changed {
				trigger()
			}
		}
	}
}

func (a *Admin) updateWatchJob(id string, fn func(*watchJob)) {
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	if job := a.watchJobs[id]; job != nil {
		fn(job)
	}
}

func (a *Admin) publicWatchJob(job *watchJob) map[string]any {
	if job == nil {
		return nil
	}
	return map[string]any{
		"id":               job.ID,
		"status":           job.Status,
		"started_at":       job.StartedAt,
		"last_checked_at":  job.LastCheckedAt,
		"last_changed_at":  job.LastChangedAt,
		"last_scan_id":     job.LastScanID,
		"change_count":     job.ChangeCount,
		"interval_seconds": job.IntervalSeconds,
		"root":             job.Root,
		"error":            job.Error,
	}
}

func directorySnapshot(root string) (string, error) {
	var rows []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		kind := importer.FileKindOther
		switch strings.ToLower(filepath.Ext(path)) {
		case ".mkv", ".mp4", ".m4v", ".ts", ".avi", ".mov", ".wmv", ".flv", ".webm", ".iso", ".rmvb",
			".strm", ".nfo", ".jpg", ".jpeg", ".png", ".webp", ".srt", ".ass", ".vtt", ".ssa", ".sub":
			kind = 1
		}
		if kind == importer.FileKindOther {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rows = append(rows, fmt.Sprintf("%s|%d|%d", rel, info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(rows)
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(sum[:]), nil
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
