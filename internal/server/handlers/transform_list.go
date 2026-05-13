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
	ParentID        string
	StartIndex      int
	Limit           int
	SortOrder       string // Ascending / Descending
	SortBy          string // csv
	Filters         []string
	IncludeItemTypes []string
	SearchTerm      string
	NameStartsWith  string
	AnyProviderTmdb string
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

	items := []any{}
	for rows.Next() {
		var (
			id         int64
			tmdbID     db.NullString
			videoType  string
			title      string
			dateAir    sql.NullTime
			createdAt  time.Time
		)
		if err := rows.Scan(&id, &tmdbID, &videoType, &title, &dateAir, &createdAt); err != nil {
			return VideoListResult{}, err
		}
		isMovie := videoType == db.VideoTypeMovie
		rowID := emby.ItemID(emby.ItemIDTypeVideoList, id)
		item := map[string]any{
			"Name":        title,
			"ServerId":    t.cfg.EmbyID,
			"Id":          rowID,
			"DateCreated": emby.FormatTime(createdAt),
			"Path":        "/.strm",
			"Genres":      []any{},
			"People":      []any{},
			"GenreItems":  []any{},
			"ProductionYear": yearOf(dateAir),
			"ProviderIds": map[string]any{
				"Tmdb": tmdbID.String,
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
			"ImageTags": map[string]any{
				"Primary": rowID,
			},
			"BackdropImageTags": []any{},
			"MediaType":         "Video",
			"CanDelete":         false,
			"CanDownload":       false,
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return VideoListResult{}, err
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
