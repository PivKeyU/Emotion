package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Emotion/internal/cache"
	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// Items serves /Items/* endpoints.
type Items struct {
	db        *db.DB
	cache     cache.Cache
	cfg       *config.Config
	log       *slog.Logger
	transform *Transform
}

// NewItems builds the handler.
func NewItems(database *db.DB, c cache.Cache, cfg *config.Config, log *slog.Logger) *Items {
	return &Items{
		db:        database,
		cache:     c,
		cfg:       cfg,
		log:       log,
		transform: NewTransform(database, cfg),
	}
}

// Items handles GET /Items. MoviePilot and other automation tools call this
// endpoint directly and expect a JSON envelope, so do not redirect to
// /Users/{id}/Items here.
func (i *Items) Items(w http.ResponseWriter, r *http.Request) {
	userID := queryUserID(r)
	if userID == 0 {
		userID = ctxpkg.UserID(r.Context())
	}
	if userID == 0 && ctxpkg.IsAdmin(r.Context()) {
		i.AdminItems(w, r)
		return
	}
	if userID == 0 {
		WriteStatus(w, http.StatusUnauthorized)
		return
	}

	result, err := i.transform.VideoList(r.Context(), userID, videoListSearchFromQuery(r.URL.Query()))
	if err != nil {
		i.log.Error("top-level video list search failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, ItemResponse(result.Items, result.Count))
}

// AdminItems handles GET /Items for API-key based management tools that do not
// pass UserId. It lists all libraries instead of applying a user folder filter.
func (i *Items) AdminItems(w http.ResponseWriter, r *http.Request) {
	result, err := i.transform.VideoListAll(r.Context(), videoListSearchFromQuery(r.URL.Query()))
	if err != nil {
		i.log.Error("admin video list search failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, ItemResponse(result.Items, result.Count))
}

// ItemInfo returns GET /Items/{itemId}, a common Emby management endpoint.
func (i *Items) ItemInfo(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	if userID == 0 {
		userID = queryUserID(r)
	}
	data, err := i.transform.ItemInfo(r.Context(), userID, chi.URLParam(r, "itemId"))
	if err != nil {
		i.log.Error("top-level item info failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	if data == nil {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	WriteJSON(w, http.StatusOK, data)
}

// Counts returns library totals.
func (i *Items) Counts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	libraryIDs, err := i.visibleLibraryIDs(ctx, r)
	if err != nil {
		i.log.Error("count visible libraries failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	var movie, series, episode int64
	if len(libraryIDs) > 0 {
		if movie, err = i.countVideoLists(ctx, libraryIDs, db.VideoTypeMovie); err != nil {
			i.log.Error("movie count failed", "err", err)
			WriteStatus(w, http.StatusInternalServerError)
			return
		}
		if series, err = i.countVideoLists(ctx, libraryIDs, db.VideoTypeTV); err != nil {
			i.log.Error("series count failed", "err", err)
			WriteStatus(w, http.StatusInternalServerError)
			return
		}
		if episode, err = i.countEpisodes(ctx, libraryIDs); err != nil {
			i.log.Error("episode count failed", "err", err)
			WriteStatus(w, http.StatusInternalServerError)
			return
		}
	}

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
		"ItemCount":       movie + series + episode,
	})
}

func videoListSearchFromQuery(q url.Values) VideoListSearch {
	providerMap := map[string]string{}
	for _, part := range parseCSV(q.Get("anyprovideridequals")) {
		kv := strings.SplitN(part, ".", 2)
		if len(kv) == 2 {
			providerMap[strings.ToLower(kv[0])] = kv[1]
		}
	}
	return VideoListSearch{
		ParentID:         q.Get("parentid"),
		StartIndex:       parseIntQuery(q.Get("startindex"), 0),
		Limit:            parseIntQuery(q.Get("limit"), 20),
		SortOrder:        q.Get("sortorder"),
		SortBy:           q.Get("sortby"),
		Filters:          parseCSV(q.Get("filters")),
		IncludeItemTypes: parseCSV(q.Get("includeitemtypes")),
		SearchTerm:       q.Get("searchterm"),
		NameStartsWith:   q.Get("namestartswith"),
		AnyProviderTmdb:  providerMap["tmdb"],
	}
}

func (i *Items) visibleLibraryIDs(ctx context.Context, r *http.Request) ([]int64, error) {
	if userID := queryUserID(r); userID > 0 {
		return i.transform.UserFolders(ctx, userID)
	}
	if userID := ctxpkg.UserID(r.Context()); userID > 0 {
		return i.transform.UserFolders(ctx, userID)
	}
	return i.transform.AllLibraryIDs(ctx)
}

func (i *Items) countVideoLists(ctx context.Context, libraryIDs []int64, videoType string) (int64, error) {
	where, args := libraryIDFilter(libraryIDs)
	args = append([]any{videoType}, args...)
	var count int64
	err := i.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM video_list WHERE video_type = ? AND deleted_at IS NULL AND video_library_id IN ("+where+")",
		args...,
	).Scan(&count)
	return count, err
}

func (i *Items) countEpisodes(ctx context.Context, libraryIDs []int64) (int64, error) {
	where, args := libraryIDFilter(libraryIDs)
	var count int64
	err := i.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM video_episode ve
		JOIN video_list vl ON vl.id = ve.video_list_id
		WHERE ve.deleted_at IS NULL
		  AND vl.deleted_at IS NULL
		  AND vl.video_library_id IN (`+where+`)`,
		args...,
	).Scan(&count)
	return count, err
}

func libraryIDFilter(ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		ph = append(ph, "?")
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}

// Similar always returns an empty item list.
func (i *Items) Similar(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, EmptyItemResponse())
}

// Image serves an item's image. Remote images (TMDB/Douban/URL) are proxied
// through this server so clients that don't follow cross-origin 301 redirects
// (e.g. MoviePilot's image cache) still receive the bytes directly.
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

	var resolvedURL string
	if cached, ok := i.cache.Get(ctx, cacheKey); ok {
		resolvedURL = cached
	} else {
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
			if kind == emby.ItemIDTypeVideoSeason || kind == emby.ItemIDTypeVideoEpisode {
				parentKind, parentID, ok := i.parentImageTarget(ctx, kind, numericID)
				if ok {
					err = i.db.QueryRowContext(ctx, `
						SELECT path_type, path_url FROM video_image
						WHERE relation_type = ? AND relation_id = ? AND type = ? AND deleted_at IS NULL
						LIMIT 1
					`, parentKind, parentID, imageType).Scan(&pathType, &pathURL)
				}
			}
			if err != nil {
				WriteStatus(w, http.StatusNotFound)
				return
			}
		}

		if pathType.String == db.PathTypeLocal {
			info, err := osStat(pathURL.String)
			if err != nil || info.IsDir() {
				WriteStatus(w, http.StatusNotFound)
				return
			}
			http.ServeFile(w, r, pathURL.String)
			return
		}

		resolvedURL = pathURL.String
		if pathType.String == db.ImagePathTypeTMDB {
			resolvedURL = "https://image.tmdb.org/t/p/w500" + pathURL.String
		}
		if resolvedURL == "" {
			WriteStatus(w, http.StatusNotFound)
			return
		}
		i.cache.Set(ctx, cacheKey, resolvedURL, time.Hour)
	}

	i.proxyRemoteImage(w, r, resolvedURL)
}

func (i *Items) proxyRemoteImage(w http.ResponseWriter, r *http.Request, target string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		WriteStatus(w, http.StatusBadGateway)
		return
	}
	req.Header.Set("User-Agent", "Emotion/1.0")
	if ref := refererForImage(target); ref != "" {
		req.Header.Set("Referer", ref)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		i.log.Warn("image proxy fetch failed", "url", target, "err", err)
		WriteStatus(w, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		WriteStatus(w, resp.StatusCode)
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}

func refererForImage(target string) string {
	if strings.Contains(target, "doubanio.com") || strings.Contains(target, "douban.com") {
		return "https://movie.douban.com/"
	}
	return ""
}

func (i *Items) parentImageTarget(ctx context.Context, kind string, numericID int64) (string, int64, bool) {
	switch kind {
	case emby.ItemIDTypeVideoSeason:
		var listID int64
		if err := i.db.QueryRowContext(ctx,
			"SELECT video_list_id FROM video_season WHERE id = ? AND deleted_at IS NULL LIMIT 1", numericID,
		).Scan(&listID); err == nil && listID > 0 {
			return emby.ItemIDTypeVideoList, listID, true
		}
	case emby.ItemIDTypeVideoEpisode:
		var listID int64
		if err := i.db.QueryRowContext(ctx,
			"SELECT video_list_id FROM video_episode WHERE id = ? AND deleted_at IS NULL LIMIT 1", numericID,
		).Scan(&listID); err == nil && listID > 0 {
			return emby.ItemIDTypeVideoList, listID, true
		}
	}
	return "", 0, false
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

	// Keep PlaybackInfo on the fast path. Sessions/Playing can insert progress
	// rows too, so this best-effort pre-create runs after the response is sent.
	progressCtx, cancelProgress := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	go func() {
		defer cancelProgress()
		i.ensureProgressRow(progressCtx, userID, videoListID, videoSeasonID, videoEpisodeID)
	}()
}

func (i *Items) ensureProgressRow(ctx context.Context, userID, videoListID int64, videoSeasonID, videoEpisodeID db.NullInt64) {
	if userID <= 0 || videoListID <= 0 {
		return
	}
	hasRow, err := hasProgress(ctx, i.db, userID, videoListID, videoEpisodeID)
	if err != nil {
		i.log.Error("progress lookup", "err", err)
		return
	}
	if hasRow {
		return
	}
	if _, err := i.db.ExecContext(ctx, `
		INSERT INTO user_video_record (video_list_id, video_season_id, video_episode_id, user_id)
		VALUES (?, ?, ?, ?)
	`, videoListID, videoSeasonID, videoEpisodeID, userID); err != nil {
		i.log.Warn("progress pre-create failed", "err", err)
	}
}

// hasProgress reports whether a user_video_record row already exists.
func hasProgress(ctx context.Context, d *db.DB, userID, videoListID int64, videoEpisodeID db.NullInt64) (bool, error) {
	query := "SELECT id FROM user_video_record WHERE user_id = ? AND video_list_id = ?"
	args := []any{userID, videoListID}
	if videoEpisodeID.Valid {
		query += " AND video_episode_id = ?"
		args = append(args, videoEpisodeID.Int64)
	} else {
		query += " AND video_episode_id IS NULL"
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
