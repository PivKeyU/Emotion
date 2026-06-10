package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"

	"github.com/PivKeyU/Emotion/internal/cache"
	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/external"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// Videos serves /Videos/* endpoints: direct-play redirects and subtitle resolution.
// No transcoding; clients must do direct play.
type Videos struct {
	db       *db.DB
	cache    cache.Cache
	cfg      *config.Config
	log      *slog.Logger
	ext      *external.Client
	playURLs singleflight.Group
}

// NewVideos builds the handler.
func NewVideos(database *db.DB, c cache.Cache, cfg *config.Config, log *slog.Logger) *Videos {
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
	if r.Method != http.MethodHead {
		if cached, ok := v.cache.Get(r.Context(), cacheKey); ok {
			http.Redirect(w, r, cached, http.StatusPermanentRedirect)
			return
		}
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
		mediaID  int64
		pathType db.NullString
		pathURL  db.NullString
		fileSize db.NullInt64
		fileCont db.NullString
	)
	err := v.db.QueryRowContext(ctx, `
		SELECT id, path_type, path_url, file_size, file_container FROM video_media
		WHERE uuid = ? AND status = ? AND deleted_at IS NULL LIMIT 1
	`, uuid, db.MediaStatusComplete).Scan(&mediaID, &pathType, &pathURL, &fileSize, &fileCont)
	if err != nil {
		WriteStatus(w, http.StatusForbidden)
		return
	}

	if r.Method == http.MethodHead {
		if pathType.String == db.PathTypeLocal {
			v.serveLocalFile(w, r, pathURL.String)
			return
		}
		v.writeRemotePlaybackHead(w, fileSize.Int64, fileCont.String)
		return
	}

	v.bumpMediaView(ctx, mediaID)

	cacheTTL := 3 * time.Hour
	playURL := ""

	switch pathType.String {
	case db.PathTypeURL:
		playURL = pathURL.String
	case db.PathTypeLocal:
		// Serve the file directly with Range support. No caching of the result URL
		// since we're streaming bytes, not redirecting.
		v.serveLocalFile(w, r, pathURL.String)
		return
	default:
		if v.ext.Enabled() {
			resolved, err, _ := v.playURLs.Do(cacheKey, func() (any, error) {
				resp, err := v.ext.Post(ctx, "/emby/videoGetUrl", map[string]any{
					"media_id":  mediaID,
					"user_id":   userID,
					"path_type": pathType.String,
					"path_url":  pathURL.String,
					"uuid":      uuid,
					"line":      line,
				})
				if err != nil {
					return nil, err
				}
				if resp.Code != 200 {
					return playURLResult{}, nil
				}
				var data struct {
					URL          string `json:"url"`
					CacheSeconds int64  `json:"cache_seconds"`
				}
				_ = json.Unmarshal(resp.Data, &data)
				return playURLResult{url: data.URL, cacheSeconds: data.CacheSeconds}, nil
			})
			if err != nil {
				v.log.Error("external video url failed", "err", err)
			} else if data, ok := resolved.(playURLResult); ok {
				playURL = data.url
				if data.cacheSeconds > 0 {
					cacheTTL = time.Duration(data.cacheSeconds) * time.Second
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

	playURL = encodeRedirectURL(playURL)
	v.cache.Set(ctx, cacheKey, playURL, cacheTTL)
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(cacheTTL.Seconds())))
	http.Redirect(w, r, playURL, http.StatusPermanentRedirect)
}

type playURLResult struct {
	url          string
	cacheSeconds int64
}

func encodeRedirectURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	return u.String()
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
	case db.PathTypeLocal:
		v.serveLocalFile(w, r, pathURL.String)
		return
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

// serveLocalFile streams a local file with standard HTTP Range support.
// http.ServeFile handles Range, conditional GET, and Content-Type for us.
// We intentionally don't sandbox the path here because the only way a local
// path enters video_media is via the admin-only import flow — trusted input.
func (v *Videos) serveLocalFile(w http.ResponseWriter, r *http.Request, absPath string) {
	info, err := osStat(absPath)
	if err != nil {
		v.log.Warn("local file missing", "path", absPath, "err", err)
		WriteStatus(w, http.StatusNotFound)
		return
	}
	if info.IsDir() {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Accel-Buffering", "no")
	http.ServeFile(w, r, absPath)
}

func (v *Videos) writeRemotePlaybackHead(w http.ResponseWriter, fileSize int64, container string) {
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Accel-Buffering", "no")
	if fileSize > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(fileSize, 10))
	}
	if ct := mime.TypeByExtension("." + directStreamExtension(container)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	WriteStatus(w, http.StatusOK)
}

func (v *Videos) bumpMediaView(parent context.Context, mediaID int64) {
	if mediaID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
	go func() {
		defer cancel()
		if _, err := v.db.ExecContext(ctx,
			"UPDATE video_media SET number_view = number_view + 1 WHERE id = ?", mediaID); err != nil {
			v.log.Debug("view counter update failed", "media_id", mediaID, "err", err)
		}
	}()
}
