package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Next-Emby/internal/cache"
	"github.com/PivKeyU/Next-Emby/internal/config"
	"github.com/PivKeyU/Next-Emby/internal/db"
	"github.com/PivKeyU/Next-Emby/internal/emby"
	"github.com/PivKeyU/Next-Emby/internal/server/ctxpkg"
)

// Items serves /Items/* endpoints.
type Items struct {
	db        *sql.DB
	cache     cache.Cache
	cfg       *config.Config
	log       *slog.Logger
	transform *Transform
}

// NewItems builds the handler.
func NewItems(database *sql.DB, c cache.Cache, cfg *config.Config, log *slog.Logger) *Items {
	return &Items{
		db:        database,
		cache:     c,
		cfg:       cfg,
		log:       log,
		transform: NewTransform(database, cfg),
	}
}

// Items handles GET /Items - hills clients use this for search.
// emya redirects to /Users/{userid}/items; we delegate inline.
func (i *Items) Items(w http.ResponseWriter, r *http.Request) {
	// Rewrite URL to /Users/{userid}/Items so we can reuse UsersController.Items semantics.
	q := r.URL.Query()
	userID := q.Get("userid")
	if userID == "" {
		userID = strconv.FormatInt(ctxpkg.UserID(r.Context()), 10)
	}

	// Build a synthetic redirect as emya does (301).
	path := strings.Replace(r.URL.Path, "/Items", fmt.Sprintf("/Users/%s/Items", userID), 1)
	target := path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// Counts returns library totals.
func (i *Items) Counts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var movie, series, episode int64
	_ = i.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM video_list WHERE video_type = ? AND deleted_at IS NULL",
		db.VideoTypeMovie).Scan(&movie)
	_ = i.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM video_season WHERE deleted_at IS NULL").Scan(&series)
	_ = i.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM video_episode WHERE deleted_at IS NULL").Scan(&episode)

	WriteJSON(w, http.StatusOK, map[string]any{
		"MovieCount":      movie,
		"SeriesCount":     series,
		"EpisodeCount":    episode,
		"GameCount":       0,
		"ArtistCount":     0,
		"ProgramCount":    0,
		"GameSystemCount": 0,
		"TrailerCount":    0,
		"SongCount":       0,
		"AlbumCount":      0,
		"MusicVideoCount": 0,
		"BoxSetCount":     0,
		"BookCount":       0,
		"ItemCount":       0,
	})
}

// Similar always returns an empty item list.
func (i *Items) Similar(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, EmptyItemResponse())
}

// Image serves an item's image by 301-redirecting to the stored URL.
// Cached to avoid re-hitting the DB for every UI tile.
func (i *Items) Image(w http.ResponseWriter, r *http.Request) {
	itemIDStr := chi.URLParam(r, "itemId")
	imageType := chi.URLParam(r, "imageType")

	kind, numericID, ok := emby.ParseItemID(itemIDStr)
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	ctx := r.Context()
	cacheKey := "image_" + itemIDStr + "_" + imageType
	if cached, ok := i.cache.Get(ctx, cacheKey); ok {
		http.Redirect(w, r, cached, http.StatusMovedPermanently)
		return
	}

	var (
		pathType db.NullString
		pathURL  db.NullString
	)
	err := i.db.QueryRowContext(ctx, `
		SELECT path_type, path_url FROM video_image
		WHERE relation_type = ? AND relation_id = ? AND type = ? AND deleted_at IS NULL
		LIMIT 1
	`, kind, numericID, imageType).Scan(&pathType, &pathURL)
	if err != nil {
		WriteStatus(w, http.StatusForbidden)
		return
	}

	url := pathURL.String
	switch pathType.String {
	case db.ImagePathTypeTMDB:
		url = "https://image.tmdb.org/t/p/w400" + pathURL.String
	case db.ImagePathTypeDouban:
		// Douban URLs are usually stored verbatim; keep as-is.
	}

	if url == "" {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	i.cache.Set(ctx, cacheKey, url, time.Hour)
	http.Redirect(w, r, url, http.StatusMovedPermanently)
}

// PlaybackInfo returns MediaSources + PlaySessionId for a given item.
// On success we also ensure there's an open user_video_record for progress reporting.
func (i *Items) PlaybackInfo(w http.ResponseWriter, r *http.Request) {
	itemIDStr := chi.URLParam(r, "itemId")
	kind, numericID, ok := emby.ParseItemID(itemIDStr)
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	userID := ctxpkg.UserID(r.Context())
	ctx := r.Context()

	var (
		videoListID    int64
		videoSeasonID  db.NullInt64
		videoEpisodeID db.NullInt64
	)
	switch kind {
	case emby.ItemIDTypeVideoList:
		videoListID = numericID
	case emby.ItemIDTypeVideoEpisode:
		videoEpisodeID.Valid = true
		videoEpisodeID.Int64 = numericID
		var season db.NullInt64
		err := i.db.QueryRowContext(ctx,
			"SELECT video_list_id, video_season_id FROM video_episode WHERE id = ? LIMIT 1", numericID,
		).Scan(&videoListID, &season)
		if err != nil {
			WriteStatus(w, http.StatusNotFound)
			return
		}
		videoSeasonID = season
	default:
		WriteStatus(w, http.StatusBadRequest)
		return
	}

	// Upsert progress row so Sessions/Playing updates have a target.
	hasRow, err := hasProgress(ctx, i.db, userID, videoListID, videoEpisodeID)
	if err != nil {
		i.log.Error("progress lookup", "err", err)
	}
	if !hasRow {
		_, _ = i.db.ExecContext(ctx, `
			INSERT INTO user_video_record (video_list_id, video_season_id, video_episode_id, user_id)
			VALUES (?, ?, ?, ?)
		`, videoListID, videoSeasonID, videoEpisodeID, userID)
	}

	playSessionID := itemIDStr
	mediaSources, err := i.transform.VideoMediaSources(ctx, videoListID, videoEpisodeID.Int64, false, playSessionID, ctxpkg.Token(ctx))
	if err != nil {
		i.log.Error("media sources", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"MediaSources":  mediaSources,
		"PlaySessionId": playSessionID,
	})
}

// hasProgress reports whether a user_video_record row already exists.
func hasProgress(ctx context.Context, d *sql.DB, userID, videoListID int64, videoEpisodeID db.NullInt64) (bool, error) {
	query := "SELECT id FROM user_video_record WHERE user_id = ? AND video_list_id = ?"
	args := []any{userID, videoListID}
	if videoEpisodeID.Valid {
		query += " AND video_episode_id = ?"
		args = append(args, videoEpisodeID.Int64)
	}
	query += " LIMIT 1"
	var id int64
	if err := d.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return id > 0, nil
}

// itoa is a tiny helper so we don't pull strconv into files that only need one call.
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// Ensure json import is kept for future use; silence "imported and not used".
var _ = json.Marshal
