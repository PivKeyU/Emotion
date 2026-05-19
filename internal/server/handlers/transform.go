package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
)

// Transform builds Emby-shaped JSON objects from database rows.
// Pure data transformation - no HTTP handling.
type Transform struct {
	db  *db.DB
	cfg *config.Config
}

// NewTransform constructs a Transform service.
func NewTransform(database *db.DB, cfg *config.Config) *Transform {
	return &Transform{db: database, cfg: cfg}
}

// UserFolders returns the list of library ids a user may see.
func (t *Transform) UserFolders(ctx context.Context, userID int64) ([]int64, error) {
	if userID <= 0 {
		return t.AllLibraryIDs(ctx)
	}
	var folders db.NullString
	err := t.db.QueryRowContext(ctx,
		"SELECT folders FROM app_user WHERE id = ? LIMIT 1", userID,
	).Scan(&folders)
	if err != nil {
		return nil, err
	}
	if !folders.Valid || folders.String == "" {
		return t.AllLibraryIDs(ctx)
	}
	var out []int64
	if err := json.Unmarshal([]byte(folders.String), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AllLibraryIDs returns every non-deleted library id in display order.
func (t *Transform) AllLibraryIDs(ctx context.Context) ([]int64, error) {
	rows, err := t.db.QueryContext(ctx, "SELECT id FROM library WHERE deleted_at IS NULL ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// User builds the Emby user object (at /Users/{id}).
// Includes Policy and Configuration because Emby clients rely on them.
func (t *Transform) User(ctx context.Context, userID int64) (map[string]any, error) {
	if userID <= 0 {
		return t.pseudoAdminUser(ctx)
	}
	var (
		username  db.NullString
		isCanDown db.NullBool
		isAdmin   db.NullBool
		folders   db.NullString
	)
	err := t.db.QueryRowContext(ctx,
		"SELECT username, is_can_down, is_admin, folders FROM app_user WHERE id = ? LIMIT 1", userID,
	).Scan(&username, &isCanDown, &isAdmin, &folders)
	if err != nil {
		return nil, err
	}

	var orderedViews []string
	ids, _ := t.UserFolders(ctx, userID)
	for _, id := range ids {
		orderedViews = append(orderedViews, emby.ItemID(emby.ItemIDTypeVideoLibrary, id))
	}
	enableAllFolders := !folders.Valid || strings.TrimSpace(folders.String) == ""
	if folders.Valid && strings.TrimSpace(folders.String) != "" {
		var explicitIDs []int64
		if err := json.Unmarshal([]byte(folders.String), &explicitIDs); err == nil {
			enableAllFolders = false
		}
	}

	now := emby.FormatTimeNow()
	embyID := strconv.FormatInt(userID, 10)

	return map[string]any{
		"Name":                  username.String,
		"ServerId":              t.cfg.EmbyID,
		"Prefix":                "E",
		"DateCreated":           now,
		"Id":                    embyID,
		"HasPassword":           true,
		"HasConfiguredPassword": true,
		"LastLoginDate":         now,
		"LastActivityDate":      now,
		"Configuration": map[string]any{
			"AudioLanguagePreference":    "",
			"PlayDefaultAudioTrack":      true,
			"DisplayMissingEpisodes":     false,
			"SubtitleMode":               "Smart",
			"OrderedViews":               orderedViews,
			"LatestItemsExcludes":        []any{},
			"SearchExcludes":             []any{},
			"MyMediaExcludes":            []any{},
			"HidePlayedInLatest":         false,
			"HidePlayedInMoreLikeThis":   false,
			"HidePlayedInSuggestions":    false,
			"RememberAudioSelections":    false,
			"RememberSubtitleSelections": false,
			"EnableNextEpisodeAutoPlay":  false,
			"ResumeRewindSeconds":        0,
			"IntroSkipMode":              "None",
			"EnableLocalPassword":        false,
		},
		"Policy": map[string]any{
			"IsAdministrator":                  isAdmin.Bool,
			"IsHidden":                         true,
			"IsHiddenRemotely":                 true,
			"IsHiddenFromUnusedDevices":        true,
			"IsDisabled":                       false,
			"LockedOutDate":                    0,
			"AllowTagOrRating":                 false,
			"BlockedTags":                      []any{},
			"IsTagBlockingModeInclusive":       false,
			"IncludeTags":                      []any{},
			"EnableUserPreferenceAccess":       false,
			"AccessSchedules":                  []any{},
			"BlockUnratedItems":                []any{},
			"EnableRemoteControlOfOtherUsers":  false,
			"EnableSharedDeviceControl":        false,
			"EnableRemoteAccess":               true,
			"EnableLiveTvManagement":           false,
			"EnableLiveTvAccess":               false,
			"EnableMediaPlayback":              true,
			"EnableAudioPlaybackTranscoding":   false,
			"EnableVideoPlaybackTranscoding":   false,
			"EnablePlaybackRemuxing":           false,
			"EnableContentDeletion":            false,
			"RestrictedFeatures":               []any{},
			"EnableContentDeletionFromFolders": []any{},
			"EnableContentDownloading":         isCanDown.Bool,
			"EnableSubtitleDownloading":        isCanDown.Bool,
			"EnableSubtitleManagement":         false,
			"EnableSyncTranscoding":            false,
			"EnableMediaConversion":            false,
			"EnabledChannels":                  []any{},
			"EnableAllChannels":                true,
			"EnabledFolders":                   orderedViews,
			"EnableAllFolders":                 enableAllFolders,
			"InvalidLoginAttemptCount":         0,
			"EnablePublicSharing":              false,
			"RemoteClientBitrateLimit":         0,
			"AuthenticationProviderId":         "emotion",
			"ExcludedSubFolders":               []any{},
			"SimultaneousStreamLimit":          0,
			"EnabledDevices":                   []any{},
			"EnableAllDevices":                 true,
			"AllowCameraUpload":                false,
			"AllowSharingPersonalItems":        false,
		},
		"HasConfiguredEasyPassword": false,
	}, nil
}

func (t *Transform) pseudoAdminUser(ctx context.Context) (map[string]any, error) {
	ids, err := t.AllLibraryIDs(ctx)
	if err != nil {
		return nil, err
	}
	orderedViews := make([]string, 0, len(ids))
	for _, id := range ids {
		orderedViews = append(orderedViews, emby.ItemID(emby.ItemIDTypeVideoLibrary, id))
	}
	now := emby.FormatTimeNow()
	return map[string]any{
		"Name":                  "admin",
		"ServerId":              t.cfg.EmbyID,
		"Prefix":                "E",
		"DateCreated":           now,
		"Id":                    "0",
		"HasPassword":           false,
		"HasConfiguredPassword": false,
		"LastLoginDate":         now,
		"LastActivityDate":      now,
		"Configuration": map[string]any{
			"OrderedViews":        orderedViews,
			"LatestItemsExcludes": []any{},
			"SearchExcludes":      []any{},
			"MyMediaExcludes":     []any{},
		},
		"Policy": map[string]any{
			"IsAdministrator":          true,
			"IsDisabled":               false,
			"EnableContentDownloading": true,
			"EnableMediaPlayback":      true,
			"EnableAllFolders":         true,
			"EnabledFolders":           orderedViews,
			"EnableAllDevices":         true,
			"EnableAllChannels":        true,
			"BlockedMediaFolders":      []any{},
			"SimultaneousStreamLimit":  0,
		},
		"HasConfiguredEasyPassword": false,
	}, nil
}

// GetVideoListTitle returns the display title for a video_list row.
func (t *Transform) GetVideoListTitle(ctx context.Context, videoListID int64) string {
	var title string
	err := t.db.QueryRowContext(ctx,
		"SELECT title FROM video_list WHERE id = ? LIMIT 1", videoListID,
	).Scan(&title)
	if err != nil {
		return ""
	}
	return title
}

// UserVideoRecord is the playback progress summary (emya formatUserVideoRecord).
type UserVideoRecord struct {
	PlayMs     int64
	IsComplete bool
	Percentage float64
}

// FormatUserVideoRecord converts raw play seconds into the Emby UserData shape.
func (t *Transform) FormatUserVideoRecord(playSeconds int64, isComplete bool, mediaSecond int64) UserVideoRecord {
	out := UserVideoRecord{}
	out.PlayMs = playSeconds * emby.TicksPerSecond
	out.IsComplete = isComplete
	if mediaSecond > 0 && playSeconds > 0 {
		out.Percentage = float64(playSeconds) / float64(mediaSecond) * 100
	}
	return out
}

// GetUserVideoRecord loads and formats a specific user's progress for a list or episode.
// Either videoListID or videoEpisodeID may be 0 to omit the filter.
func (t *Transform) GetUserVideoRecord(ctx context.Context, userID, videoListID, videoEpisodeID int64) (UserVideoRecord, error) {
	query := "SELECT play_seconds, is_complete, video_media_id FROM user_video_record WHERE user_id = ?"
	args := []any{userID}
	if videoListID > 0 {
		query += " AND video_list_id = ?"
		args = append(args, videoListID)
	}
	if videoEpisodeID > 0 {
		query += " AND video_episode_id = ?"
		args = append(args, videoEpisodeID)
	}
	query += " LIMIT 1"

	var (
		playSeconds  db.NullInt64
		isComplete   db.NullBool
		videoMediaID db.NullInt64
	)
	err := t.db.QueryRowContext(ctx, query, args...).Scan(&playSeconds, &isComplete, &videoMediaID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserVideoRecord{}, nil
		}
		return UserVideoRecord{}, err
	}

	var fileSecond int64
	if videoMediaID.Valid {
		var fs db.NullInt64
		_ = t.db.QueryRowContext(ctx,
			"SELECT file_second FROM video_media WHERE id = ? LIMIT 1", videoMediaID.Int64,
		).Scan(&fs)
		fileSecond = fs.Int64
	}

	return t.FormatUserVideoRecord(playSeconds.Int64, isComplete.Bool, fileSecond), nil
}

// GetUserLibrary returns the list of libraries (CollectionFolder) the user can see.
func (t *Transform) GetUserLibrary(ctx context.Context, userID int64) ([]any, error) {
	folders, err := t.UserFolders(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(folders) == 0 {
		return []any{}, nil
	}

	placeholders := ""
	args := make([]any, 0, len(folders))
	for i, id := range folders {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}

	rows, err := t.db.QueryContext(ctx,
		fmt.Sprintf("SELECT id, name, COALESCE(role, ''), COALESCE(root_path, '') FROM library WHERE id IN (%s) ORDER BY id ASC", placeholders),
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []any
	for rows.Next() {
		var id int64
		var name, role, root string
		if err := rows.Scan(&id, &name, &role, &root); err != nil {
			return nil, err
		}
		root = libraryRootOrFallback(name, root)
		libID := emby.ItemID(emby.ItemIDTypeVideoLibrary, id)
		childCount, recursiveItemCount := t.libraryCounts(ctx, id)
		out = append(out, map[string]any{
			"Name":                  name,
			"ServerId":              t.cfg.EmbyID,
			"Id":                    libID,
			"Guid":                  libID,
			"Etag":                  libID,
			"DateCreated":           emby.DefaultTime,
			"DateModified":          emby.DefaultTime,
			"CanDelete":             false,
			"CanDownload":           false,
			"PresentationUniqueKey": libID,
			"SortName":              name,
			"ForcedSortName":        name,
			"ExternalUrls":          []any{},
			"Taglines":              []any{},
			"RemoteTrailers":        []any{},
			"ProviderIds":           map[string]any{},
			"IsFolder":              true,
			"ParentId":              "0",
			"Type":                  "CollectionFolder",
			"CollectionType":        embyCollectionType(role),
			"Path":                  root,
			"UserData": map[string]any{
				"PlaybackPositionTicks": 0,
				"IsFavorite":            false,
				"Played":                false,
			},
			"ChildCount":              childCount,
			"RecursiveItemCount":      recursiveItemCount,
			"DisplayPreferencesId":    libID,
			"PrimaryImageAspectRatio": 1,
			"ImageTags": map[string]any{
				"Primary": libID,
			},
			"BackdropImageTags": []any{},
			"LockedFields":      []any{},
			"LockData":          false,
		})
	}
	return out, rows.Err()
}

func (t *Transform) libraryCounts(ctx context.Context, libraryID int64) (childCount int64, recursiveItemCount int64) {
	_ = t.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM video_list WHERE video_library_id = ? AND deleted_at IS NULL",
		libraryID,
	).Scan(&childCount)
	_ = t.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM video_episode ve
		JOIN video_list vl ON vl.id = ve.video_list_id
		WHERE vl.video_library_id = ?
		  AND vl.deleted_at IS NULL
		  AND ve.deleted_at IS NULL`,
		libraryID,
	).Scan(&recursiveItemCount)
	recursiveItemCount += childCount
	return childCount, recursiveItemCount
}

func embyCollectionType(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case db.VideoTypeMovie, "movies":
		return "movies"
	case db.VideoTypeTV, "series", "tvshows":
		return "tvshows"
	default:
		return ""
	}
}
