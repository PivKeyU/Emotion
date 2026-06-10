package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
)

// videoMediaRow holds the columns we read for building MediaSources.
type videoMediaRow struct {
	ID            int64
	UUID          string
	Name          string
	FileSize      int64
	FileSecond    int64
	FileMetadata  string
	FileContainer string
	FileChapters  string
	PathType      string
	PathURL       string
}

// loadVideoMedias fetches video_media rows for a list or a specific episode.
func (t *Transform) loadVideoMedias(ctx context.Context, videoListID, videoEpisodeID int64) ([]videoMediaRow, error) {
	query := `SELECT id, uuid, name, COALESCE(file_size,0), COALESCE(file_second,0),
			COALESCE(file_matadata::text,''), COALESCE(file_container,''),
			COALESCE(file_chapters::text,''), COALESCE(path_type,''), COALESCE(path_url,'')
		FROM video_media WHERE deleted_at IS NULL AND video_list_id = ?`
	args := []any{videoListID}
	if videoEpisodeID > 0 {
		query += " AND video_episode_id = ?"
		args = append(args, videoEpisodeID)
	}
	query += " ORDER BY CASE path_type WHEN 'url' THEN 0 WHEN 'local' THEN 1 ELSE 2 END, id ASC"
	rows, err := t.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []videoMediaRow
	for rows.Next() {
		var r videoMediaRow
		if err := rows.Scan(&r.ID, &r.UUID, &r.Name, &r.FileSize, &r.FileSecond,
			&r.FileMetadata, &r.FileContainer, &r.FileChapters, &r.PathType, &r.PathURL); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// videoSubtitleRow holds subtitle columns for a media.
type videoSubtitleRow struct {
	ID    int64
	Title string
	Codec string
}

func (t *Transform) loadSubtitles(ctx context.Context, videoMediaUUID string) ([]videoSubtitleRow, error) {
	rows, err := t.db.QueryContext(ctx, `
		SELECT s.id, s.title, s.codec
		FROM video_subtitle s
		JOIN video_media m ON m.id = s.video_media_id
		WHERE m.uuid = ? AND s.deleted_at IS NULL
	`, videoMediaUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []videoSubtitleRow
	for rows.Next() {
		var r videoSubtitleRow
		if err := rows.Scan(&r.ID, &r.Title, &r.Codec); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (t *Transform) loadSubtitlesForMediaIDs(ctx context.Context, mediaIDs []int64) (map[int64][]videoSubtitleRow, error) {
	out := map[int64][]videoSubtitleRow{}
	if len(mediaIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(mediaIDs))
	args := make([]any, len(mediaIDs))
	for idx, id := range mediaIDs {
		placeholders[idx] = "?"
		args[idx] = id
	}
	rows, err := t.db.QueryContext(ctx, `
		SELECT video_media_id, id, title, codec
		FROM video_subtitle
		WHERE deleted_at IS NULL AND video_media_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY video_media_id, id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mediaID int64
		var sub videoSubtitleRow
		if err := rows.Scan(&mediaID, &sub.ID, &sub.Title, &sub.Codec); err != nil {
			return nil, err
		}
		out[mediaID] = append(out[mediaID], sub)
	}
	return out, rows.Err()
}

// formatStreams parses file_matadata JSON and extracts ffprobe-style streams for the MediaStreams array.
// We return the streams plus the overall bit_rate.
func formatStreams(fileMetadata string) (streams []any, bitRate int64) {
	streams = []any{}
	if strings.TrimSpace(fileMetadata) == "" {
		return
	}
	var meta struct {
		Streams []map[string]any `json:"streams"`
		Format  struct {
			BitRate any `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal([]byte(fileMetadata), &meta); err != nil {
		return
	}
	for _, s := range meta.Streams {
		streams = append(streams, s)
	}
	switch v := meta.Format.BitRate.(type) {
	case string:
		fmt.Sscanf(v, "%d", &bitRate)
	case float64:
		bitRate = int64(v)
	}
	return
}

// VideoMediaSources builds the MediaSources array for an item.
// play_session_id non-empty means we include DirectStreamUrl.
func (t *Transform) VideoMediaSources(ctx context.Context, videoListID, videoEpisodeID int64, noMediaAddDefault bool, playSessionID, apiKey string) ([]any, error) {
	medias, err := t.loadVideoMedias(ctx, videoListID, videoEpisodeID)
	if err != nil {
		return nil, err
	}

	mediaIDs := make([]int64, 0, len(medias))
	for _, m := range medias {
		mediaIDs = append(mediaIDs, m.ID)
	}
	subtitlesByMedia, err := t.loadSubtitlesForMediaIDs(ctx, mediaIDs)
	if err != nil {
		return nil, err
	}

	rows := make([]any, 0, len(medias))
	for _, m := range medias {
		streams, bitRate := formatStreams(m.FileMetadata)

		subs := subtitlesByMedia[m.ID]
		defaultSubID := int64(0)
		for _, s := range subs {
			defaultSubID = s.ID
			streams = append(streams, map[string]any{
				"Codec":                  s.Codec,
				"DisplayTitle":           s.Title,
				"IsInterlaced":           false,
				"IsDefault":              false,
				"IsForced":               false,
				"IsHearingImpaired":      false,
				"Type":                   "Subtitle",
				"Index":                  s.ID,
				"IsExternal":             true,
				"DeliveryMethod":         "External",
				"DeliveryUrl":            fmt.Sprintf("/emby/Videos/%s/Subtitles/%d?api_key=%s", m.UUID, s.ID, apiKey),
				"IsExternalUrl":          false,
				"IsTextSubtitleStream":   true,
				"SupportsExternalStream": true,
				"Path":                   fmt.Sprintf("/subtitles/%d", s.ID),
				"Protocol":               "File",
				"ExtendedVideoType":      "None",
				"ExtendedVideoSubType":   "None",
				"AttachmentSize":         0,
			})
		}

		directStreamURL, addAPIKey := directStreamURLForMedia(m, playSessionID, apiKey)

		item := map[string]any{
			"Chapters":                   []any{},
			"Protocol":                   "Http",
			"Id":                         m.UUID + "_",
			"Path":                       "/" + m.UUID,
			"Type":                       "Default",
			"Container":                  mediaContainer(m.FileContainer),
			"Size":                       m.FileSize,
			"Name":                       m.Name,
			"IsRemote":                   true,
			"RunTimeTicks":               m.FileSecond * emby.TicksPerSecond,
			"HasMixedProtocols":          false,
			"SupportsTranscoding":        false,
			"SupportsDirectStream":       true,
			"SupportsDirectPlay":         true,
			"IsInfiniteStream":           false,
			"RequiresOpening":            false,
			"RequiresClosing":            false,
			"RequiresLooping":            false,
			"SupportsProbing":            false,
			"MediaStreams":               streams,
			"Formats":                    []any{},
			"Bitrate":                    bitRate,
			"RequiredHttpHeaders":        map[string]any{},
			"DirectStreamUrl":            directStreamURL,
			"AddApiKeyToDirectStreamUrl": addAPIKey,
			"ReadAtNativeFramerate":      false,
			"ItemId":                     m.UUID,
		}
		if defaultSubID > 0 {
			item["DefaultSubtitleStreamIndex"] = defaultSubID
		}
		rows = append(rows, item)
	}

	if noMediaAddDefault && len(rows) == 0 {
		rows = append(rows, map[string]any{
			"Id":           "none",
			"Name":         "暂无资源",
			"Path":         "暂无资源",
			"MediaStreams": []any{},
		})
	}
	return rows, nil
}

func directStreamURLForMedia(m videoMediaRow, playSessionID, apiKey string) (any, bool) {
	if playSessionID == "" {
		return nil, false
	}
	return fmt.Sprintf("/Videos/%s/original.%s?line=&api_key=%s", m.UUID, directStreamExtension(m.FileContainer), apiKey), true
}

func directStreamExtension(container string) string {
	ext := strings.ToLower(strings.TrimSpace(mediaContainer(container)))
	switch ext {
	case "", "unknown":
		return "mkv"
	case "matroska":
		return "mkv"
	case "quicktime":
		return "mov"
	case "mpegts":
		return "ts"
	default:
		return ext
	}
}

func mediaContainer(container string) string {
	container = strings.TrimSpace(container)
	if container == "" {
		return "mkv"
	}
	if i := strings.Index(container, ","); i >= 0 {
		return container[:i]
	}
	return container
}

// HasFavorite reports whether user has favorited (type, id).
func (t *Transform) HasFavorite(ctx context.Context, userID int64, relationType string, relationID int64) bool {
	var id int64
	err := t.db.QueryRowContext(ctx,
		"SELECT id FROM favorites WHERE user_id = ? AND relation_type = ? AND relation_id = ? LIMIT 1",
		userID, relationType, relationID,
	).Scan(&id)
	return err == nil && id > 0
}

// UserCanDownload checks user.is_can_down.
func (t *Transform) UserCanDownload(ctx context.Context, userID int64) bool {
	var v db.NullBool
	_ = t.db.QueryRowContext(ctx,
		"SELECT is_can_down FROM app_user WHERE id = ? LIMIT 1", userID,
	).Scan(&v)
	return v.Bool
}

// nullTimeToDate formats a date column, or returns empty string.
func nullTimeToDate(nt sql.NullTime) string {
	if !nt.Valid {
		return ""
	}
	return nt.Time.UTC().Format("2006-01-02T15:04:05.0000000Z")
}

// yearOf returns the 4-digit year or 0.
func yearOf(nt sql.NullTime) int {
	if !nt.Valid || nt.Time.IsZero() {
		return 0
	}
	return nt.Time.Year()
}

// ItemInfo returns the full Emby item object for the given item id.
// Returns nil when the item does not exist.
func (t *Transform) ItemInfo(ctx context.Context, userID int64, itemID string) (map[string]any, error) {
	kind, numericID, ok := emby.ParseItemID(itemID)
	if !ok {
		return nil, nil
	}
	hasFavorited := t.HasFavorite(ctx, userID, kind, numericID)
	canDownload := t.UserCanDownload(ctx, userID)

	switch kind {
	case emby.ItemIDTypeVideoLibrary:
		return t.itemInfoLibrary(ctx, numericID, itemID)
	case emby.ItemIDTypeVideoList:
		return t.itemInfoList(ctx, userID, numericID, itemID, hasFavorited, canDownload)
	case emby.ItemIDTypeVideoSeason:
		return t.itemInfoSeason(ctx, numericID, itemID, hasFavorited, canDownload)
	case emby.ItemIDTypeVideoEpisode:
		return t.itemInfoEpisode(ctx, userID, numericID, itemID, hasFavorited, canDownload)
	}
	return nil, nil
}

func (t *Transform) itemInfoLibrary(ctx context.Context, numericID int64, itemID string) (map[string]any, error) {
	var name string
	err := t.db.QueryRowContext(ctx,
		"SELECT name FROM library WHERE id = ? AND deleted_at IS NULL LIMIT 1", numericID,
	).Scan(&name)
	if err != nil {
		return nil, nil //nolint:nilerr
	}
	childCount, recursiveItemCount := t.libraryCounts(ctx, numericID)
	item := map[string]any{
		"Name":                  name,
		"Id":                    itemID,
		"Guid":                  itemID,
		"Etag":                  itemID,
		"CanDelete":             false,
		"CanDownload":           false,
		"PresentationUniqueKey": itemID,
		"SortName":              name,
		"ForcedSortName":        name,
		"IsFolder":              true,
		"Type":                  "CollectionFolder",
		"UserData": map[string]any{
			"PlaybackPositionTicks": 0,
			"IsFavorite":            false,
			"Played":                false,
		},
		"PrimaryImageAspectRatio": 1.7,
		"LockData":                true,
		"ServerId":                t.cfg.EmbyID,
		"ChildCount":              childCount,
		"RecursiveItemCount":      recursiveItemCount,
	}
	t.applyImageFields(ctx, item, emby.ItemIDTypeVideoLibrary, numericID, itemID, "", 0)
	return item, nil
}

func (t *Transform) itemInfoList(ctx context.Context, userID, numericID int64, itemID string, hasFavorited, canDownload bool) (map[string]any, error) {
	var (
		videoType      string
		tmdbID         db.NullString
		title          string
		originTitle    db.NullString
		description    db.NullString
		dateAir        sql.NullTime
		createdAt      time.Time
		updatedAt      time.Time
		videoLibraryID int64
	)
	err := t.db.QueryRowContext(ctx, `
		SELECT video_type, tmdb_id, title, origin_title, description, date_air, created_at, updated_at, video_library_id
		FROM video_list WHERE id = ? AND deleted_at IS NULL LIMIT 1
	`, numericID).Scan(&videoType, &tmdbID, &title, &originTitle, &description, &dateAir, &createdAt, &updatedAt, &videoLibraryID)
	if err != nil {
		return nil, nil //nolint:nilerr
	}

	isMovie := videoType == db.VideoTypeMovie
	childCount := int64(0)
	var uvr UserVideoRecord
	if isMovie {
		uvr, _ = t.GetUserVideoRecord(ctx, userID, numericID, 0)
	} else {
		_ = t.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM video_season WHERE video_list_id = ? AND deleted_at IS NULL", numericID,
		).Scan(&childCount)
	}

	externalURLs := []any{}
	providerIDs := map[string]any{}
	if tmdbID.Valid && tmdbID.String != "" {
		externalURLs = append(externalURLs, map[string]any{
			"Name": "TheMovieDb",
			"Url":  fmt.Sprintf("https://www.themoviedb.org/%s/%s", videoType, tmdbID.String),
		})
		providerIDs["Tmdb"] = tmdbID.String
	}

	var mediaSources []any
	if isMovie {
		mediaSources, _ = t.VideoMediaSources(ctx, numericID, 0, true, "", "")
	}
	size, bitrate, runtimeTicks, mediaStreams := summarizeMediaSources(mediaSources)

	item := map[string]any{
		"Name":                  title,
		"OriginalTitle":         nullStr(originTitle),
		"Id":                    itemID,
		"DateCreated":           emby.FormatTime(createdAt),
		"DateModified":          emby.FormatTime(updatedAt),
		"CanDelete":             false,
		"CanDownload":           canDownload,
		"PresentationUniqueKey": itemID,
		"SupportsSync":          true,
		"SortName":              title,
		"ForcedSortName":        title,
		"ExternalUrls":          externalURLs,
		"MediaSources":          mediaSources,
		"ProductionLocations":   []any{},
		"Path":                  "/" + videoType,
		"Overview":              nullStr(description),
		"Taglines":              []any{},
		"Genres":                []any{},
		"Size":                  size,
		"FileName":              title,
		"ProductionYear":        yearOf(dateAir),
		"RemoteTrailers":        []any{},
		"ProviderIds":           providerIDs,
		"IsFolder":              !isMovie,
		"ParentId":              emby.ItemID(emby.ItemIDTypeVideoLibrary, videoLibraryID),
		"Type":                  typeOfVideo(isMovie),
		"People":                []any{},
		"Studios":               []any{},
		"GenreItems":            []any{},
		"TagItems":              []any{},
		"LocalTrailerCount":     0,
		"UserData": map[string]any{
			"PlayedPercentage":      uvr.Percentage,
			"PlaybackPositionTicks": uvr.PlayMs,
			"PlayCount":             0,
			"IsFavorite":            hasFavorited,
			"Played":                uvr.IsComplete,
		},
		"ChildCount":           childCount,
		"RecursiveItemCount":   childCount,
		"DisplayPreferencesId": itemID,
		"AirDays":              []any{},
		"MediaStreams":         mediaStreams,
		"Bitrate":              bitrate,
		"RunTimeTicks":         runtimeTicks,
		"PartCount":            1,
		"DisplayOrder":         "Aired",
		"Chapters":             []any{},
		"MediaType":            "Video",
		"LockedFields":         []any{},
		"LockData":             true,
		"ServerId":             t.cfg.EmbyID,
		"Etag":                 itemID,
	}
	if isMovie {
		item["PrimaryImageAspectRatio"] = 0.67
	} else {
		item["PrimaryImageAspectRatio"] = 0.67
	}
	t.applyImageFields(ctx, item, emby.ItemIDTypeVideoList, numericID, itemID, "", 0)
	return item, nil
}

func (t *Transform) itemInfoSeason(ctx context.Context, numericID int64, itemID string, hasFavorited, canDownload bool) (map[string]any, error) {
	var (
		videoListID  int64
		seasonNumber int64
		title        string
		description  db.NullString
		dateAir      sql.NullTime
		createdAt    time.Time
		updatedAt    time.Time
	)
	err := t.db.QueryRowContext(ctx, `
		SELECT video_list_id, season_number, title, description, date_air, created_at, updated_at
		FROM video_season WHERE id = ? AND deleted_at IS NULL LIMIT 1
	`, numericID).Scan(&videoListID, &seasonNumber, &title, &description, &dateAir, &createdAt, &updatedAt)
	if err != nil {
		return nil, nil //nolint:nilerr
	}

	seriesName := t.GetVideoListTitle(ctx, videoListID)

	var childCount int64
	_ = t.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM video_episode WHERE video_season_id = ? AND deleted_at IS NULL", numericID,
	).Scan(&childCount)

	seriesItemID := emby.ItemID(emby.ItemIDTypeVideoList, videoListID)
	item := map[string]any{
		"Name":                    title,
		"Id":                      itemID,
		"DateCreated":             emby.FormatTime(createdAt),
		"DateModified":            emby.FormatTime(updatedAt),
		"CanDelete":               false,
		"CanDownload":             canDownload,
		"PresentationUniqueKey":   itemID,
		"SupportsSync":            true,
		"SortName":                title,
		"ForcedSortName":          title,
		"PremiereDate":            nullTimeToDate(dateAir),
		"ExternalUrls":            []any{},
		"Path":                    "/" + itemID,
		"Overview":                nullStr(description),
		"Taglines":                []any{},
		"Genres":                  []any{},
		"FileName":                fmt.Sprintf("%d", seasonNumber),
		"ProductionYear":          yearOf(dateAir),
		"IndexNumber":             seasonNumber,
		"RemoteTrailers":          []any{},
		"ProviderIds":             map[string]any{},
		"IsFolder":                true,
		"ParentId":                emby.ItemID(emby.ItemIDTypeVideoList, videoListID),
		"Type":                    "Season",
		"People":                  []any{},
		"Studios":                 []any{},
		"GenreItems":              []any{},
		"TagItems":                []any{},
		"ParentBackdropImageTags": []any{},
		"UserData": map[string]any{
			"PlaybackPositionTicks": 0,
			"PlayCount":             0,
			"IsFavorite":            hasFavorited,
			"Played":                false,
		},
		"ChildCount":              childCount,
		"SeriesId":                seriesItemID,
		"SeriesName":              seriesName,
		"DisplayPreferencesId":    "",
		"PrimaryImageAspectRatio": 0.6,
		"LockedFields":            []any{},
		"LockData":                true,
		"ServerId":                t.cfg.EmbyID,
		"Etag":                    itemID,
	}
	t.applyImageFields(ctx, item, emby.ItemIDTypeVideoSeason, numericID, itemID, seriesItemID, videoListID)
	return item, nil
}

func (t *Transform) itemInfoEpisode(ctx context.Context, userID, numericID int64, itemID string, hasFavorited, canDownload bool) (map[string]any, error) {
	var (
		videoListID   int64
		videoSeasonID int64
		episodeNumber int64
		title         string
		description   db.NullString
		dateAir       sql.NullTime
		createdAt     time.Time
		updatedAt     time.Time
	)
	err := t.db.QueryRowContext(ctx, `
		SELECT video_list_id, video_season_id, episode_number, title, description, date_air, created_at, updated_at
		FROM video_episode WHERE id = ? AND deleted_at IS NULL LIMIT 1
	`, numericID).Scan(&videoListID, &videoSeasonID, &episodeNumber, &title, &description, &dateAir, &createdAt, &updatedAt)
	if err != nil {
		return nil, nil //nolint:nilerr
	}

	seriesName := t.GetVideoListTitle(ctx, videoListID)

	var (
		seasonTitle  string
		seasonNumber int64
	)
	_ = t.db.QueryRowContext(ctx,
		"SELECT title, season_number FROM video_season WHERE id = ? LIMIT 1", videoSeasonID,
	).Scan(&seasonTitle, &seasonNumber)

	uvr, _ := t.GetUserVideoRecord(ctx, userID, videoListID, numericID)
	mediaSources, _ := t.VideoMediaSources(ctx, videoListID, numericID, true, "", "")
	size, bitrate, runtimeTicks, mediaStreams := summarizeMediaSources(mediaSources)

	seriesItemID := emby.ItemID(emby.ItemIDTypeVideoList, videoListID)
	seasonItemID := emby.ItemID(emby.ItemIDTypeVideoSeason, videoSeasonID)
	item := map[string]any{
		"Name":                    title,
		"Id":                      itemID,
		"DateCreated":             emby.FormatTime(createdAt),
		"DateModified":            emby.FormatTime(updatedAt),
		"CanDelete":               false,
		"CanDownload":             canDownload,
		"PresentationUniqueKey":   itemID,
		"SupportsSync":            true,
		"SortName":                title,
		"ForcedSortName":          title,
		"PremiereDate":            nullTimeToDate(dateAir),
		"ExternalUrls":            []any{},
		"MediaSources":            mediaSources,
		"Path":                    "/" + itemID,
		"Overview":                nullStr(description),
		"Taglines":                []any{},
		"Genres":                  []any{},
		"FileName":                fmt.Sprintf("%d", episodeNumber),
		"ProductionYear":          yearOf(dateAir),
		"IndexNumber":             episodeNumber,
		"ParentIndexNumber":       seasonNumber,
		"RemoteTrailers":          []any{},
		"ProviderIds":             map[string]any{},
		"IsFolder":                false,
		"ParentId":                emby.ItemID(emby.ItemIDTypeVideoSeason, videoSeasonID),
		"Type":                    "Episode",
		"People":                  []any{},
		"Studios":                 []any{},
		"GenreItems":              []any{},
		"TagItems":                []any{},
		"ParentBackdropImageTags": []any{},
		"LocalTrailerCount":       0,
		"Size":                    size,
		"Bitrate":                 bitrate,
		"RunTimeTicks":            runtimeTicks,
		"MediaStreams":            mediaStreams,
		"UserData": map[string]any{
			"PlayedPercentage":      uvr.Percentage,
			"PlaybackPositionTicks": uvr.PlayMs,
			"PlayCount":             0,
			"IsFavorite":            hasFavorited,
			"Played":                uvr.IsComplete,
		},
		"SeriesId":                seriesItemID,
		"SeriesName":              seriesName,
		"SeasonId":                seasonItemID,
		"SeasonName":              seasonTitle,
		"DisplayPreferencesId":    "",
		"PrimaryImageAspectRatio": 1.7,
		"PartCount":               0,
		"MediaType":               "Video",
		"LockedFields":            []any{},
		"LockData":                true,
		"ServerId":                t.cfg.EmbyID,
		"Etag":                    itemID,
	}
	t.applyImageFields(ctx, item, emby.ItemIDTypeVideoEpisode, numericID, itemID, seriesItemID, videoListID)
	return item, nil
}

func summarizeMediaSources(sources []any) (size int64, bitrate int64, runtimeTicks int64, streams []any) {
	streams = []any{}
	for _, src := range sources {
		m, ok := src.(map[string]any)
		if !ok {
			continue
		}
		if size == 0 {
			size = anyInt64(m["Size"])
		}
		if bitrate == 0 {
			bitrate = anyInt64(m["Bitrate"])
		}
		if runtimeTicks == 0 {
			runtimeTicks = anyInt64(m["RunTimeTicks"])
		}
		if raw, ok := m["MediaStreams"].([]any); ok && len(raw) > 0 && len(streams) == 0 {
			streams = raw
		}
	}
	return
}

func anyInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}

type imageKey struct {
	kind      string
	id        int64
	imageType string
}

type imagePresence map[imageKey]bool

func (p imagePresence) has(kind string, id int64, imageType string) bool {
	if p == nil {
		return false
	}
	return p[imageKey{kind: kind, id: id, imageType: imageType}]
}

func (t *Transform) loadImagePresence(ctx context.Context, targets map[string][]int64) (imagePresence, error) {
	presence := imagePresence{}
	clauses := []string{}
	args := []any{db.ImageTypePrimary, db.ImageTypeBackdrop}

	for kind, ids := range targets {
		ids = uniquePositiveIDs(ids)
		if len(ids) == 0 {
			continue
		}
		clauses = append(clauses, "relation_type = ? AND relation_id IN ("+placeholdersForIDs(ids)+")")
		args = append(args, kind)
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if len(clauses) == 0 {
		return presence, nil
	}

	rows, err := t.db.QueryContext(ctx, `
		SELECT relation_type, relation_id, type
		FROM video_image
		WHERE deleted_at IS NULL
		  AND type IN (?, ?)
		  AND (`+strings.Join(clauses, " OR ")+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var kind, typ string
		var id int64
		if err := rows.Scan(&kind, &id, &typ); err != nil {
			return nil, err
		}
		presence[imageKey{kind: kind, id: id, imageType: typ}] = true
	}
	return presence, rows.Err()
}

func uniquePositiveIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := map[int64]struct{}{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func placeholdersForIDs(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i := range ids {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func (t *Transform) applyImageFields(ctx context.Context, item map[string]any, kind string, numericID int64, itemID, seriesItemID string, seriesID int64) {
	t.applyImageFieldsWithPresence(ctx, item, kind, numericID, itemID, seriesItemID, seriesID, nil)
}

func (t *Transform) applyImageFieldsWithPresence(ctx context.Context, item map[string]any, kind string, numericID int64, itemID, seriesItemID string, seriesID int64, images imagePresence) {
	item["ImageTags"] = map[string]any{}
	item["BackdropImageTags"] = []any{}

	hasImage := func(kind string, id int64, imageType string) bool {
		if images != nil {
			return images.has(kind, id, imageType)
		}
		return t.hasImage(ctx, kind, id, imageType)
	}

	hasOwnPrimary := hasImage(kind, numericID, db.ImageTypePrimary)
	hasSeriesPrimary := seriesID > 0 && hasImage(emby.ItemIDTypeVideoList, seriesID, db.ImageTypePrimary)
	if hasSeriesPrimary {
		item["SeriesPrimaryImageTag"] = seriesItemID
	}

	if hasImage(kind, numericID, db.ImageTypeBackdrop) {
		item["BackdropImageTags"] = []any{itemID}
	} else if seriesID > 0 && hasImage(emby.ItemIDTypeVideoList, seriesID, db.ImageTypeBackdrop) {
		item["ParentBackdropItemId"] = seriesItemID
		item["ParentBackdropImageTags"] = []any{seriesItemID}
	}

	if hasOwnPrimary {
		item["ImageTags"] = map[string]any{"Primary": itemID}
		item["PrimaryImageTag"] = itemID
		return
	}
	if hasSeriesPrimary {
		item["PrimaryImageItemId"] = seriesItemID
		item["ImageTags"] = map[string]any{"Primary": seriesItemID}
		return
	}
	if seriesItemID != "" {
		item["SeriesPrimaryImageTag"] = ""
	}
}

func (t *Transform) hasImage(ctx context.Context, kind string, numericID int64, imageType string) bool {
	var exists int64
	_ = t.db.QueryRowContext(ctx, `
		SELECT 1 FROM video_image
		WHERE relation_type = ? AND relation_id = ? AND type = ? AND deleted_at IS NULL
		LIMIT 1
	`, kind, numericID, imageType).Scan(&exists)
	return exists > 0
}

func nullStr(ns db.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

func typeOfVideo(isMovie bool) string {
	if isMovie {
		return "Movie"
	}
	return "Series"
}
