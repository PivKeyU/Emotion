package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
)

// VideoListSearch is the search / list specification used by emya UserItems.
type VideoListSearch struct {
	ParentID         string
	StartIndex       int
	Limit            int
	SortOrder        string // Ascending / Descending
	SortBy           string // csv
	Filters          []string
	IncludeItemTypes []string
	SearchTerm       string
	NameStartsWith   string
	AnyProviderTmdb  string
}

// VideoListResult is (rows, total).
type VideoListResult struct {
	Items []any
	Count int64
}

// VideoList searches video_list filtered by the user's libraries and query opts.
func (t *Transform) VideoList(ctx context.Context, userID int64, s VideoListSearch) (VideoListResult, error) {
	folders, err := t.UserFolders(ctx, userID)
	if err != nil {
		return VideoListResult{}, err
	}
	return t.runVideoListSearch(ctx, userID, s, folders)
}

// VideoListAll searches all libraries for admin/API-key management callers.
func (t *Transform) VideoListAll(ctx context.Context, s VideoListSearch) (VideoListResult, error) {
	rows, err := t.db.QueryContext(ctx, "SELECT id FROM library WHERE deleted_at IS NULL ORDER BY id ASC")
	if err != nil {
		return VideoListResult{}, err
	}
	defer rows.Close()

	folders := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return VideoListResult{}, err
		}
		folders = append(folders, id)
	}
	if err := rows.Err(); err != nil {
		return VideoListResult{}, err
	}
	return t.runVideoListSearch(ctx, 0, s, folders)
}

// runVideoListSearch is the cleaned implementation used by VideoList.
func (t *Transform) runVideoListSearch(ctx context.Context, userID int64, s VideoListSearch, folders []int64) (VideoListResult, error) {
	var (
		joinArgs  []any
		joinParts []string
		whereArgs []any
		wheres    []string
	)

	// Folder access filter.
	if len(folders) == 0 {
		return VideoListResult{Items: []any{}, Count: 0}, nil
	}
	ph := make([]string, 0, len(folders))
	for _, f := range folders {
		ph = append(ph, "?")
		whereArgs = append(whereArgs, f)
	}
	wheres = append(wheres, "video_list.deleted_at IS NULL")
	wheres = append(wheres, "video_list.video_library_id IN ("+strings.Join(ph, ",")+")")

	// Parent id.
	if s.ParentID != "" {
		if kind, id, ok := emby.ParseItemID(s.ParentID); ok && kind == emby.ItemIDTypeVideoLibrary {
			wheres = append(wheres, "video_list.video_library_id = ?")
			whereArgs = append(whereArgs, id)
		}
	}

	// Search term.
	searchTerm := s.SearchTerm
	if searchTerm == "" {
		searchTerm = s.NameStartsWith
	}
	if searchTerm != "" {
		like := "%" + searchTerm + "%"
		joinParts = append(joinParts,
			`LEFT JOIN video_list_title_alias
				ON video_list_title_alias.video_list_id = video_list.id
				AND video_list_title_alias.deleted_at IS NULL`)
		wheres = append(wheres, "(video_list.title LIKE ? OR video_list_title_alias.title LIKE ?)")
		whereArgs = append(whereArgs, like, like)
	}

	// Favorite filter.
	if contains(s.Filters, "IsFavorite") {
		joinParts = append(joinParts, `INNER JOIN favorites
			ON favorites.relation_type = ? AND favorites.relation_id = video_list.id AND favorites.user_id = ?`)
		joinArgs = append(joinArgs, emby.ItemIDTypeVideoList, userID)

		// When filtering by favorite, constrain by include types if provided.
		if len(s.IncludeItemTypes) > 0 {
			types := []string{}
			if contains(s.IncludeItemTypes, "Series") {
				types = append(types, db.VideoTypeTV)
			}
			if contains(s.IncludeItemTypes, "Movie") {
				types = append(types, db.VideoTypeMovie)
			}
			if len(types) == 0 {
				return VideoListResult{Items: []any{}, Count: 0}, nil
			}
			tPh := make([]string, 0, len(types))
			for _, t := range types {
				tPh = append(tPh, "?")
				whereArgs = append(whereArgs, t)
			}
			wheres = append(wheres, "video_list.video_type IN ("+strings.Join(tPh, ",")+")")
		}
	}

	// TMDB filter.
	if s.AnyProviderTmdb != "" {
		wheres = append(wheres, "video_list.tmdb_id = ?")
		whereArgs = append(whereArgs, s.AnyProviderTmdb)
	}

	// Sort column.
	sortCol := "video_list.updated_at"
	if strings.Contains(s.SortBy, "DateCreated") {
		sortCol = "video_list.id"
	}
	if strings.Contains(s.SortBy, "ProductionYear") || strings.Contains(s.SortBy, "PremiereDate") {
		sortCol = "video_list.date_air"
	}
	sortDir := "DESC"
	if strings.EqualFold(s.SortOrder, "Ascending") {
		sortDir = "ASC"
	}

	// Pagination.
	limit := s.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := s.StartIndex
	if offset < 0 {
		offset = 0
	}

	// Build SQL.
	joinClause := strings.Join(joinParts, "\n")
	whereClause := strings.Join(wheres, " AND ")

	selectSQL := fmt.Sprintf(`
		SELECT video_list.id, video_list.tmdb_id, video_list.video_type, video_list.title,
		       video_list.date_air, video_list.created_at
		FROM video_list
		%s
		WHERE %s
		ORDER BY %s %s
		LIMIT ? OFFSET ?`, joinClause, whereClause, sortCol, sortDir)

	countSQL := fmt.Sprintf(`
		SELECT COUNT(DISTINCT video_list.id)
		FROM video_list
		%s
		WHERE %s`, joinClause, whereClause)

	// Assemble args.
	selectArgs := append([]any{}, joinArgs...)
	selectArgs = append(selectArgs, whereArgs...)
	selectArgs = append(selectArgs, limit, offset)

	countArgs := append([]any{}, joinArgs...)
	countArgs = append(countArgs, whereArgs...)

	var total int64
	if err := t.db.QueryRowContext(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return VideoListResult{}, err
	}

	rows, err := t.db.QueryContext(ctx, selectSQL, selectArgs...)
	if err != nil {
		return VideoListResult{}, err
	}
	defer rows.Close()

	type listRow struct {
		id        int64
		tmdbID    db.NullString
		videoType string
		title     string
		dateAir   sql.NullTime
		createdAt time.Time
	}
	listRows := []listRow{}
	listIDs := []int64{}
	for rows.Next() {
		var row listRow
		if err := rows.Scan(&row.id, &row.tmdbID, &row.videoType, &row.title, &row.dateAir, &row.createdAt); err != nil {
			return VideoListResult{}, err
		}
		listRows = append(listRows, row)
		listIDs = append(listIDs, row.id)
	}
	if err := rows.Err(); err != nil {
		return VideoListResult{}, err
	}

	images, err := t.loadImagePresence(ctx, map[string][]int64{
		emby.ItemIDTypeVideoList: listIDs,
	})
	if err != nil {
		return VideoListResult{}, err
	}

	items := make([]any, 0, len(listRows))
	for _, row := range listRows {
		isMovie := row.videoType == db.VideoTypeMovie
		rowID := emby.ItemID(emby.ItemIDTypeVideoList, row.id)
		item := map[string]any{
			"Name":           row.title,
			"ServerId":       t.cfg.EmbyID,
			"Id":             rowID,
			"DateCreated":    emby.FormatTime(createdAt),
			"Path":           "/" + rowID,
			"DateCreated":    emby.FormatTime(row.createdAt),
			"Path":           "/.strm",
			"Genres":         []any{},
			"People":         []any{},
			"GenreItems":     []any{},
			"ProductionYear": yearOf(row.dateAir),
			"ProviderIds": map[string]any{
				"Tmdb": row.tmdbID.String,
			},
			"IsFolder": !isMovie,
			"Type":     typeOfVideo(isMovie),
			"UserData": map[string]any{
				"PlaybackPositionTicks": 0,
				"PlayCount":             0,
				"IsFavorite":            false,
				"Played":                false,
			},
			"PrimaryImageAspectRatio": 0.67,
			"MediaType":               "Video",
			"CanDelete":               false,
			"CanDownload":             false,
		}
		t.applyImageFieldsWithPresence(ctx, item, emby.ItemIDTypeVideoList, row.id, rowID, "", 0, images)
		items = append(items, item)
	}
	return VideoListResult{Items: items, Count: total}, nil
}

// contains is a tiny helper for string-slice membership.
func contains(slice []string, needle string) bool {
	for _, s := range slice {
		if s == needle {
			return true
		}
	}
	return false
}

// parseCSV splits a comma-separated query parameter into trimmed non-empty parts.
func parseCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseIntQuery is a tolerant strconv that returns def on empty/invalid.
func parseIntQuery(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
