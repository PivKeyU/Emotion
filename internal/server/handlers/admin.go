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

	"github.com/PivKeyU/Next-Emby/internal/config"
	"github.com/PivKeyU/Next-Emby/internal/importer"
	"github.com/PivKeyU/Next-Emby/internal/server/ctxpkg"
	"github.com/PivKeyU/Next-Emby/internal/tmdb"
)

// Admin serves /admin/* endpoints for manual library management.
// All endpoints require the admin API key or an admin user.
type Admin struct {
	db       *sql.DB
	cfg      *config.Config
	log      *slog.Logger
	importer *importer.Importer
	tmdb     *tmdb.Client // may be nil
	scraper  *tmdb.Scraper
}

// NewAdmin constructs the handler.
func NewAdmin(database *sql.DB, cfg *config.Config, log *slog.Logger) *Admin {
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
