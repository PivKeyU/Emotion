package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// Shows serves /Shows/* endpoints (TV series navigation).
type Shows struct {
	db        *db.DB
	cfg       *config.Config
	log       *slog.Logger
	transform *Transform
}

// NewShows builds the handler.
func NewShows(database *db.DB, cfg *config.Config, log *slog.Logger) *Shows {
	return &Shows{
		db:        database,
		cfg:       cfg,
		log:       log,
		transform: NewTransform(database, cfg),
	}
}

// NextUp always returns an empty list.
func (s *Shows) NextUp(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, EmptyItemResponse())
}

// Seasons returns the season list for a series.
func (s *Shows) Seasons(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	kind, numericID, ok := emby.ParseItemID(itemID)
	if !ok || kind != emby.ItemIDTypeVideoList {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	ctx := r.Context()

	videoTitle := s.transform.GetVideoListTitle(ctx, numericID)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, season_number, title, description, created_at
		FROM video_season
		WHERE video_list_id = ? AND deleted_at IS NULL
		ORDER BY season_number ASC
	`, numericID)
	if err != nil {
		s.log.Error("seasons query failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type seasonRow struct {
		id           int64
		seasonNumber int64
		title        string
		description  db.NullString
		createdAt    time.Time
	}
	seasonRows := []seasonRow{}
	seasonIDs := []int64{}
	for rows.Next() {
		var row seasonRow
		if err := rows.Scan(&row.id, &row.seasonNumber, &row.title, &row.description, &row.createdAt); err != nil {
			continue
		}
		seasonRows = append(seasonRows, row)
		seasonIDs = append(seasonIDs, row.id)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("seasons rows failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	childCounts, err := s.seasonChildCounts(ctx, seasonIDs)
	if err != nil {
		s.log.Error("season child counts failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	images, err := s.transform.loadImagePresence(ctx, map[string][]int64{
		emby.ItemIDTypeVideoSeason: seasonIDs,
		emby.ItemIDTypeVideoList:   []int64{numericID},
	})
	if err != nil {
		s.log.Error("season image prefetch failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	seriesItemID := emby.ItemID(emby.ItemIDTypeVideoList, numericID)
	out := make([]any, 0, len(seasonRows))
	for _, row := range seasonRows {
		seasonID := emby.ItemID(emby.ItemIDTypeVideoSeason, row.id)
		displayTitle := row.title
		if displayTitle == "" {
			displayTitle = fmt.Sprintf("Season %d", row.seasonNumber)
		}
		childCount := childCounts[row.id]

		item := map[string]any{
			"Name":         displayTitle,
			"SortName":     displayTitle,
			"ServerId":     s.cfg.EmbyID,
			"Id":           seasonID,
			"CanDelete":    false,
			"CanDownload":  false,
			"SupportsSync": true,
			"Overview":     nullStr(row.description),
			"IndexNumber":  row.seasonNumber,
			"IsFolder":     true,
			"ParentId":     seriesItemID,
			"Type":         "Season",
			"SeriesId":     seriesItemID,
			"SeriesName":   videoTitle,
			"Genres":       []any{},
			"People":       []any{},
			"GenreItems":   []any{},
			"ChildCount":   childCount,
			"Etag":         seasonID,
			"DateCreated":  emby.FormatTime(row.createdAt),
			"UserData": map[string]any{
				"UnplayedItemCount":     childCount,
				"PlaybackPositionTicks": 0,
				"PlayCount":             0,
				"IsFavorite":            false,
				"Played":                false,
			},
		}
		s.transform.applyImageFieldsWithPresence(ctx, item, emby.ItemIDTypeVideoSeason, row.id, seasonID, seriesItemID, numericID, images)
		out = append(out, item)
	}
	WriteJSON(w, http.StatusOK, ItemResponse(out, int64(len(out))))
}

func (s *Shows) seasonChildCounts(ctx context.Context, seasonIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	seasonIDs = uniquePositiveIDs(seasonIDs)
	if len(seasonIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(seasonIDs))
	for _, id := range seasonIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT video_season_id, COUNT(*)
		FROM video_episode
		WHERE deleted_at IS NULL
		  AND video_season_id IN (`+placeholdersForIDs(seasonIDs)+`)
		GROUP BY video_season_id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var seasonID, count int64
		if err := rows.Scan(&seasonID, &count); err != nil {
			return nil, err
		}
		out[seasonID] = count
	}
	return out, rows.Err()
}

// Episodes returns episodes for a series/season.
func (s *Shows) Episodes(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	kind, numericID, ok := emby.ParseItemID(itemID)
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	ctx := r.Context()
	userID := ctxpkg.UserID(ctx)

	q := r.URL.Query()
	fields := strings.ToLower(q.Get("fields"))
	includeMediaSources := strings.Contains(fields, "mediasources")

	var (
		wheres []string
		args   []any
	)
	wheres = append(wheres, "ve.deleted_at IS NULL")
	// yamby sometimes passes the series id here; real season filter goes via seasonid query.
	if kind == emby.ItemIDTypeVideoList {
		wheres = append(wheres, "ve.video_list_id = ?")
		args = append(args, numericID)
	} else if kind == emby.ItemIDTypeVideoSeason {
		wheres = append(wheres, "ve.video_season_id = ?")
		args = append(args, numericID)
	}

	seasonQueryID := q.Get("seasonid")
	if seasonQueryID != "" {
		if seasonKind, id, ok := emby.ParseItemID(seasonQueryID); ok && seasonKind == emby.ItemIDTypeVideoSeason {
			wheres = append(wheres, "ve.video_season_id = ?")
			args = append(args, id)
		}
	}

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT
			ve.id, ve.video_list_id, ve.video_season_id, ve.episode_number,
			ve.title, ve.description, ve.date_air,
			COALESCE(vs.title, ''), COALESCE(vs.season_number, 0),
			COALESCE(ve.runtime * 60, vl.runtime * 60, 0)
		FROM video_episode ve
		JOIN video_list vl ON vl.id = ve.video_list_id AND vl.deleted_at IS NULL
		LEFT JOIN video_season vs ON vs.id = ve.video_season_id AND vs.deleted_at IS NULL
		WHERE %s
		ORDER BY COALESCE(vs.season_number, 0), ve.episode_number ASC
	`, strings.Join(wheres, " AND ")), args...)
	if err != nil {
		s.log.Error("episodes query", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type epRow struct {
		id             int64
		videoListID    int64
		videoSeasonID  int64
		episodeNumber  int64
		title          string
		description    db.NullString
		dateAir        sql.NullTime
		seasonTitle    string
		seasonNumber   int64
		runtimeSeconds int64
	}
	var episodes []epRow
	episodeIDs := []int64{}
	seriesIDs := []int64{}
	for rows.Next() {
		var e epRow
		if err := rows.Scan(&e.id, &e.videoListID, &e.videoSeasonID, &e.episodeNumber,
			&e.title, &e.description, &e.dateAir, &e.seasonTitle, &e.seasonNumber, &e.runtimeSeconds); err != nil {
			continue
		}
		episodes = append(episodes, e)
		episodeIDs = append(episodeIDs, e.id)
		seriesIDs = append(seriesIDs, e.videoListID)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("episodes rows failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	seriesTitle := ""
	if len(episodes) > 0 {
		seriesTitle = s.transform.GetVideoListTitle(ctx, episodes[0].videoListID)
	}

	fileSeconds, err := s.firstEpisodeFileSeconds(ctx, episodeIDs)
	if err != nil {
		s.log.Error("episode media duration prefetch failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	progress, err := s.episodeProgress(ctx, userID, episodeIDs)
	if err != nil {
		s.log.Error("episode progress prefetch failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	images, err := s.transform.loadImagePresence(ctx, map[string][]int64{
		emby.ItemIDTypeVideoEpisode: episodeIDs,
		emby.ItemIDTypeVideoList:    seriesIDs,
	})
	if err != nil {
		s.log.Error("episode image prefetch failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	out := make([]any, 0, len(episodes))
	for _, e := range episodes {
		episodeItemID := emby.ItemID(emby.ItemIDTypeVideoEpisode, e.id)

		firstFileSecond := fileSeconds[e.id]
		if firstFileSecond <= 0 {
			firstFileSecond = e.runtimeSeconds
		}

		var mediaSources []any
		if includeMediaSources {
			mediaSources, _ = s.transform.VideoMediaSources(ctx, e.videoListID, e.id, false, "", "")
		}

		uvr := progress[e.id]
		seriesItemID := emby.ItemID(emby.ItemIDTypeVideoList, e.videoListID)
		item := map[string]any{
			"Name":                    e.title,
			"SortName":                e.title,
			"Path":                    "/" + episodeItemID,
			"ServerId":                s.cfg.EmbyID,
			"Id":                      episodeItemID,
			"CanDownload":             true,
			"SupportsSync":            true,
			"PremiereDate":            nullTimeToDate(e.dateAir),
			"RunTimeTicks":            firstFileSecond * emby.TicksPerSecond,
			"Overview":                nullStr(e.description),
			"IndexNumber":             e.episodeNumber,
			"ParentIndexNumber":       e.seasonNumber,
			"IsFolder":                false,
			"Type":                    "Episode",
			"People":                  []any{},
			"ParentBackdropImageTags": []any{},
			"SeriesId":                seriesItemID,
			"SeriesName":              seriesTitle,
			"SeasonId":                emby.ItemID(emby.ItemIDTypeVideoSeason, e.videoSeasonID),
			"SeasonName":              e.seasonTitle,
			"PrimaryImageAspectRatio": 1.7,
			"SeriesPrimaryImageTag":   "",
			"BackdropImageTags":       []any{},
			"Chapters":                []any{},
			"MediaSources":            mediaSources,
			"MediaType":               "Video",
			"UserData": map[string]any{
				"PlayedPercentage":      uvr.Percentage,
				"PlaybackPositionTicks": uvr.PlayMs,
				"PlayCount":             0,
				"IsFavorite":            false,
				"Played":                uvr.IsComplete,
			},
		}
		s.transform.applyImageFieldsWithPresence(ctx, item, emby.ItemIDTypeVideoEpisode, e.id, episodeItemID, seriesItemID, e.videoListID, images)
		out = append(out, item)
	}

	WriteJSON(w, http.StatusOK, ItemResponse(out, int64(len(out))))
}

func (s *Shows) firstEpisodeFileSeconds(ctx context.Context, episodeIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	episodeIDs = uniquePositiveIDs(episodeIDs)
	if len(episodeIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(episodeIDs))
	for _, id := range episodeIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (video_episode_id)
		       video_episode_id, COALESCE(file_second, 0)
		FROM video_media
		WHERE deleted_at IS NULL
		  AND video_episode_id IN (`+placeholdersForIDs(episodeIDs)+`)
		ORDER BY video_episode_id, id ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var episodeID, fileSecond int64
		if err := rows.Scan(&episodeID, &fileSecond); err != nil {
			return nil, err
		}
		out[episodeID] = fileSecond
	}
	return out, rows.Err()
}

func (s *Shows) episodeProgress(ctx context.Context, userID int64, episodeIDs []int64) (map[int64]UserVideoRecord, error) {
	out := map[int64]UserVideoRecord{}
	if userID <= 0 {
		return out, nil
	}
	episodeIDs = uniquePositiveIDs(episodeIDs)
	if len(episodeIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(episodeIDs)+1)
	args = append(args, userID)
	for _, id := range episodeIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (uvr.video_episode_id)
		       uvr.video_episode_id,
		       uvr.play_seconds,
		       uvr.is_complete,
		       COALESCE(NULLIF(vm.file_second, 0), ve.runtime * 60, vl.runtime * 60, 0)
		FROM user_video_record uvr
		JOIN video_episode ve ON ve.id = uvr.video_episode_id
		JOIN video_list vl ON vl.id = uvr.video_list_id
		LEFT JOIN video_media vm ON vm.id = uvr.video_media_id
		WHERE uvr.user_id = ?
		  AND uvr.video_episode_id IN (`+placeholdersForIDs(episodeIDs)+`)
		ORDER BY uvr.video_episode_id, uvr.updated_at DESC, uvr.id DESC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			episodeID   int64
			playSeconds db.NullInt64
			isComplete  db.NullBool
			fileSecond  db.NullInt64
		)
		if err := rows.Scan(&episodeID, &playSeconds, &isComplete, &fileSecond); err != nil {
			return nil, err
		}
		out[episodeID] = s.transform.FormatUserVideoRecord(playSeconds.Int64, isComplete.Bool, fileSecond.Int64)
	}
	return out, rows.Err()
}
