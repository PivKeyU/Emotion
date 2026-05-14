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
	a := &Admin{
		db:        database,
		cfg:       cfg,
		log:       log,
		importer:  importer.New(database, log),
		tmdb:      client,
		scraper:   scraper,
		scanJobs:  map[string]*scanJob{},
		watchJobs: map[string]*watchJob{},
	}
	settings := a.loadTMDBSettings(context.Background())
	a.cfg.TMDBAutoScrape = settings.AutoScrape
	if key := a.rawSetting(context.Background(), "tmdb_api_key", cfg.TMDBAPIKey); strings.TrimSpace(key) != "" {
		a.rebuildTMDB(key, settings.Language)
	}
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
	APIKey     string `json:"api_key,omitempty"`
	Configured bool   `json:"configured"`
	Language   string `json:"language"`
	AutoScrape bool   `json:"auto_scrape"`
}

type tmdbSettingsRequest struct {
	APIKey     string `json:"api_key"`
	Language   string `json:"language"`
	AutoScrape bool   `json:"auto_scrape"`
	ClearKey   bool   `json:"clear_key"`
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
	a.rebuildTMDB(apiKey, lang)
	a.cfg.TMDBAutoScrape = body.AutoScrape
	a.log.Info("tmdb settings updated", "category", "tmdb", "language", lang, "auto_scrape", body.AutoScrape, "configured", apiKey != "")
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
	lang := a.rawSetting(ctx, "tmdb_language", a.cfg.TMDBLanguage)
	if strings.TrimSpace(lang) == "" {
		lang = "zh-CN"
	}
	autoRaw := a.rawSetting(ctx, "tmdb_auto_scrape", strconv.FormatBool(a.cfg.TMDBAutoScrape))
	auto, _ := strconv.ParseBool(autoRaw)
	return tmdbSettings{
		Configured: strings.TrimSpace(key) != "",
		Language:   lang,
		AutoScrape: auto,
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
	if strings.TrimSpace(apiKey) == "" {
		a.tmdb = nil
		a.scraper = nil
		return
	}
	a.tmdb = tmdb.NewClient(apiKey, tmdb.WithLanguage(language))
	a.scraper = tmdb.NewScraper(a.tmdb, a.db, a.log)
}

// LibrariesList returns every library (admin view).
// GET /admin/libraries
func (a *Admin) LibrariesList(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT
			l.id,
			l.name,
			COALESCE(l.role, ''),
			COALESCE(l.root_path, ''),
			l.watch_enabled,
			l.watch_interval_seconds,
			l.created_at,
			COUNT(DISTINCT vl.id) AS item_count,
			COUNT(DISTINCT CASE WHEN vl.video_type = 'movie' THEN vl.id END) AS movie_count,
			COUNT(DISTINCT CASE WHEN vl.video_type = 'tv' THEN vl.id END) AS series_count,
			COUNT(DISTINCT ve.id) AS episode_count,
			COUNT(DISTINCT vm.id) AS media_count
		FROM library l
		LEFT JOIN video_list vl
			ON vl.video_library_id = l.id AND vl.deleted_at IS NULL
		LEFT JOIN video_episode ve
			ON ve.video_list_id = vl.id AND ve.deleted_at IS NULL
		LEFT JOIN video_media vm
			ON vm.video_list_id = vl.id AND vm.deleted_at IS NULL
		WHERE l.deleted_at IS NULL
		GROUP BY l.id, l.name, l.role, l.root_path, l.watch_enabled, l.watch_interval_seconds, l.created_at
		ORDER BY l.id ASC`)
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
			createdAt            sql.NullTime
			itemCount            int64
			movieCount           int64
			seriesCount          int64
			episodeCount         int64
			mediaCount           int64
		)
		if err := rows.Scan(
			&id, &name, &role, &rootPath, &watchEnabled, &watchIntervalSeconds, &createdAt,
			&itemCount, &movieCount, &seriesCount, &episodeCount, &mediaCount,
		); err != nil {
			continue
		}
		m := map[string]any{
			"id":                     id,
			"name":                   name,
			"role":                   role,
			"root_path":              rootPath,
			"watch_enabled":          watchEnabled,
			"watch_interval_seconds": watchIntervalSeconds,
			"item_count":             itemCount,
			"movie_count":            movieCount,
			"series_count":           seriesCount,
			"episode_count":          episodeCount,
			"media_count":            mediaCount,
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
	Name                 string `json:"name"`
	Role                 string `json:"role"`
	RootPath             string `json:"root_path"`
	WatchEnabled         bool   `json:"watch_enabled"`
	WatchIntervalSeconds int    `json:"watch_interval_seconds"`
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
		"INSERT INTO library (name, role, root_path, watch_enabled, watch_interval_seconds) VALUES (?, ?, ?, ?, ?)",
		body.Name, role, root, body.WatchEnabled, interval)
	if err != nil {
		a.log.Error("library create failed", "category", "admin", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
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
		SET name = ?, role = ?, root_path = ?, watch_enabled = ?, watch_interval_seconds = ?, updated_at = NOW()
		WHERE id = ? AND deleted_at IS NULL
	`, body.Name, nullableString(body.Role), nullableString(body.RootPath), body.WatchEnabled, interval, id)
	if err != nil {
		a.log.Error("library update failed", "category", "admin", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
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

func nullableString(s string) sql.NullString {
	s = strings.TrimSpace(s)
	return sql.NullString{String: s, Valid: s != ""}
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
