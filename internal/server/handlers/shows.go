package handlers

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Next-Emby/internal/config"
	"github.com/PivKeyU/Next-Emby/internal/db"
	"github.com/PivKeyU/Next-Emby/internal/emby"
	"github.com/PivKeyU/Next-Emby/internal/server/ctxpkg"
)

// Shows serves /Shows/* endpoints (TV series navigation).
type Shows struct {
	db        *sql.DB
	cfg       *config.Config
	log       *slog.Logger
	transform *Transform
}

// NewShows builds the handler.
func NewShows(database *sql.DB, cfg *config.Config, log *slog.Logger) *Shows {
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

	seriesItemID := emby.ItemID(emby.ItemIDTypeVideoList, numericID)
	out := []any{}
	for rows.Next() {
		var (
			id           int64
			seasonNumber int64
			title        string
			description  db.NullString
			createdAt    time.Time
		)
		if err := rows.Scan(&id, &seasonNumber, &title, &description, &createdAt); err != nil {
			continue
		}
		seasonID := emby.ItemID(emby.ItemIDTypeVideoSeason, id)
		displayTitle := title
		if displayTitle == "" {
			displayTitle = fmt.Sprintf("第 %d 季", seasonNumber)
		}

		// Fetch child count lazily to avoid N+1 JOINs.
		var childCount int64
		_ = s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM video_episode WHERE video_season_id = ? AND deleted_at IS NULL", id,
		).Scan(&childCount)

		out = append(out, map[string]any{
			"Name":                   displayTitle,
			"SortName":               displayTitle,
			"ServerId":               s.cfg.EmbyID,
			"Id":                     seasonID,
			"ImageTags":              map[string]any{"Primary": seasonID},
			"CanDelete":              false,
			"CanDownload":            false,
			"SupportsSync":           true,
			"Overview":               nullStr(description),
			"IndexNumber":            seasonNumber,
			"IsFolder":               true,
			"ParentId":               seriesItemID,
			"Type":                   "Season",
			"SeriesId":               seriesItemID,
			"SeriesName":             videoTitle,
			"SeriesPrimaryImageTag":  "image",
			"Genres":                 []any{},
			"People":                 []any{},
			"GenreItems":             []any{},
			"ChildCount":             childCount,
			"Etag":                   seasonID,
			"DateCreated":            emby.FormatTime(createdAt),
			"UserData": map[string]any{
				"UnplayedItemCount":     childCount,
				"PlaybackPositionTicks": 0,
				"PlayCount":             0,
				"IsFavorite":            false,
				"Played":                false,
			},
		})
	}
	WriteJSON(w, http.StatusOK, ItemResponse(out, int64(len(out))))
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
	wheres = append(wheres, "deleted_at IS NULL")
	// yamby sometimes passes the series id here; real season filter goes via seasonid query.
	if kind == emby.ItemIDTypeVideoList {
		wheres = append(wheres, "video_list_id = ?")
		args = append(args, numericID)
	}

	seasonQueryID := q.Get("seasonid")
	var seasonFilterID int64
	if seasonQueryID != "" {
		if _, id, ok := emby.ParseItemID(seasonQueryID); ok {
			seasonFilterID = id
			wheres = append(wheres, "video_season_id = ?")
			args = append(args, id)
		}
	}

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, video_list_id, video_season_id, episode_number, title, description, date_air
		FROM video_episode
		WHERE %s
		ORDER BY episode_number ASC
	`, strings.Join(wheres, " AND ")), args...)
	if err != nil {
		s.log.Error("episodes query", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type epRow struct {
		id            int64
		videoListID   int64
		videoSeasonID int64
		episodeNumber int64
		title         string
		description   db.NullString
		dateAir       sql.NullTime
	}
	var episodes []epRow
	for rows.Next() {
		var e epRow
		if err := rows.Scan(&e.id, &e.videoListID, &e.videoSeasonID, &e.episodeNumber,
			&e.title, &e.description, &e.dateAir); err != nil {
			continue
		}
		episodes = append(episodes, e)
	}

	var (
		seasonTitle  string
		seasonNumber int64
	)
	if seasonFilterID == 0 && len(episodes) > 0 {
		seasonFilterID = episodes[0].videoSeasonID
	}
	if seasonFilterID > 0 {
		_ = s.db.QueryRowContext(ctx,
			"SELECT title, season_number FROM video_season WHERE id = ? LIMIT 1", seasonFilterID,
		).Scan(&seasonTitle, &seasonNumber)
	}

	seriesTitle := ""
	if len(episodes) > 0 {
		seriesTitle = s.transform.GetVideoListTitle(ctx, episodes[0].videoListID)
	}

	out := []any{}
	for _, e := range episodes {
		episodeItemID := emby.ItemID(emby.ItemIDTypeVideoEpisode, e.id)

		// Compute user progress.
		uvr, _ := s.transform.GetUserVideoRecord(ctx, userID, e.videoListID, e.id)

		// Pull first media's duration for RunTimeTicks.
		var firstFileSecond int64
		_ = s.db.QueryRowContext(ctx,
			"SELECT COALESCE(file_second, 0) FROM video_media WHERE video_episode_id = ? AND deleted_at IS NULL LIMIT 1",
			e.id,
		).Scan(&firstFileSecond)

		var mediaSources []any
		if includeMediaSources {
			mediaSources, _ = s.transform.VideoMediaSources(ctx, e.videoListID, e.id, false, "", "")
		}

		out = append(out, map[string]any{
			"Name":                    e.title,
			"SortName":                e.title,
			"Path":                    "/.strm",
			"ServerId":                s.cfg.EmbyID,
			"Id":                      episodeItemID,
			"CanDownload":             true,
			"SupportsSync":            true,
			"PremiereDate":            nullTimeToDate(e.dateAir),
			"RunTimeTicks":            firstFileSecond * emby.TicksPerSecond,
			"Overview":                nullStr(e.description),
			"IndexNumber":             e.episodeNumber,
			"ParentIndexNumber":       seasonNumber,
			"IsFolder":                false,
			"Type":                    "Episode",
			"People":                  []any{},
			"ParentBackdropImageTags": []any{},
			"SeriesId":                emby.ItemID(emby.ItemIDTypeVideoList, e.videoListID),
			"SeriesName":              seriesTitle,
			"SeasonId":                emby.ItemID(emby.ItemIDTypeVideoSeason, e.videoSeasonID),
			"SeasonName":              seasonTitle,
			"PrimaryImageAspectRatio": 1.7,
			"SeriesPrimaryImageTag":   "",
			"ImageTags":               map[string]any{"Primary": episodeItemID},
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
		})
	}

	WriteJSON(w, http.StatusOK, ItemResponse(out, int64(len(out))))
}
