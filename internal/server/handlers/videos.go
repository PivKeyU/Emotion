package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Next-Emby/internal/cache"
	"github.com/PivKeyU/Next-Emby/internal/config"
	"github.com/PivKeyU/Next-Emby/internal/db"
	"github.com/PivKeyU/Next-Emby/internal/emby"
	"github.com/PivKeyU/Next-Emby/internal/external"
	"github.com/PivKeyU/Next-Emby/internal/server/ctxpkg"
)

// Videos serves /Videos/* endpoints: direct-play redirects and subtitle resolution.
// No transcoding; clients must do direct play.
type Videos struct {
	db    *sql.DB
	cache cache.Cache
	cfg   *config.Config
	log   *slog.Logger
	ext   *external.Client
}

// NewVideos builds the handler.
func NewVideos(database *sql.DB, c cache.Cache, cfg *config.Config, log *slog.Logger) *Videos {
	return &Videos{
		db:    database,
		cache: c,
		cfg:   cfg,
		log:   log,
		ext:   external.NewClient(cfg.APIExternal, cfg.APIKey),
	}
}

// Play is the "resolve to a direct-play URL" endpoint. Emby clients use this to
// turn a DirectStreamUrl into a real HTTP source. We 302-redirect to whatever
// URL is stored on the video_media row.
//
// The URL path looks like /Videos/{mediaUUID}/{mediaName} where mediaName can be
// "original.strm", "original.mkv", etc. The name is ignored; only mediaUUID matters.
// mediaUUID may also be an emby item-id (e.g. vl-42), in which case we pick the
// first associated media row.
func (v *Videos) Play(w http.ResponseWriter, r *http.Request) {
	mediaKey := chi.URLParam(r, "mediaUUID")
	line := r.URL.Query().Get("line")
	userID := ctxpkg.UserID(r.Context())

	cacheKey := fmt.Sprintf("video_play_%s_%d_%s", mediaKey, userID, line)
	if cached, ok := v.cache.Get(r.Context(), cacheKey); ok {
		http.Redirect(w, r, cached, http.StatusPermanentRedirect)
		return
	}

	ctx := r.Context()

	// Resolve to an actual UUID.
	uuid := mediaKey
	if kind, numericID, ok := emby.ParseItemID(mediaKey); ok {
		var col string
		switch kind {
		case emby.ItemIDTypeVideoList:
			col = "video_list_id"
		case emby.ItemIDTypeVideoEpisode:
			col = "video_episode_id"
		default:
			v.log.Error("video play: unsupported item type", "kind", kind)
			WriteStatus(w, http.StatusUnprocessableEntity)
			return
		}
		query := fmt.Sprintf("SELECT uuid FROM video_media WHERE %s = ? AND deleted_at IS NULL LIMIT 1", col)
		var found db.NullString
		if err := v.db.QueryRowContext(ctx, query, numericID).Scan(&found); err != nil || !found.Valid {
			WriteStatus(w, http.StatusForbidden)
			return
		}
		uuid = found.String
	}

	var (
		mediaID    int64
		pathType   db.NullString
		pathURL    db.NullString
	)
	err := v.db.QueryRowContext(ctx, `
		SELECT id, path_type, path_url FROM video_media
		WHERE uuid = ? AND status = ? AND deleted_at IS NULL LIMIT 1
	`, uuid, db.MediaStatusComplete).Scan(&mediaID, &pathType, &pathURL)
	if err != nil {
		WriteStatus(w, http.StatusForbidden)
		return
	}

	// Bump view counter.
	_, _ = v.db.ExecContext(ctx,
		"UPDATE video_media SET number_view = number_view + 1 WHERE id = ?", mediaID)

	cacheTTL := 3 * time.Hour
	playURL := ""

	switch pathType.String {
	case db.PathTypeURL:
		playURL = pathURL.String
	default:
		if v.ext.Enabled() {
			resp, err := v.ext.Post(ctx, "/emby/videoGetUrl", map[string]any{
				"media_id":  mediaID,
				"user_id":   userID,
				"path_type": pathType.String,
				"path_url":  pathURL.String,
				"uuid":      uuid,
				"line":      line,
			})
			if err != nil {
				v.log.Error("external video url failed", "err", err)
			} else if resp.Code == 200 {
				var data struct {
					URL          string `json:"url"`
					CacheSeconds int64  `json:"cache_seconds"`
				}
				_ = json.Unmarshal(resp.Data, &data)
				playURL = data.URL
				if data.CacheSeconds > 0 {
					cacheTTL = time.Duration(data.CacheSeconds) * time.Second
				}
			}
		}
	}

	if playURL == "" {
		v.log.Error("no playable url for media",
			"uuid", uuid, "pathType", pathType.String, "ua", r.Header.Get("User-Agent"))
		WriteStatus(w, http.StatusNotFound)
		return
	}

	v.cache.Set(ctx, cacheKey, playURL, cacheTTL)
	http.Redirect(w, r, playURL, http.StatusPermanentRedirect)
}

// Subtitle resolves a subtitle id to its hosted URL.
func (v *Videos) Subtitle(w http.ResponseWriter, r *http.Request) {
	subtitleIDStr := chi.URLParam(r, "subtitleId")
	subtitleID, err := strconv.ParseInt(subtitleIDStr, 10, 64)
	if err != nil || subtitleID <= 0 {
		WriteStatus(w, http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	cacheKey := "video_subtitle_" + subtitleIDStr
	if cached, ok := v.cache.Get(ctx, cacheKey); ok {
		http.Redirect(w, r, cached, http.StatusPermanentRedirect)
		return
	}

	var (
		pathType db.NullString
		pathURL  db.NullString
	)
	err = v.db.QueryRowContext(ctx,
		"SELECT path_type, path_url FROM video_subtitle WHERE id = ? AND deleted_at IS NULL LIMIT 1", subtitleID,
	).Scan(&pathType, &pathURL)
	if err != nil {
		WriteStatus(w, http.StatusUnauthorized)
		return
	}

	cacheTTL := 3 * time.Hour
	subURL := ""
	switch pathType.String {
	case db.PathTypeURL:
		subURL = pathURL.String
	default:
		if v.ext.Enabled() {
			resp, err := v.ext.Post(ctx, "/emby/subtitleGetUrl", map[string]any{
				"user_id":     ctxpkg.UserID(ctx),
				"path_type":   pathType.String,
				"path_url":    pathURL.String,
				"subtitle_id": subtitleID,
			})
			if err == nil && resp.Code == 200 {
				var data struct {
					URL          string `json:"url"`
					CacheSeconds int64  `json:"cache_seconds"`
				}
				_ = json.Unmarshal(resp.Data, &data)
				subURL = data.URL
				if data.CacheSeconds > 0 {
					cacheTTL = time.Duration(data.CacheSeconds) * time.Second
				}
			}
		}
	}

	if subURL == "" {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	v.cache.Set(ctx, cacheKey, subURL, cacheTTL)
	http.Redirect(w, r, subURL, http.StatusPermanentRedirect)
}
