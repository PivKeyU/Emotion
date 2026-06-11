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

	"github.com/PivKeyU/Emotion/internal/auth"
	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/importer"
	"github.com/PivKeyU/Emotion/internal/logger"
	"github.com/PivKeyU/Emotion/internal/metadata"
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

	tmdbMu   sync.Mutex
	tmdbJobs map[string]*tmdbRefreshJob

	probeMu   sync.Mutex
	probeJobs map[string]*mediaProbeJob
}

// NewAdmin constructs the handler.
func NewAdmin(database *db.DB, cfg *config.Config, log *slog.Logger) *Admin {
	var (
		client  *tmdb.Client
		scraper *tmdb.Scraper
		opts    []tmdb.ScraperOption
	)
	if cfg.TMDBAPIKey != "" {
		client = tmdb.NewClient(cfg.TMDBAPIKey, tmdb.WithLanguage(cfg.TMDBLanguage))
	}
	if strings.TrimSpace(cfg.TVDBAPIKey) != "" {
		opts = append(opts, tmdb.WithTVDBClient(metadata.NewTVDBClient(cfg.TVDBAPIKey, cfg.TVDBPIN)))
	}
	if strings.TrimSpace(cfg.OMDBAPIKey) != "" {
		opts = append(opts, tmdb.WithOMDBClient(metadata.NewOMDBClient(cfg.OMDBAPIKey)))
	}
	if client != nil || len(opts) > 0 {
		scraper = tmdb.NewScraper(client, database, log, opts...)
	}
	a := &Admin{
		db:        database,
		cfg:       cfg,
		log:       log,
		importer:  importer.New(database, log),
		tmdb:      client,
		scraper:   scraper,
		scanJobs:  map[string]*scanJob{},
		watchJobs: map[string]*watchJob{},
		tmdbJobs:  map[string]*tmdbRefreshJob{},
		probeJobs: map[string]*mediaProbeJob{},
	}
	settings := a.loadTMDBSettings(context.Background())
	a.cfg.TMDBAutoScrape = settings.AutoScrape
	a.cfg.TVDBAPIKey = a.rawSetting(context.Background(), "tvdb_api_key", cfg.TVDBAPIKey)
	a.cfg.TVDBPIN = a.rawSetting(context.Background(), "tvdb_pin", cfg.TVDBPIN)
	a.cfg.OMDBAPIKey = a.rawSetting(context.Background(), "omdb_api_key", cfg.OMDBAPIKey)
	a.rebuildTMDB(a.rawSetting(context.Background(), "tmdb_api_key", cfg.TMDBAPIKey), settings.Language)
	go a.startConfiguredWatchers()
	return a
}

func (a *Admin) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if ctxpkg.IsAPIKey(r.Context()) || ctxpkg.IsAdmin(r.Context()) {
		return true
	}
	WriteText(w, http.StatusForbidden, "需要管理员权限")
	return false
}

type loginRequest struct {
	APIKey string `json:"api_key"`
}

// Login validates the bootstrap admin key and returns a dashboard session token.
// POST /admin/login
func (a *Admin) Login(w http.ResponseWriter, r *http.Request) {
	var body loginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteText(w, http.StatusBadRequest, "invalid body")
		return
	}
	defer r.Body.Close()
	if strings.TrimSpace(a.cfg.APIKey) == "" {
		WriteText(w, http.StatusServiceUnavailable, "server API_KEY is not configured")
		return
	}
	if body.APIKey != a.cfg.APIKey {
		a.log.Warn("admin login failed", "category", "auth")
		WriteText(w, http.StatusUnauthorized, "登录失败")
		return
	}
	token := auth.RandomToken(32)
	expires := time.Now().Add(7 * 24 * time.Hour)
	if _, err := a.db.ExecContext(r.Context(), `
		INSERT INTO admin_session (token, expires_at)
		VALUES (?, ?)
	`, token, expires); err != nil {
		a.log.Error("admin session create failed", "category", "auth", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	a.log.Info("admin login ok", "category", "auth")
	WriteJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expires,
	})
}

func (a *Admin) hydrateScanRequest(ctx context.Context, body *scanRequest) error {
	if body.LibraryID <= 0 {
		return errors.New("library_id is required")
	}
	var (
		role sql.NullString
		root sql.NullString
	)
	if err := a.db.QueryRowContext(ctx,
		"SELECT role, root_path FROM library WHERE id = ? AND deleted_at IS NULL LIMIT 1", body.LibraryID,
	).Scan(&role, &root); err != nil {
		return err
	}
	if strings.TrimSpace(body.DefaultType) == "" && role.Valid {
		body.DefaultType = role.String
	}
	if strings.TrimSpace(body.Root) == "" && root.Valid {
		body.Root = root.String
	}
	if strings.TrimSpace(body.Root) != "" {
		_, _ = a.db.ExecContext(ctx,
			"UPDATE library SET root_path = ?, updated_at = NOW() WHERE id = ?", body.Root, body.LibraryID)
	}
	return nil
}

// Logs returns captured backend logs for the dashboard.
// GET /admin/logs?level=&category=&limit=200
func (a *Admin) Logs(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	limit := parsePositiveInt(r.URL.Query().Get("limit"), 200)
	WriteJSON(w, http.StatusOK, map[string]any{
		"items": logger.Recent(r.URL.Query().Get("level"), r.URL.Query().Get("category"), limit),
	})
}

type apiKeyCreateRequest struct {
	Name   string `json:"name"`
	Remark string `json:"remark"`
}

// APIKeysList lists server-generated keys without revealing the secret.
// GET /admin/api-keys
func (a *Admin) APIKeysList(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, name, COALESCE(remark, ''), token_prefix, created_at, last_used_at
		FROM admin_api_key
		WHERE revoked_at IS NULL
		ORDER BY id DESC
	`)
	if err != nil {
		a.log.Error("api key list failed", "category", "auth", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var (
			id       int64
			name     string
			remark   string
			prefix   string
			created  time.Time
			lastUsed sql.NullTime
		)
		if err := rows.Scan(&id, &name, &remark, &prefix, &created, &lastUsed); err != nil {
			continue
		}
		m := map[string]any{
			"id":           id,
			"name":         name,
			"remark":       remark,
			"token_prefix": prefix,
			"created_at":   created,
		}
		if lastUsed.Valid {
			m["last_used_at"] = lastUsed.Time
		}
		out = append(out, m)
	}
	WriteJSON(w, http.StatusOK, out)
}

// APIKeyCreate creates a per-tool API key. The clear token is returned once.
// POST /admin/api-keys
func (a *Admin) APIKeyCreate(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body apiKeyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteText(w, http.StatusBadRequest, "invalid body")
		return
	}
	defer r.Body.Close()
	body.Name = strings.TrimSpace(body.Name)
	body.Remark = strings.TrimSpace(body.Remark)
	if body.Name == "" {
		WriteText(w, http.StatusBadRequest, "name required")
		return
	}
	token := "emo_" + auth.RandomToken(32)
	prefix := token
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	res, err := a.db.ExecContext(r.Context(), `
		INSERT INTO admin_api_key (name, remark, token_hash, token_prefix)
		VALUES (?, ?, ?, ?)
	`, body.Name, nullableString(body.Remark), hashToken(token), prefix)
	if err != nil {
		a.log.Error("api key create failed", "category", "auth", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	a.log.Info("admin api key created", "category", "auth", "id", id, "name", body.Name)
	WriteJSON(w, http.StatusCreated, map[string]any{
		"id":           id,
		"name":         body.Name,
		"remark":       body.Remark,
		"token_prefix": prefix,
		"token":        token,
	})
}

// APIKeyRevoke revokes a generated API key.
// DELETE /admin/api-keys/{id}
func (a *Admin) APIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	if _, err := a.db.ExecContext(r.Context(),
		"UPDATE admin_api_key SET revoked_at = NOW() WHERE id = ? AND revoked_at IS NULL", id); err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	a.log.Info("admin api key revoked", "category", "auth", "id", id)
	WriteStatus(w, http.StatusNoContent)
}

type tmdbSettings struct {
	APIKey         string `json:"api_key,omitempty"`
	Configured     bool   `json:"configured"`
	Language       string `json:"language"`
	AutoScrape     bool   `json:"auto_scrape"`
	TVDBConfigured bool   `json:"tvdb_configured"`
	OMDBConfigured bool   `json:"omdb_configured"`
}

type tmdbSettingsRequest struct {
	APIKey       string `json:"api_key"`
	Language     string `json:"language"`
	AutoScrape   bool   `json:"auto_scrape"`
	ClearKey     bool   `json:"clear_key"`
	TVDBAPIKey   string `json:"tvdb_api_key"`
	TVDBPIN      string `json:"tvdb_pin"`
	ClearTVDBKey bool   `json:"clear_tvdb_key"`
	OMDBAPIKey   string `json:"omdb_api_key"`
	ClearOMDBKey bool   `json:"clear_omdb_key"`
}

// TMDBSettingsGet returns the editable metadata settings, masking the token.
// GET /admin/tmdb/settings
func (a *Admin) TMDBSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	s := a.loadTMDBSettings(r.Context())
	WriteJSON(w, http.StatusOK, s)
}

// TMDBSettingsUpdate stores TMDB settings from the dashboard.
// POST /admin/tmdb/settings
func (a *Admin) TMDBSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body tmdbSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteText(w, http.StatusBadRequest, "invalid body")
		return
	}
	defer r.Body.Close()
	lang := strings.TrimSpace(body.Language)
	if lang == "" {
		lang = "zh-CN"
	}
	current := a.rawSetting(r.Context(), "tmdb_api_key", a.cfg.TMDBAPIKey)
	apiKey := strings.TrimSpace(body.APIKey)
	if body.ClearKey {
		apiKey = ""
	} else if apiKey == "" {
		apiKey = current
	}
	tvdbKey := strings.TrimSpace(body.TVDBAPIKey)
	if body.ClearTVDBKey {
		tvdbKey = ""
	} else if tvdbKey == "" {
		tvdbKey = a.rawSetting(r.Context(), "tvdb_api_key", a.cfg.TVDBAPIKey)
	}
	tvdbPIN := strings.TrimSpace(body.TVDBPIN)
	if tvdbPIN == "" {
		tvdbPIN = a.rawSetting(r.Context(), "tvdb_pin", a.cfg.TVDBPIN)
	}
	omdbKey := strings.TrimSpace(body.OMDBAPIKey)
	if body.ClearOMDBKey {
		omdbKey = ""
	} else if omdbKey == "" {
		omdbKey = a.rawSetting(r.Context(), "omdb_api_key", a.cfg.OMDBAPIKey)
	}
	if err := a.saveSetting(r.Context(), "tmdb_api_key", apiKey); err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	if err := a.saveSetting(r.Context(), "tmdb_language", lang); err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	if err := a.saveSetting(r.Context(), "tmdb_auto_scrape", strconv.FormatBool(body.AutoScrape)); err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	if err := a.saveSetting(r.Context(), "tvdb_api_key", tvdbKey); err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	if err := a.saveSetting(r.Context(), "tvdb_pin", tvdbPIN); err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	if err := a.saveSetting(r.Context(), "omdb_api_key", omdbKey); err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	a.cfg.TVDBAPIKey = tvdbKey
	a.cfg.TVDBPIN = tvdbPIN
	a.cfg.OMDBAPIKey = omdbKey
	a.rebuildTMDB(apiKey, lang)
	a.cfg.TMDBAutoScrape = body.AutoScrape
	a.log.Info("metadata settings updated", "category", "tmdb", "language", lang, "auto_scrape", body.AutoScrape, "tmdb_configured", apiKey != "", "tvdb_configured", tvdbKey != "", "omdb_configured", omdbKey != "")
	WriteJSON(w, http.StatusOK, a.loadTMDBSettings(r.Context()))
}

// TMDBSettingsTest checks whether TMDB responds with the current or submitted config.
// POST /admin/tmdb/settings/test
func (a *Admin) TMDBSettingsTest(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body tmdbSettingsRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()
	lang := strings.TrimSpace(body.Language)
	if lang == "" {
		lang = a.rawSetting(r.Context(), "tmdb_language", a.cfg.TMDBLanguage)
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey == "" {
		apiKey = a.rawSetting(r.Context(), "tmdb_api_key", a.cfg.TMDBAPIKey)
	}
	client := tmdb.NewClient(apiKey, tmdb.WithLanguage(lang))
	defer client.Close()
	if !client.Enabled() {
		WriteText(w, http.StatusBadRequest, "TMDB API key required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	results, err := client.SearchMovie(ctx, "Inception", 2010)
	if err != nil {
		a.log.Warn("tmdb test failed", "category", "tmdb", "err", err)
		WriteText(w, http.StatusBadGateway, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"language": lang,
		"results":  len(results),
	})
}

func (a *Admin) loadTMDBSettings(ctx context.Context) tmdbSettings {
	key := a.rawSetting(ctx, "tmdb_api_key", a.cfg.TMDBAPIKey)
	tvdbKey := a.rawSetting(ctx, "tvdb_api_key", a.cfg.TVDBAPIKey)
	omdbKey := a.rawSetting(ctx, "omdb_api_key", a.cfg.OMDBAPIKey)
	lang := a.rawSetting(ctx, "tmdb_language", a.cfg.TMDBLanguage)
	if strings.TrimSpace(lang) == "" {
		lang = "zh-CN"
	}
	autoRaw := a.rawSetting(ctx, "tmdb_auto_scrape", strconv.FormatBool(a.cfg.TMDBAutoScrape))
	auto, _ := strconv.ParseBool(autoRaw)
	return tmdbSettings{
		Configured:     strings.TrimSpace(key) != "",
		Language:       lang,
		AutoScrape:     auto,
		TVDBConfigured: strings.TrimSpace(tvdbKey) != "",
		OMDBConfigured: strings.TrimSpace(omdbKey) != "",
	}
}

func (a *Admin) rawSetting(ctx context.Context, key, fallback string) string {
	var v string
	err := a.db.QueryRowContext(ctx, "SELECT value FROM app_setting WHERE key = ? LIMIT 1", key).Scan(&v)
	if err != nil {
		return fallback
	}
	return v
}

func (a *Admin) saveSetting(ctx context.Context, key, value string) error {
	_, err := a.db.DB.ExecContext(ctx, db.Rebind(`
		INSERT INTO app_setting (key, value, updated_at)
		VALUES (?, ?, NOW())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()
	`), key, value)
	return err
}

func (a *Admin) rebuildTMDB(apiKey, language string) {
	if a.tmdb != nil {
		a.tmdb.Close()
	}
	opts := a.scraperOptions()
	if strings.TrimSpace(apiKey) == "" {
		a.tmdb = nil
		if len(opts) == 0 {
			a.scraper = nil
			return
		}
		a.scraper = tmdb.NewScraper(nil, a.db, a.log, opts...)
		return
	}
	a.tmdb = tmdb.NewClient(apiKey, tmdb.WithLanguage(language))
	a.scraper = tmdb.NewScraper(a.tmdb, a.db, a.log, opts...)
}

func (a *Admin) scraperOptions() []tmdb.ScraperOption {
	var opts []tmdb.ScraperOption
	if strings.TrimSpace(a.cfg.TVDBAPIKey) != "" {
		opts = append(opts, tmdb.WithTVDBClient(metadata.NewTVDBClient(a.cfg.TVDBAPIKey, a.cfg.TVDBPIN)))
	}
	if strings.TrimSpace(a.cfg.OMDBAPIKey) != "" {
		opts = append(opts, tmdb.WithOMDBClient(metadata.NewOMDBClient(a.cfg.OMDBAPIKey)))
	}
	return opts
}

// LibrariesList returns every library (admin view).
// GET /admin/libraries
func (a *Admin) LibrariesList(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		WITH active_lists AS (
			SELECT id, video_library_id, video_type
			FROM video_list
			WHERE deleted_at IS NULL
		),
		list_counts AS (
			SELECT
				video_library_id,
				COUNT(*) AS item_count,
				COUNT(*) FILTER (WHERE video_type = 'movie') AS movie_count,
				COUNT(*) FILTER (WHERE video_type = 'tv') AS series_count
			FROM active_lists
			GROUP BY video_library_id
		),
		episode_counts AS (
			SELECT vl.video_library_id, COUNT(ve.id) AS episode_count
			FROM active_lists vl
			JOIN video_episode ve
				ON ve.video_list_id = vl.id AND ve.deleted_at IS NULL
			GROUP BY vl.video_library_id
		),
		media_counts AS (
			SELECT vl.video_library_id, COUNT(vm.id) AS media_count
			FROM active_lists vl
			JOIN video_media vm
				ON vm.video_list_id = vl.id AND vm.deleted_at IS NULL
			GROUP BY vl.video_library_id
		),
		library_images AS (
			SELECT
				relation_id,
				COALESCE(MAX(CASE WHEN type = 'Primary' THEN path_type END), '') AS poster_type,
				COALESCE(MAX(CASE WHEN type = 'Primary' THEN path_url END), '') AS poster_url,
				COALESCE(MAX(CASE WHEN type = 'Backdrop' THEN path_type END), '') AS backdrop_type,
				COALESCE(MAX(CASE WHEN type = 'Backdrop' THEN path_url END), '') AS backdrop_url
			FROM video_image
			WHERE relation_type = ? AND type IN (?, ?) AND deleted_at IS NULL
			GROUP BY relation_id
		)
		SELECT
			l.id,
			l.name,
			COALESCE(l.role, ''),
			COALESCE(l.root_path, ''),
			l.watch_enabled,
			l.watch_interval_seconds,
			l.is_hidden,
			l.created_at,
			COALESCE(lc.item_count, 0) AS item_count,
			COALESCE(lc.movie_count, 0) AS movie_count,
			COALESCE(lc.series_count, 0) AS series_count,
			COALESCE(ec.episode_count, 0) AS episode_count,
			COALESCE(mc.media_count, 0) AS media_count,
			COALESCE(li.poster_type, '') AS poster_type,
			COALESCE(li.poster_url, '') AS poster_url,
			COALESCE(li.backdrop_type, '') AS backdrop_type,
			COALESCE(li.backdrop_url, '') AS backdrop_url
		FROM library l
		LEFT JOIN list_counts lc
			ON lc.video_library_id = l.id
		LEFT JOIN episode_counts ec
			ON ec.video_library_id = l.id
		LEFT JOIN media_counts mc
			ON mc.video_library_id = l.id
		LEFT JOIN library_images li
			ON li.relation_id = l.id
		WHERE l.deleted_at IS NULL
		ORDER BY l.id ASC`, emby.ItemIDTypeVideoLibrary, db.ImageTypePrimary, db.ImageTypeBackdrop)
	if err != nil {
		a.log.Error("library list failed", "category", "admin", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var (
			id                   int64
			name                 string
			role                 string
			rootPath             string
			watchEnabled         bool
			watchIntervalSeconds int
			isHidden             bool
			createdAt            sql.NullTime
			itemCount            int64
			movieCount           int64
			seriesCount          int64
			episodeCount         int64
			mediaCount           int64
			posterType           string
			posterPath           string
			backdropType         string
			backdropPath         string
		)
		if err := rows.Scan(
			&id, &name, &role, &rootPath, &watchEnabled, &watchIntervalSeconds, &isHidden, &createdAt,
			&itemCount, &movieCount, &seriesCount, &episodeCount, &mediaCount,
			&posterType, &posterPath, &backdropType, &backdropPath,
		); err != nil {
			continue
		}
		itemID := emby.ItemID(emby.ItemIDTypeVideoLibrary, id)
		m := map[string]any{
			"id":                     id,
			"item_id":                itemID,
			"name":                   name,
			"role":                   role,
			"root_path":              rootPath,
			"watch_enabled":          watchEnabled,
			"watch_interval_seconds": watchIntervalSeconds,
			"is_hidden":              isHidden,
			"item_count":             itemCount,
			"movie_count":            movieCount,
			"series_count":           seriesCount,
			"episode_count":          episodeCount,
			"media_count":            mediaCount,
			"poster_url":             adminImageURL(posterType, posterPath, itemID, db.ImageTypePrimary),
			"backdrop_url":           adminImageURL(backdropType, backdropPath, itemID, db.ImageTypeBackdrop),
		}
		if watcher := a.watchByLibrary(id); watcher != nil {
			m["watcher"] = a.publicWatchJob(watcher)
		}
		if createdAt.Valid {
			m["created_at"] = createdAt.Time
		}
		out = append(out, m)
	}
	WriteJSON(w, http.StatusOK, out)
}

// libraryCreateBody is the POST /admin/libraries body.
type libraryCreateBody struct {
	Name                 string  `json:"name"`
	Role                 string  `json:"role"`
	RootPath             string  `json:"root_path"`
	WatchEnabled         bool    `json:"watch_enabled"`
	WatchIntervalSeconds int     `json:"watch_interval_seconds"`
	IsHidden             bool    `json:"is_hidden"`
	PosterURL            *string `json:"poster_url"`
	BackdropURL          *string `json:"backdrop_url"`
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
	role := nullableString(strings.TrimSpace(body.Role))
	root := nullableString(strings.TrimSpace(body.RootPath))
	interval := normalizeWatchInterval(body.WatchIntervalSeconds)
	res, err := a.db.ExecContext(r.Context(),
		"INSERT INTO library (name, role, root_path, watch_enabled, watch_interval_seconds, is_hidden) VALUES (?, ?, ?, ?, ?, ?)",
		body.Name, role, root, body.WatchEnabled, interval, body.IsHidden)
	if err != nil {
		a.log.Error("library create failed", "category", "admin", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	if body.PosterURL != nil {
		if err := a.replaceLibraryImage(r.Context(), id, db.ImageTypePrimary, strings.TrimSpace(*body.PosterURL)); err != nil {
			a.log.Error("library poster update failed", "category", "admin", "err", err)
			WriteStatus(w, http.StatusInternalServerError)
			return
		}
	}
	if body.BackdropURL != nil {
		if err := a.replaceLibraryImage(r.Context(), id, db.ImageTypeBackdrop, strings.TrimSpace(*body.BackdropURL)); err != nil {
			a.log.Error("library backdrop update failed", "category", "admin", "err", err)
			WriteStatus(w, http.StatusInternalServerError)
			return
		}
	}
	if body.WatchEnabled && root.Valid {
		a.startLibraryWatcher(context.Background(), id, root.String, body.Role, interval)
	}
	WriteJSON(w, http.StatusCreated, map[string]any{
		"id":                     id,
		"name":                   body.Name,
		"role":                   body.Role,
		"root_path":              body.RootPath,
		"watch_enabled":          body.WatchEnabled,
		"watch_interval_seconds": interval,
		"is_hidden":              body.IsHidden,
		"poster_url":             valueOrEmpty(body.PosterURL),
		"backdrop_url":           valueOrEmpty(body.BackdropURL),
	})
}

// LibraryUpdate updates library metadata and watcher settings.
// PATCH /admin/libraries/{id}
func (a *Admin) LibraryUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	var body libraryCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteText(w, http.StatusBadRequest, "invalid body")
		return
	}
	defer r.Body.Close()
	body.Name = strings.TrimSpace(body.Name)
	body.Role = strings.TrimSpace(body.Role)
	body.RootPath = strings.TrimSpace(body.RootPath)
	if body.Name == "" {
		WriteText(w, http.StatusBadRequest, "name required")
		return
	}
	if body.WatchEnabled {
		if body.RootPath == "" {
			WriteText(w, http.StatusBadRequest, "root_path required when watch is enabled")
			return
		}
		if info, err := os.Stat(body.RootPath); err != nil || !info.IsDir() {
			WriteText(w, http.StatusBadRequest, "root_path does not exist: "+body.RootPath)
			return
		}
	}
	interval := normalizeWatchInterval(body.WatchIntervalSeconds)
	_, err = a.db.ExecContext(r.Context(), `
		UPDATE library
		SET name = ?, role = ?, root_path = ?, watch_enabled = ?, watch_interval_seconds = ?, is_hidden = ?, updated_at = NOW()
		WHERE id = ? AND deleted_at IS NULL
	`, body.Name, nullableString(body.Role), nullableString(body.RootPath), body.WatchEnabled, interval, body.IsHidden, id)
	if err != nil {
		a.log.Error("library update failed", "category", "admin", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	if body.PosterURL != nil {
		if err := a.replaceLibraryImage(r.Context(), id, db.ImageTypePrimary, strings.TrimSpace(*body.PosterURL)); err != nil {
			a.log.Error("library poster update failed", "category", "admin", "err", err)
			WriteStatus(w, http.StatusInternalServerError)
			return
		}
	}
	if body.BackdropURL != nil {
		if err := a.replaceLibraryImage(r.Context(), id, db.ImageTypeBackdrop, strings.TrimSpace(*body.BackdropURL)); err != nil {
			a.log.Error("library backdrop update failed", "category", "admin", "err", err)
			WriteStatus(w, http.StatusInternalServerError)
			return
		}
	}
	if body.WatchEnabled {
		a.startLibraryWatcher(context.Background(), id, body.RootPath, body.Role, interval)
	} else {
		a.stopWatchByLibrary(id)
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"id":                     id,
		"name":                   body.Name,
		"role":                   body.Role,
		"root_path":              body.RootPath,
		"watch_enabled":          body.WatchEnabled,
		"watch_interval_seconds": interval,
		"is_hidden":              body.IsHidden,
		"poster_url":             valueOrEmpty(body.PosterURL),
		"backdrop_url":           valueOrEmpty(body.BackdropURL),
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
	a.stopWatchByLibrary(id)
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
	DateAir       string `json:"date_air,omitempty"`
	Runtime       int64  `json:"runtime,omitempty"`
	TMDBID        string `json:"tmdb_id,omitempty"`
	IMDBID        string `json:"imdb_id,omitempty"`
	TVDBID        string `json:"tvdb_id,omitempty"`
	Overview      string `json:"overview,omitempty"`
	Tagline       string `json:"tagline,omitempty"`
	PosterURL     string `json:"poster_url,omitempty"`
	BackdropURL   string `json:"backdrop_url,omitempty"`
	MediaCount    int64  `json:"media_count"`
	SeasonCount   int64  `json:"season_count"`
	EpisodeCount  int64  `json:"episode_count"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	MissingPoster bool   `json:"missing_poster"`
	MissingInfo   bool   `json:"missing_info"`
}

// AdminMediaListResponse is returned by GET /admin/media.
type AdminMediaListResponse struct {
	Items []AdminMediaItem `json:"items"`
	Total int64            `json:"total"`
}

type AdminMediaStats struct {
	Total         int64 `json:"total"`
	Scraped       int64 `json:"scraped"`
	Unscraped     int64 `json:"unscraped"`
	MissingPoster int64 `json:"missing_poster"`
	MissingInfo   int64 `json:"missing_info"`
	Complete      int64 `json:"complete"`
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
// GET /admin/media?library_id=1&type=movie|tv&search=...&missing=poster|info|any&limit=60&offset=0
func (a *Admin) AdminMediaList(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	q := r.URL.Query()
	limit := parsePositiveInt(q.Get("limit"), 50)
	if limit > 100 {
		limit = 100
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
	missingPoster := `NOT EXISTS (
		SELECT 1 FROM video_image vi
		WHERE vi.relation_type = 'vl' AND vi.relation_id = vl.id
		  AND vi.type = 'Primary' AND vi.deleted_at IS NULL
	)`
	missingInfo := "(vl.description IS NULL OR vl.description = '' OR vl.date_air IS NULL)"
	metadataMissing := "(COALESCE(vl.tmdb_id, '') = '' AND COALESCE(vl.imdb_id, '') = '' AND COALESCE(vl.tvdb_id, '') = '' AND COALESCE(vl.description, '') = '' AND vl.date_air IS NULL)"
	switch strings.ToLower(strings.TrimSpace(q.Get("missing"))) {
	case "poster":
		where = append(where, missingPoster)
	case "info":
		where = append(where, missingInfo)
	case "unscraped":
		where = append(where, metadataMissing)
	case "any":
		where = append(where, "("+missingInfo+" OR "+missingPoster+" OR "+metadataMissing+")")
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
	queryArgs = append(queryArgs, limit, offset,
		emby.ItemIDTypeVideoList, db.ImageTypePrimary, db.ImageTypeBackdrop)
	rows, err := a.db.QueryContext(r.Context(), `
		WITH page AS (
			SELECT vl.id
			FROM video_list vl
			WHERE `+whereSQL+`
			ORDER BY vl.updated_at DESC, vl.id DESC
			LIMIT ? OFFSET ?
		),
		media_counts AS (
			SELECT vm.video_list_id, COUNT(*) AS media_count
			FROM video_media vm
			WHERE vm.deleted_at IS NULL
			  AND vm.video_list_id IN (SELECT id FROM page)
			GROUP BY vm.video_list_id
		),
		season_counts AS (
			SELECT vs.video_list_id, COUNT(*) AS season_count
			FROM video_season vs
			WHERE vs.deleted_at IS NULL
			  AND vs.video_list_id IN (SELECT id FROM page)
			GROUP BY vs.video_list_id
		),
		episode_counts AS (
			SELECT ve.video_list_id, COUNT(*) AS episode_count
			FROM video_episode ve
			WHERE ve.deleted_at IS NULL
			  AND ve.video_list_id IN (SELECT id FROM page)
			GROUP BY ve.video_list_id
		),
		images AS (
			SELECT
				vi.relation_id,
				COALESCE(MAX(CASE WHEN vi.type = 'Primary' THEN vi.path_type END), '') AS poster_type,
				COALESCE(MAX(CASE WHEN vi.type = 'Primary' THEN vi.path_url END), '') AS poster_url,
				COALESCE(MAX(CASE WHEN vi.type = 'Backdrop' THEN vi.path_type END), '') AS backdrop_type,
				COALESCE(MAX(CASE WHEN vi.type = 'Backdrop' THEN vi.path_url END), '') AS backdrop_url
			FROM video_image vi
			WHERE vi.relation_type = ?
			  AND vi.deleted_at IS NULL
			  AND vi.type IN (?, ?)
			  AND vi.relation_id IN (SELECT id FROM page)
			GROUP BY vi.relation_id
		)
		SELECT
			vl.id,
			vl.video_library_id,
			vl.video_type,
			vl.title,
			COALESCE(vl.origin_title, ''),
			COALESCE(vl.tmdb_id, ''),
			COALESCE(vl.imdb_id, ''),
			COALESCE(vl.tvdb_id, ''),
			COALESCE(vl.description, ''),
			COALESCE(vl.tagline, ''),
			vl.date_air,
			COALESCE(vl.runtime, 0),
			vl.updated_at,
			COALESCE(mc.media_count, 0) AS media_count,
			COALESCE(sc.season_count, 0) AS season_count,
			COALESCE(ec.episode_count, 0) AS episode_count,
			COALESCE(img.poster_type, '') AS poster_type,
			COALESCE(img.poster_url, '') AS poster_url,
			COALESCE(img.backdrop_type, '') AS backdrop_type,
			COALESCE(img.backdrop_url, '') AS backdrop_url
		FROM page p
		JOIN video_list vl ON vl.id = p.id
		LEFT JOIN media_counts mc ON mc.video_list_id = vl.id
		LEFT JOIN season_counts sc ON sc.video_list_id = vl.id
		LEFT JOIN episode_counts ec ON ec.video_list_id = vl.id
		LEFT JOIN images img ON img.relation_id = vl.id
		ORDER BY vl.updated_at DESC, vl.id DESC`,
		queryArgs...,
	)
	if err != nil {
		a.log.Error("admin media list failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	items := []AdminMediaItem{}
	for rows.Next() {
		item, err := scanAdminMediaItem(rows)
		if err != nil {
			a.log.Warn("admin media row skipped", "err", err)
			continue
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		a.log.Error("admin media rows failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	WriteJSON(w, http.StatusOK, AdminMediaListResponse{Items: items, Total: total})
}

// AdminMediaStats returns scrape coverage for the current media filter.
// GET /admin/media/stats?library_id=1&type=movie|tv
func (a *Admin) AdminMediaStats(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	where := []string{"vl.deleted_at IS NULL"}
	args := []any{}
	if libID := parsePositiveInt64(q.Get("library_id"), 0); libID > 0 {
		where = append(where, "vl.video_library_id = ?")
		args = append(args, libID)
	}
	if typ := strings.TrimSpace(q.Get("type")); typ == db.VideoTypeMovie || typ == db.VideoTypeTV {
		where = append(where, "vl.video_type = ?")
		args = append(args, typ)
	}
	whereSQL := strings.Join(where, " AND ")
	providerMissing := "(COALESCE(vl.tmdb_id, '') = '' AND COALESCE(vl.imdb_id, '') = '' AND COALESCE(vl.tvdb_id, '') = '')"
	providerPresent := "(COALESCE(vl.tmdb_id, '') <> '' OR COALESCE(vl.imdb_id, '') <> '' OR COALESCE(vl.tvdb_id, '') <> '')"
	metadataPresent := "(" + providerPresent + " OR COALESCE(vl.description, '') <> '' OR vl.date_air IS NOT NULL)"
	metadataMissing := "(" + providerMissing + " AND COALESCE(vl.description, '') = '' AND vl.date_air IS NULL)"
	missingInfo := "(vl.description IS NULL OR vl.description = '' OR vl.date_air IS NULL)"
	row := a.db.QueryRowContext(r.Context(), `
		WITH primary_images AS (
			SELECT vi.relation_id
			FROM video_image vi
			WHERE vi.relation_type = ?
			  AND vi.type = ?
			  AND vi.deleted_at IS NULL
			GROUP BY vi.relation_id
		)
		SELECT
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE `+metadataPresent+`) AS scraped,
			COUNT(*) FILTER (WHERE `+metadataMissing+`) AS unscraped,
			COUNT(*) FILTER (WHERE pi.relation_id IS NULL) AS missing_poster,
			COUNT(*) FILTER (WHERE `+missingInfo+`) AS missing_info,
			COUNT(*) FILTER (WHERE `+metadataPresent+`
				AND COALESCE(vl.description, '') <> ''
				AND vl.date_air IS NOT NULL
				AND pi.relation_id IS NOT NULL
			) AS complete
		FROM video_list vl
		LEFT JOIN primary_images pi ON pi.relation_id = vl.id
		WHERE `+whereSQL, append([]any{emby.ItemIDTypeVideoList, db.ImageTypePrimary}, args...)...)
	var out AdminMediaStats
	if err := row.Scan(&out.Total, &out.Scraped, &out.Unscraped, &out.MissingPoster, &out.MissingInfo, &out.Complete); err != nil {
		a.log.Error("admin media stats failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, out)
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
	ID          int64  `json:"id"`
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	PathType    string `json:"path_type,omitempty"`
	PathURL     string `json:"path_url,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Duration    int64  `json:"duration,omitempty"`
	Bitrate     int64  `json:"bitrate,omitempty"`
	Container   string `json:"container,omitempty"`
	VideoCodec  string `json:"video_codec,omitempty"`
	AudioCodec  string `json:"audio_codec,omitempty"`
	Width       int64  `json:"width,omitempty"`
	Height      int64  `json:"height,omitempty"`
	StreamCount int    `json:"stream_count,omitempty"`
}

type adminMediaUpdateRequest struct {
	Title         *string `json:"title"`
	OriginalTitle *string `json:"original_title"`
	TMDBID        *string `json:"tmdb_id"`
	IMDBID        *string `json:"imdb_id"`
	TVDBID        *string `json:"tvdb_id"`
	Overview      *string `json:"overview"`
	Tagline       *string `json:"tagline"`
	DateAir       *string `json:"date_air"`
	Runtime       *int    `json:"runtime"`
	PosterURL     *string `json:"poster_url"`
	BackdropURL   *string `json:"backdrop_url"`
}

// AdminMediaUpdate updates editable metadata for one video_list.
// PATCH /admin/media/{id}
func (a *Admin) AdminMediaUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	var body adminMediaUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteText(w, http.StatusBadRequest, "invalid body")
		return
	}
	defer r.Body.Close()
	var exists int64
	if err := a.db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM video_list WHERE id = ? AND deleted_at IS NULL", id,
	).Scan(&exists); err != nil || exists == 0 {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	updates := []string{}
	args := []any{}
	if body.Title != nil {
		title := strings.TrimSpace(*body.Title)
		if title == "" {
			WriteText(w, http.StatusBadRequest, "title required")
			return
		}
		updates = append(updates, "title = ?")
		args = append(args, title)
	}
	if body.OriginalTitle != nil {
		updates = append(updates, "origin_title = ?")
		args = append(args, nullableString(*body.OriginalTitle))
	}
	if body.TMDBID != nil {
		updates = append(updates, "tmdb_id = ?")
		args = append(args, nullableString(*body.TMDBID))
	}
	if body.IMDBID != nil {
		updates = append(updates, "imdb_id = ?")
		args = append(args, nullableString(*body.IMDBID))
	}
	if body.TVDBID != nil {
		updates = append(updates, "tvdb_id = ?")
		args = append(args, nullableString(*body.TVDBID))
	}
	if body.Overview != nil {
		updates = append(updates, "description = ?")
		args = append(args, nullableString(*body.Overview))
	}
	if body.Tagline != nil {
		updates = append(updates, "tagline = ?")
		args = append(args, nullableString(*body.Tagline))
	}
	if body.DateAir != nil {
		air, err := parseOptionalDate(*body.DateAir)
		if err != nil {
			WriteText(w, http.StatusBadRequest, "date_air must be YYYY-MM-DD")
			return
		}
		updates = append(updates, "date_air = ?")
		args = append(args, air)
	}
	if body.Runtime != nil {
		updates = append(updates, "runtime = ?")
		args = append(args, nullableAdminInt(*body.Runtime))
	}
	if len(updates) > 0 {
		updates = append(updates, "updated_at = NOW()")
		stmt := "UPDATE video_list SET " + strings.Join(updates, ", ") + " WHERE id = ? AND deleted_at IS NULL"
		args = append(args, id)
		res, err := a.db.ExecContext(r.Context(), stmt, args...)
		if err != nil {
			a.log.Error("admin media update failed", "err", err)
			WriteText(w, http.StatusInternalServerError, err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			WriteStatus(w, http.StatusNotFound)
			return
		}
	}
	if body.PosterURL != nil {
		if err := a.replaceAdminImage(r.Context(), id, db.ImageTypePrimary, strings.TrimSpace(*body.PosterURL)); err != nil {
			WriteText(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.BackdropURL != nil {
		if err := a.replaceAdminImage(r.Context(), id, db.ImageTypeBackdrop, strings.TrimSpace(*body.BackdropURL)); err != nil {
			WriteText(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	item, err := a.adminMediaOne(r.Context(), id)
	if err != nil {
		WriteText(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, item)
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
	episodeTotal, err := a.adminEpisodeTotal(r.Context(), id)
	if err != nil {
		a.log.Error("admin episode count failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	episodes, err := a.adminEpisodes(r, id, 60)
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
		"type":          videoType,
		"seasons":       seasons,
		"episodes":      episodes,
		"episode_total": episodeTotal,
		"sources":       sources,
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

func (a *Admin) adminEpisodeTotal(ctx context.Context, videoListID int64) (int64, error) {
	var total int64
	err := a.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM video_episode WHERE video_list_id = ? AND deleted_at IS NULL",
		videoListID,
	).Scan(&total)
	return total, err
}

func (a *Admin) adminEpisodes(r *http.Request, videoListID int64, limit int) ([]adminMediaEpisode, error) {
	if limit <= 0 {
		limit = 60
	}
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
		ORDER BY COALESCE(vs.season_number, 0), ve.episode_number, ve.id
		LIMIT ?`, videoListID, limit)
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
		SELECT id, uuid, name, COALESCE(path_url, ''), COALESCE(file_size, 0),
		       COALESCE(file_second, 0), COALESCE(file_container, ''),
		       COALESCE(file_matadata::text, ''), COALESCE(path_type, '')
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
		var metadata string
		if err := rows.Scan(&s.ID, &s.UUID, &s.Name, &s.PathURL, &s.Size, &s.Duration, &s.Container, &metadata, &s.PathType); err != nil {
			return nil, err
		}
		s.applyProbeSummary(metadata)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *adminMediaSource) applyProbeSummary(metadata string) {
	if strings.TrimSpace(metadata) == "" {
		return
	}
	var meta struct {
		Streams []map[string]any `json:"streams"`
		Format  struct {
			BitRate any `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		return
	}
	s.StreamCount = len(meta.Streams)
	if s.Bitrate == 0 {
		s.Bitrate = int64FromAny(meta.Format.BitRate)
	}
	for _, stream := range meta.Streams {
		codecType, _ := stream["codec_type"].(string)
		codecName, _ := stream["codec_name"].(string)
		switch codecType {
		case "video":
			if s.VideoCodec == "" {
				s.VideoCodec = codecName
				s.Width = int64FromAny(stream["width"])
				s.Height = int64FromAny(stream["height"])
			}
		case "audio":
			if s.AudioCodec == "" {
				s.AudioCodec = codecName
			}
		}
	}
}

func int64FromAny(v any) int64 {
	switch x := v.(type) {
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
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

func (a *Admin) adminMediaOne(ctx context.Context, id int64) (*AdminMediaItem, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT
			vl.id,
			vl.video_library_id,
			vl.video_type,
			vl.title,
			COALESCE(vl.origin_title, ''),
			COALESCE(vl.tmdb_id, ''),
			COALESCE(vl.imdb_id, ''),
			COALESCE(vl.tvdb_id, ''),
			COALESCE(vl.description, ''),
			COALESCE(vl.tagline, ''),
			vl.date_air,
			COALESCE(vl.runtime, 0),
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
		WHERE vl.id = ? AND vl.deleted_at IS NULL
		GROUP BY vl.id`,
		emby.ItemIDTypeVideoList, db.ImageTypePrimary,
		emby.ItemIDTypeVideoList, db.ImageTypeBackdrop,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	item, err := scanAdminMediaItem(rows)
	if err != nil {
		return nil, err
	}
	return &item, rows.Err()
}

func scanAdminMediaItem(rows interface {
	Scan(dest ...any) error
}) (AdminMediaItem, error) {
	var (
		item         AdminMediaItem
		originTitle  string
		tmdbID       string
		imdbID       string
		tvdbID       string
		overview     string
		tagline      string
		dateAir      sql.NullTime
		runtime      int64
		updatedAt    sql.NullTime
		posterType   string
		posterPath   string
		backdropType string
		backdropPath string
	)
	if err := rows.Scan(
		&item.ID, &item.LibraryID, &item.Type, &item.Title,
		&originTitle, &tmdbID, &imdbID, &tvdbID, &overview, &tagline, &dateAir, &runtime, &updatedAt,
		&item.MediaCount, &item.SeasonCount, &item.EpisodeCount,
		&posterType, &posterPath, &backdropType, &backdropPath,
	); err != nil {
		return item, err
	}
	item.ItemID = emby.ItemID(emby.ItemIDTypeVideoList, item.ID)
	item.OriginalTitle = originTitle
	item.TMDBID = tmdbID
	item.IMDBID = imdbID
	item.TVDBID = tvdbID
	item.Overview = overview
	item.Tagline = tagline
	item.Runtime = runtime
	if dateAir.Valid {
		item.Year = dateAir.Time.Year()
		item.DateAir = dateAir.Time.Format("2006-01-02")
	}
	if updatedAt.Valid {
		item.UpdatedAt = updatedAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	item.PosterURL = adminImageURL(posterType, posterPath, item.ItemID, db.ImageTypePrimary)
	item.BackdropURL = adminImageURL(backdropType, backdropPath, item.ItemID, db.ImageTypeBackdrop)
	item.MissingPoster = item.PosterURL == ""
	item.MissingInfo = item.Overview == "" || item.DateAir == ""
	return item, nil
}

func (a *Admin) replaceAdminImage(ctx context.Context, listID int64, imageType, rawURL string) error {
	return a.replaceImage(ctx, emby.ItemIDTypeVideoList, listID, imageType, rawURL)
}

func (a *Admin) replaceLibraryImage(ctx context.Context, libraryID int64, imageType, rawURL string) error {
	return a.replaceImage(ctx, emby.ItemIDTypeVideoLibrary, libraryID, imageType, rawURL)
}

func (a *Admin) replaceImage(ctx context.Context, relationType string, relationID int64, imageType, rawURL string) error {
	if rawURL == "" {
		_, err := a.db.ExecContext(ctx, `
			UPDATE video_image SET deleted_at = NOW(), updated_at = NOW()
			WHERE relation_type = ? AND relation_id = ? AND type = ? AND deleted_at IS NULL
		`, relationType, relationID, imageType)
		return err
	}
	pathType, pathURL := adminImagePath(rawURL)
	var existing int64
	_ = a.db.QueryRowContext(ctx, `
		SELECT id FROM video_image
		WHERE relation_type = ? AND relation_id = ? AND type = ? AND deleted_at IS NULL
		LIMIT 1
	`, relationType, relationID, imageType).Scan(&existing)
	if existing > 0 {
		_, err := a.db.ExecContext(ctx, `
			UPDATE video_image SET path_type = ?, path_url = ?, updated_at = NOW()
			WHERE id = ?
		`, pathType, pathURL, existing)
		return err
	}
	_, err := a.db.ExecContext(ctx, `
		INSERT INTO video_image (type, relation_type, relation_id, path_type, path_url)
		VALUES (?, ?, ?, ?, ?)
	`, imageType, relationType, relationID, pathType, pathURL)
	return err
}

func adminImagePath(rawURL string) (pathType, pathURL string) {
	rawURL = strings.TrimSpace(rawURL)
	if strings.HasPrefix(rawURL, "https://image.tmdb.org/t/p/") {
		parts := strings.SplitN(strings.TrimPrefix(rawURL, "https://image.tmdb.org/t/p/"), "/", 2)
		if len(parts) == 2 && strings.HasPrefix(parts[1], "/") {
			return db.ImagePathTypeTMDB, parts[1]
		}
		if len(parts) == 2 {
			return db.ImagePathTypeTMDB, "/" + parts[1]
		}
	}
	switch {
	case strings.HasPrefix(rawURL, "/") && !strings.Contains(rawURL, "://"):
		if strings.Count(rawURL, "/") == 1 {
			return db.ImagePathTypeTMDB, rawURL
		}
		return db.PathTypeLocal, rawURL
	case strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://"):
		if strings.Contains(rawURL, "douban") {
			return db.ImagePathTypeDouban, rawURL
		}
		return db.ImagePathTypeURL, rawURL
	default:
		return db.PathTypeLocal, rawURL
	}
}

func parseOptionalDate(raw string) (sql.NullTime, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return sql.NullTime{}, nil
	}
	t, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return sql.NullTime{}, err
	}
	return sql.NullTime{Valid: true, Time: t}, nil
}

func nullableAdminInt(v int) sql.NullInt64 {
	if v <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Valid: true, Int64: int64(v)}
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

func nullableString(s string) sql.NullString {
	s = strings.TrimSpace(s)
	return sql.NullString{String: s, Valid: s != ""}
}

func valueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

func normalizeWatchInterval(v int) int {
	if v <= 0 {
		return 30
	}
	if v < 5 {
		return 5
	}
	if v > 3600 {
		return 3600
	}
	return v
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// scanRequest is the POST body for /admin/library/scan.
type scanRequest struct {
	LibraryID      int64  `json:"library_id"`
	Root           string `json:"root"`
	DefaultType    string `json:"default_type"` // movie | tv | (empty = auto)
	FollowSymlinks bool   `json:"follow_symlinks"`
	DryRun         bool   `json:"dry_run"`
	ProbeMedia     bool   `json:"probe_media"`
	// Scrape controls TMDB backfill on items touched by this import.
	// Accepts: "" (use server default TMDB_AUTO_SCRAPE), "on", "off", "force".
	Scrape string `json:"scrape"`
}

type scanAllRequest struct {
	FollowSymlinks bool   `json:"follow_symlinks"`
	DryRun         bool   `json:"dry_run"`
	ProbeMedia     bool   `json:"probe_media"`
	Scrape         string `json:"scrape"`
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

type embyRefreshRequest struct {
	Recursive           bool     `json:"Recursive"`
	ImageRefreshMode    string   `json:"ImageRefreshMode"`
	MetadataRefreshMode string   `json:"MetadataRefreshMode"`
	ReplaceAllImages    bool     `json:"ReplaceAllImages"`
	ReplaceAllMetadata  bool     `json:"ReplaceAllMetadata"`
	Paths               []string `json:"Paths"`
	Updates             []struct {
		Path       string `json:"Path"`
		UpdateType string `json:"UpdateType"`
	} `json:"Updates"`
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

	if err := a.hydrateScanRequest(r.Context(), &body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			WriteText(w, http.StatusNotFound, "library not found")
			return
		}
		WriteText(w, http.StatusBadRequest, err.Error())
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

	if err := a.hydrateScanRequest(r.Context(), &body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			WriteText(w, http.StatusNotFound, "library not found")
			return
		}
		WriteText(w, http.StatusBadRequest, err.Error())
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

// LibraryScanAllStart starts async scan jobs for every library with root_path.
// POST /admin/library/scan-all/start
func (a *Admin) LibraryScanAllStart(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body scanAllRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
		defer r.Body.Close()
	}

	reqs, err := a.scanRequestsForAllLibraries(r.Context(), body)
	if err != nil {
		a.log.Error("scan all libraries query failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	started := []any{}
	errorsOut := []string{}
	for _, req := range reqs {
		job, err := a.startScanJob(req)
		if err != nil {
			errorsOut = append(errorsOut, fmt.Sprintf("library_id=%d root=%s: %v", req.LibraryID, req.Root, err))
			continue
		}
		started = append(started, job)
	}
	if len(started) == 0 {
		WriteJSON(w, http.StatusBadRequest, map[string]any{
			"message": "no scannable libraries with a valid root_path",
			"errors":  errorsOut,
		})
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"items":              started,
		"Items":              started,
		"total":              len(started),
		"TotalRecordCount":   len(started),
		"errors":             errorsOut,
		"failed_start_count": len(errorsOut),
	})
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
		ProbeMedia:     body.ProbeMedia,
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
			scrapeResults := a.scrapeVideoLists(ctx, report.TouchedVideoListIDs, mode == "force", func(p tmdb.BatchResult) {
				if progress == nil {
					return
				}
				current := ""
				if len(p.ActiveItems) > 0 {
					current = "正在刮削 " + scrapeResultLabel(p.ActiveItems[0])
				} else if len(p.Items) > 0 {
					last := p.Items[len(p.Items)-1]
					current = scrapeResultLabel(last)
					if last.Failed {
						current += ": " + last.Reason
					} else if last.Skipped {
						current += ": " + last.Reason
					} else if last.MatchedTitle != "" {
						current += " -> " + last.MatchedTitle
					}
				}
				progress(importer.Progress{
					Stage:      "scraping",
					Current:    current,
					WalkedDirs: report.Scanned,
					Processed:  p.Processed,
					Total:      p.Total,
					Report:     *report,
					Details: map[string]any{
						"tmdb": copyTMDBBatchResult(p),
					},
				})
			})
			return map[string]any{
				"import": report,
				"tmdb":   scrapeResults,
			}, nil
		}
	}

	return report, nil
}

func (a *Admin) scanRequestsForAllLibraries(ctx context.Context, body scanAllRequest) ([]scanRequest, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT id, COALESCE(root_path, ''), COALESCE(role, '')
		FROM library
		WHERE deleted_at IS NULL AND COALESCE(root_path, '') <> ''
		ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []scanRequest{}
	for rows.Next() {
		var req scanRequest
		if err := rows.Scan(&req.LibraryID, &req.Root, &req.DefaultType); err != nil {
			return nil, err
		}
		req.FollowSymlinks = body.FollowSymlinks
		req.DryRun = body.DryRun
		req.ProbeMedia = body.ProbeMedia
		req.Scrape = body.Scrape
		out = append(out, req)
	}
	return out, rows.Err()
}

func (a *Admin) scrapeVideoLists(ctx context.Context, ids []int64, force bool, progress func(tmdb.BatchResult)) []*tmdb.ScrapeResult {
	items := a.scrapeItemsForIDs(ctx, ids)
	if len(items) == 0 {
		return nil
	}
	const workers = 8
	jobs := make(chan adminScrapeItem)
	events := make(chan adminScrapeEvent)
	rep := tmdb.BatchResult{Total: len(items)}
	if progress != nil {
		progress(rep)
	}
	var wg sync.WaitGroup
	workerCount := workers
	if len(items) < workerCount {
		workerCount = len(items)
	}
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				started := item
				events <- adminScrapeEvent{Started: &started}
				res, err := a.scraper.ScrapeVideoList(ctx, item.id, force)
				if err != nil {
					a.log.Warn("tmdb scrape failed", "video_list_id", item.id, "err", err)
					events <- adminScrapeEvent{Result: &tmdb.ScrapeResult{VideoListID: item.id, VideoType: item.videoType, Title: item.title, Failed: true, Reason: err.Error()}}
					continue
				}
				if res == nil {
					events <- adminScrapeEvent{Result: &tmdb.ScrapeResult{VideoListID: item.id, VideoType: item.videoType, Title: item.title, Skipped: true, Reason: "empty result"}}
					continue
				}
				events <- adminScrapeEvent{Result: res}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, item := range items {
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
	out := make([]*tmdb.ScrapeResult, 0, len(items))
	active := map[int64]tmdb.ScrapeResult{}
	for event := range events {
		if event.Started != nil {
			active[event.Started.id] = tmdb.ScrapeResult{VideoListID: event.Started.id, VideoType: event.Started.videoType, Title: event.Started.title}
			rep.ActiveItems = adminActiveScrapeItems(active)
			rep.Active = len(rep.ActiveItems)
			if progress != nil {
				progress(rep)
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
			rep.Errors = append(rep.Errors, fmt.Sprintf("id=%d: %s", res.VideoListID, res.Reason))
		} else if res.Skipped {
			rep.Skipped++
		} else {
			rep.Matched++
		}
		rep.Items = append(rep.Items, res)
		rep.ActiveItems = adminActiveScrapeItems(active)
		rep.Active = len(rep.ActiveItems)
		copyRes := res
		out = append(out, &copyRes)
		if progress != nil {
			progress(rep)
		}
	}
	return out
}

type adminScrapeItem struct {
	id        int64
	videoType string
	title     string
}

type adminScrapeEvent struct {
	Started *adminScrapeItem
	Result  *tmdb.ScrapeResult
}

func (a *Admin) scrapeItemsForIDs(ctx context.Context, ids []int64) []adminScrapeItem {
	ids = uniquePositiveIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := a.db.QueryContext(ctx, `
		SELECT id, video_type, title
		FROM video_list
		WHERE deleted_at IS NULL
		  AND id IN (`+placeholdersForIDs(ids)+`)`,
		args...,
	)
	if err != nil {
		out := make([]adminScrapeItem, 0, len(ids))
		for _, id := range ids {
			out = append(out, adminScrapeItem{id: id})
		}
		return out
	}
	defer rows.Close()
	found := map[int64]adminScrapeItem{}
	for rows.Next() {
		var item adminScrapeItem
		if err := rows.Scan(&item.id, &item.videoType, &item.title); err == nil {
			found[item.id] = item
		}
	}
	out := make([]adminScrapeItem, 0, len(ids))
	for _, id := range ids {
		if item, ok := found[id]; ok {
			out = append(out, item)
			continue
		}
		out = append(out, adminScrapeItem{id: id})
	}
	return out
}

func adminActiveScrapeItems(active map[int64]tmdb.ScrapeResult) []tmdb.ScrapeResult {
	if len(active) == 0 {
		return nil
	}
	out := make([]tmdb.ScrapeResult, 0, len(active))
	for _, item := range active {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VideoListID < out[j].VideoListID })
	return out
}

func scrapeResultLabel(item tmdb.ScrapeResult) string {
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = fmt.Sprintf("#%d", item.VideoListID)
	}
	if item.VideoType == db.VideoTypeTV {
		return "剧集 " + title
	}
	if item.VideoType == db.VideoTypeMovie {
		return "电影 " + title
	}
	return title
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

// EmbyLibraryRefresh accepts Emby/Jellyfin-style refresh requests used by
// automation tools. It maps them to Emotion's existing async scan jobs.
func (a *Admin) EmbyLibraryRefresh(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body embyRefreshRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	started := []any{}
	for _, req := range a.refreshRequestsFromBody(r, body) {
		job, err := a.startScanJob(req)
		if err != nil {
			a.log.Warn("emby refresh start failed", "err", err)
			continue
		}
		started = append(started, job)
	}
	if len(started) == 0 {
		WriteStatus(w, http.StatusNoContent)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"Items": started, "TotalRecordCount": len(started)})
}

// EmbyItemRefresh refreshes one library or item by Emby item id.
func (a *Admin) EmbyItemRefresh(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	itemID := chi.URLParam(r, "itemId")
	if itemID == "" {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	libraryID, root, role, ok := libraryRootForItem(r.Context(), a.db, itemID)
	if !ok || libraryID <= 0 {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	force := strings.EqualFold(r.URL.Query().Get("ReplaceAllMetadata"), "true")
	if kind, numericID, parsed := emby.ParseItemID(itemID); parsed && kind == emby.ItemIDTypeVideoList {
		if a.scraper != nil && a.scraper.Enabled() {
			res, err := a.scraper.ScrapeVideoList(r.Context(), numericID, force)
			if err == nil {
				WriteJSON(w, http.StatusOK, res)
				return
			}
			a.log.Warn("emby item tmdb refresh failed", "item_id", itemID, "err", err)
		}
	}
	if strings.TrimSpace(root) == "" {
		WriteStatus(w, http.StatusNoContent)
		return
	}
	job, err := a.startScanJob(scanRequest{
		LibraryID:   libraryID,
		Root:        root,
		DefaultType: role,
		Scrape:      "on",
	})
	if err != nil {
		WriteText(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusAccepted, job)
}

func (a *Admin) refreshRequestsFromBody(r *http.Request, body embyRefreshRequest) []scanRequest {
	var paths []string
	paths = append(paths, body.Paths...)
	for _, u := range body.Updates {
		if strings.TrimSpace(u.Path) != "" {
			paths = append(paths, u.Path)
		}
	}
	for _, key := range []string{"path", "Path"} {
		if v := strings.TrimSpace(r.URL.Query().Get(key)); v != "" {
			paths = append(paths, v)
		}
	}
	for _, key := range []string{"ItemId", "itemId", "ParentId"} {
		if v := strings.TrimSpace(r.URL.Query().Get(key)); v != "" {
			if libID, root, role, ok := libraryRootForItem(r.Context(), a.db, v); ok && strings.TrimSpace(root) != "" {
				return []scanRequest{{LibraryID: libID, Root: root, DefaultType: role, Scrape: "on"}}
			}
		}
	}

	type libRow struct {
		id   int64
		root string
		role string
	}
	libraries := []libRow{}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, COALESCE(root_path, ''), COALESCE(role, '')
		FROM library
		WHERE deleted_at IS NULL AND COALESCE(root_path, '') <> ''
		ORDER BY id ASC`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var l libRow
			if err := rows.Scan(&l.id, &l.root, &l.role); err == nil {
				libraries = append(libraries, l)
			}
		}
	}

	out := []scanRequest{}
	if len(paths) == 0 {
		for _, l := range libraries {
			out = append(out, scanRequest{LibraryID: l.id, Root: l.root, DefaultType: l.role, Scrape: "on"})
		}
		return out
	}
	seen := map[string]bool{}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for _, l := range libraries {
			if pathWithinRoot(p, l.root) {
				key := strconv.FormatInt(l.id, 10) + "|" + p
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, scanRequest{LibraryID: l.id, Root: p, DefaultType: l.role, Scrape: "on"})
				break
			}
		}
	}
	if len(out) == 0 {
		for _, l := range libraries {
			out = append(out, scanRequest{LibraryID: l.id, Root: l.root, DefaultType: l.role, Scrape: "on"})
		}
	}
	return out
}

func (a *Admin) startScanJob(body scanRequest) (*scanJob, error) {
	if err := a.hydrateScanRequest(context.Background(), &body); err != nil {
		return nil, err
	}
	if _, err := os.Stat(body.Root); err != nil {
		return nil, err
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
			a.updateScanJob(id, func(j *scanJob) { j.Progress = p })
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
	return job, nil
}

func pathWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if strings.EqualFold(path, root) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != "" && !strings.HasPrefix(rel, "..")
}

type watchRequest struct {
	scanRequest
	IntervalSeconds int `json:"interval_seconds"`
}

type watchJob struct {
	ID              string    `json:"id"`
	LibraryID       int64     `json:"library_id"`
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

	if _, err := a.db.ExecContext(r.Context(), `
		UPDATE library
		SET root_path = ?, watch_enabled = TRUE, watch_interval_seconds = ?, updated_at = NOW()
		WHERE id = ? AND deleted_at IS NULL
	`, body.Root, interval, body.LibraryID); err != nil {
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	job := a.startLibraryWatcher(context.Background(), body.LibraryID, body.Root, body.DefaultType, interval)
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
	if job.LibraryID > 0 {
		_, _ = a.db.ExecContext(r.Context(),
			"UPDATE library SET watch_enabled = FALSE, updated_at = NOW() WHERE id = ?", job.LibraryID)
	}
	WriteStatus(w, http.StatusNoContent)
}

func (a *Admin) startConfiguredWatchers() {
	time.Sleep(500 * time.Millisecond)
	rows, err := a.db.QueryContext(context.Background(), `
		SELECT id, COALESCE(root_path, ''), COALESCE(role, ''), watch_interval_seconds
		FROM library
		WHERE deleted_at IS NULL AND watch_enabled = TRUE AND COALESCE(root_path, '') <> ''
	`)
	if err != nil {
		a.log.Warn("load configured watchers failed", "category", "watch", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var root, role string
		var interval int
		if err := rows.Scan(&id, &root, &role, &interval); err != nil {
			continue
		}
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			a.startLibraryWatcher(context.Background(), id, root, role, normalizeWatchInterval(interval))
		}
	}
}

func (a *Admin) startLibraryWatcher(ctx context.Context, libraryID int64, root, defaultType string, interval int) *watchJob {
	interval = normalizeWatchInterval(interval)
	a.stopWatchByLibrary(libraryID)
	id := randomScanID()
	watchCtx, cancel := context.WithCancel(ctx)
	job := &watchJob{
		ID:              id,
		LibraryID:       libraryID,
		Status:          "running",
		StartedAt:       time.Now(),
		IntervalSeconds: interval,
		Root:            root,
		cancel:          cancel,
	}
	a.watchMu.Lock()
	a.watchJobs[id] = job
	a.watchMu.Unlock()
	a.log.Info("library watcher started", "category", "watch", "library_id", libraryID, "root", root, "interval", interval)
	go a.runWatchJob(watchCtx, id, scanRequest{
		LibraryID:   libraryID,
		Root:        root,
		DefaultType: defaultType,
	}, time.Duration(interval)*time.Second)
	return job
}

func (a *Admin) stopWatchByLibrary(libraryID int64) {
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	for id, job := range a.watchJobs {
		if job != nil && job.LibraryID == libraryID {
			job.Status = "stopped"
			if job.cancel != nil {
				job.cancel()
			}
			delete(a.watchJobs, id)
		}
	}
}

func (a *Admin) watchByLibrary(libraryID int64) *watchJob {
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	for _, job := range a.watchJobs {
		if job != nil && job.LibraryID == libraryID && job.Status == "running" {
			cp := *job
			return &cp
		}
	}
	return nil
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
		"library_id":       job.LibraryID,
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
		WriteText(w, http.StatusServiceUnavailable, "metadata scraper disabled: set TMDB_API_KEY, TVDB_API_KEY, or OMDB_API_KEY")
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
	Max       int    `json:"max"`
	Force     bool   `json:"force"`
	LibraryID int64  `json:"library_id"`
	VideoType string `json:"video_type"`
	Missing   string `json:"missing"`
}

type tmdbRefreshJob struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at,omitempty"`
	Progress   tmdb.BatchResult  `json:"progress"`
	Result     *tmdb.BatchResult `json:"result,omitempty"`
	Error      string            `json:"error,omitempty"`
}

type mediaProbeRequest struct {
	LibraryID int64 `json:"library_id"`
	Max       int   `json:"max"`
	Force     bool  `json:"force"`
	Workers   int   `json:"workers"`
}

type mediaProbeProgress struct {
	Total     int      `json:"total"`
	Processed int      `json:"processed"`
	Active    int      `json:"active"`
	Updated   int      `json:"updated"`
	Skipped   int      `json:"skipped"`
	Failed    int      `json:"failed"`
	Current   string   `json:"current,omitempty"`
	Errors    []string `json:"errors,omitempty"`
}

type mediaProbeJob struct {
	ID         string             `json:"id"`
	Status     string             `json:"status"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt time.Time          `json:"finished_at,omitempty"`
	Progress   mediaProbeProgress `json:"progress"`
	Error      string             `json:"error,omitempty"`
}

type mediaProbeItem struct {
	ID   int64
	Path string
}

// MediaProbeStart starts an async ffprobe job for imported local media.
func (a *Admin) MediaProbeStart(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var body mediaProbeRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	id := randomScanID()
	job := &mediaProbeJob{ID: id, Status: "running", StartedAt: time.Now()}
	a.probeMu.Lock()
	a.probeJobs[id] = job
	a.probeMu.Unlock()

	go func() {
		progress, err := a.runMediaProbeJob(context.Background(), body, func(p mediaProbeProgress) {
			a.updateMediaProbeJob(id, func(j *mediaProbeJob) {
				j.Progress = p
			})
		})
		a.updateMediaProbeJob(id, func(j *mediaProbeJob) {
			j.FinishedAt = time.Now()
			j.Progress = progress
			if err != nil {
				j.Status = "failed"
				j.Error = err.Error()
				return
			}
			j.Status = "done"
		})
	}()

	WriteJSON(w, http.StatusAccepted, job)
}

// MediaProbeStatus returns one media probe job.
func (a *Admin) MediaProbeStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "id")
	a.probeMu.Lock()
	job := a.cloneMediaProbeJob(a.probeJobs[id])
	a.probeMu.Unlock()
	if job == nil {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	WriteJSON(w, http.StatusOK, job)
}

func (a *Admin) runMediaProbeJob(ctx context.Context, body mediaProbeRequest, progressFn func(mediaProbeProgress)) (mediaProbeProgress, error) {
	items, err := a.mediaProbeItems(ctx, body)
	if err != nil {
		return mediaProbeProgress{}, err
	}
	p := mediaProbeProgress{Total: len(items)}
	if progressFn != nil {
		progressFn(p)
	}
	if len(items) == 0 {
		return p, nil
	}

	workers := body.Workers
	if workers <= 0 {
		workers = 8
	}
	if workers > 32 {
		workers = 32
	}
	if workers > len(items) {
		workers = len(items)
	}

	jobs := make(chan mediaProbeItem)
	results := make(chan struct {
		item mediaProbeItem
		err  error
	})
	var wg sync.WaitGroup
	var progressMu sync.Mutex
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				progressMu.Lock()
				p.Active++
				p.Current = item.Path
				if progressFn != nil {
					progressFn(p)
				}
				progressMu.Unlock()
				err := a.probeOneMedia(ctx, item)
				results <- struct {
					item mediaProbeItem
					err  error
				}{item: item, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, item := range items {
			if ctx.Err() != nil {
				return
			}
			jobs <- item
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	for res := range results {
		progressMu.Lock()
		if p.Active > 0 {
			p.Active--
		}
		p.Processed++
		p.Current = res.item.Path
		if res.err != nil {
			p.Failed++
			if len(p.Errors) < 20 {
				p.Errors = append(p.Errors, fmt.Sprintf("%s: %v", res.item.Path, res.err))
			}
		} else {
			p.Updated++
		}
		if progressFn != nil {
			progressFn(p)
		}
		progressMu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		return p, err
	}
	return p, nil
}

func (a *Admin) mediaProbeItems(ctx context.Context, body mediaProbeRequest) ([]mediaProbeItem, error) {
	where := []string{"vm.deleted_at IS NULL", "COALESCE(vm.path_url, '') <> ''"}
	args := []any{}
	if body.LibraryID > 0 {
		where = append(where, "vl.video_library_id = ?")
		args = append(args, body.LibraryID)
	}
	if !body.Force {
		where = append(where, "(vm.file_matadata IS NULL OR vm.file_second IS NULL OR vm.file_second = 0 OR COALESCE(vm.file_container, '') = '')")
	}
	limitSQL := ""
	if body.Max > 0 {
		limitSQL = " LIMIT ?"
		args = append(args, body.Max)
	}
	rows, err := a.db.QueryContext(ctx, `
		SELECT vm.id, vm.path_url
		FROM video_media vm
		JOIN video_list vl ON vl.id = vm.video_list_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY vm.id ASC`+limitSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []mediaProbeItem{}
	for rows.Next() {
		var item mediaProbeItem
		if err := rows.Scan(&item.ID, &item.Path); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *Admin) probeOneMedia(ctx context.Context, item mediaProbeItem) error {
	probe, err := importer.ProbeLocalMedia(ctx, item.Path)
	if err != nil {
		return err
	}
	var fileSize sql.NullInt64
	if info, err := os.Stat(item.Path); err == nil && info.Size() > 0 {
		fileSize = sql.NullInt64{Valid: true, Int64: info.Size()}
	} else if probe.Size > 0 {
		fileSize = sql.NullInt64{Valid: true, Int64: probe.Size}
	}
	_, err = a.db.ExecContext(ctx, `
		UPDATE video_media
		SET file_size = COALESCE(?, file_size),
		    file_second = ?, file_matadata = ?, file_container = ?, updated_at = NOW()
		WHERE id = ?
	`, fileSize, sql.NullInt64{Valid: probe.Duration > 0, Int64: probe.Duration},
		sql.NullString{Valid: len(probe.Metadata) > 0, String: string(probe.Metadata)},
		nullableString(probe.Container), item.ID)
	return err
}

func (a *Admin) updateMediaProbeJob(id string, fn func(*mediaProbeJob)) {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if job := a.probeJobs[id]; job != nil {
		fn(job)
	}
}

func (a *Admin) cloneMediaProbeJob(job *mediaProbeJob) *mediaProbeJob {
	if job == nil {
		return nil
	}
	cp := *job
	cp.Progress.Errors = append([]string(nil), job.Progress.Errors...)
	return &cp
}

// TMDBRefreshAll scans all items missing metadata and refreshes each.
//
// POST /admin/tmdb/refresh-all  {"max": 200, "force": false}
func (a *Admin) TMDBRefreshAll(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.scraper == nil || !a.scraper.Enabled() {
		WriteText(w, http.StatusServiceUnavailable, "metadata scraper disabled: set TMDB_API_KEY, TVDB_API_KEY, or OMDB_API_KEY")
		return
	}
	var body tmdbRefreshAllRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	rep, err := a.scraper.ScrapeMissing(r.Context(), tmdb.ScrapeMissingOptions{
		MaxItems:  body.Max,
		Force:     body.Force,
		LibraryID: body.LibraryID,
		VideoType: body.VideoType,
		Missing:   body.Missing,
	})
	if err != nil {
		a.log.Error("tmdb refresh-all failed", "err", err)
		WriteText(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, rep)
}

// TMDBRefreshAllStart starts an asynchronous bulk TMDB refresh job.
// POST /admin/tmdb/refresh-all/start
func (a *Admin) TMDBRefreshAllStart(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.scraper == nil || !a.scraper.Enabled() {
		WriteText(w, http.StatusServiceUnavailable, "metadata scraper disabled: set TMDB_API_KEY, TVDB_API_KEY, or OMDB_API_KEY")
		return
	}
	var body tmdbRefreshAllRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	id := randomScanID()
	job := &tmdbRefreshJob{
		ID:        id,
		Status:    "running",
		StartedAt: time.Now(),
	}
	a.tmdbMu.Lock()
	a.tmdbJobs[id] = job
	a.tmdbMu.Unlock()

	go func() {
		rep, err := a.scraper.ScrapeMissing(context.Background(), tmdb.ScrapeMissingOptions{
			MaxItems:  body.Max,
			Force:     body.Force,
			LibraryID: body.LibraryID,
			VideoType: body.VideoType,
			Missing:   body.Missing,
			Progress: func(p tmdb.BatchResult) {
				p.Duration = time.Since(job.StartedAt).Milliseconds()
				a.updateTMDBJob(id, func(j *tmdbRefreshJob) {
					j.Progress = copyTMDBBatchResult(p)
				})
			},
		})
		a.updateTMDBJob(id, func(j *tmdbRefreshJob) {
			j.FinishedAt = time.Now()
			if err != nil {
				j.Status = "failed"
				j.Error = err.Error()
				return
			}
			j.Status = "done"
			if rep != nil {
				cp := copyTMDBBatchResult(*rep)
				j.Progress = cp
				j.Result = &cp
			}
		})
	}()

	WriteJSON(w, http.StatusAccepted, job)
}

// TMDBRefreshAllStatus returns the latest status for an async TMDB job.
// GET /admin/tmdb/refresh-all/{id}
func (a *Admin) TMDBRefreshAllStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "id")
	a.tmdbMu.Lock()
	job := a.cloneTMDBJob(a.tmdbJobs[id])
	a.tmdbMu.Unlock()
	if job == nil {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	WriteJSON(w, http.StatusOK, job)
}

func (a *Admin) updateTMDBJob(id string, fn func(*tmdbRefreshJob)) {
	a.tmdbMu.Lock()
	defer a.tmdbMu.Unlock()
	if job := a.tmdbJobs[id]; job != nil {
		fn(job)
	}
}

func (a *Admin) cloneTMDBJob(job *tmdbRefreshJob) *tmdbRefreshJob {
	if job == nil {
		return nil
	}
	out := *job
	out.Progress = copyTMDBBatchResult(job.Progress)
	if job.Result != nil {
		cp := copyTMDBBatchResult(*job.Result)
		out.Result = &cp
	}
	return &out
}

func copyTMDBBatchResult(in tmdb.BatchResult) tmdb.BatchResult {
	out := in
	out.Errors = append([]string(nil), in.Errors...)
	out.ActiveItems = append([]tmdb.ScrapeResult(nil), in.ActiveItems...)
	out.Items = append([]tmdb.ScrapeResult(nil), in.Items...)
	return out
}
