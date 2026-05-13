package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
)

// Transform builds Emby-shaped JSON objects from database rows.
// Pure data transformation - no HTTP handling.
type Transform struct {
	db  *sql.DB
	cfg *config.Config
}

// NewTransform constructs a Transform service.
func NewTransform(database *sql.DB, cfg *config.Config) *Transform {
	return &Transform{db: database, cfg: cfg}
}

// UserFolders returns the list of library ids a user may see.
func (t *Transform) UserFolders(ctx context.Context, userID int64) ([]int64, error) {
	var folders db.NullString
	err := t.db.QueryRowContext(ctx,
		"SELECT folders FROM user WHERE id = ? LIMIT 1", userID,
	).Scan(&folders)
	if err != nil {
		return nil, err
	}
	if !folders.Valid || folders.String == "" {
		return nil, nil
	}
	var out []int64
	if err := json.Unmarshal([]byte(folders.String), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// User builds the Emby user object (at /Users/{id}).
// Includes Policy and Configuration because Emby clients rely on them.
func (t *Transform) User(ctx context.Context, userID int64) (map[string]any, error) {
	var (
		username  db.NullString
		isCanDown db.NullBool
		isAdmin   db.NullBool
		folders   db.NullString
	)
	err := t.db.QueryRowContext(ctx,
		"SELECT username, is_can_down, is_admin, folders FROM user WHERE id = ? LIMIT 1", userID,
	).Scan(&username, &isCanDown, &isAdmin, &folders)
	if err != nil {
		return nil, err
	}

	var orderedViews []string
	if folders.Valid && folders.String != "" {
		var ids []int64
		_ = json.Unmarshal([]byte(folders.String), &ids)
		for _, id := range ids {
			orderedViews = append(orderedViews, strconv.FormatInt(id, 10))
		}
	}

	now := emby.FormatTimeNow()
	embyID := strconv.FormatInt(userID, 10)

	return map[string]any{
		"Name":                     username.String,
		"ServerId":                 t.cfg.EmbyID,
		"Prefix":                   "E",
		"DateCreated":              now,
		"Id":                       embyID,
		"HasPassword":              true,
		"HasConfiguredPassword":    true,
		"LastLoginDate":            now,
		"LastActivityDate":         now,
		"Configuration": map[string]any{
			"AudioLanguagePreference":     "",
			"PlayDefaultAudioTrack":       true,
			"DisplayMissingEpisodes":      false,
			"SubtitleMode":                "Smart",
			"OrderedViews":                orderedViews,
			"LatestItemsExcludes":         []any{},
			"SearchExcludes":              []any{},
			"MyMediaExcludes":             []any{},
			"HidePlayedInLatest":          false,
			"HidePlayedInMoreLikeThis":    false,
			"HidePlayedInSuggestions":     false,
			"RememberAudioSelections":     false,
			"RememberSubtitleSelections":  false,
			"EnableNextEpisodeAutoPlay":   false,
			"ResumeRewindSeconds":         0,
			"IntroSkipMode":               "None",
			"EnableLocalPassword":         false,
		},
		"Policy": map[string]any{
			"IsAdministrator":                      isAdmin.Bool,
			"IsHidden":                             true,
			"IsHiddenRemotely":                     true,
			"IsHiddenFromUnusedDevices":            true,
			"IsDisabled":                           false,
			"LockedOutDate":                        0,
			"AllowTagOrRating":                     false,
			"BlockedTags":                          []any{},
			"IsTagBlockingModeInclusive":           false,
			"IncludeTags":                          []any{},
			"EnableUserPreferenceAccess":           false,
			"AccessSchedules":                      []any{},
			"BlockUnratedItems":                    []any{},
			"EnableRemoteControlOfOtherUsers":      false,
			"EnableSharedDeviceControl":            false,
			"EnableRemoteAccess":                   true,
			"EnableLiveTvManagement":               false,
			"EnableLiveTvAccess":                   false,
			"EnableMediaPlayback":                  true,
			"EnableAudioPlaybackTranscoding":       false,
			"EnableVideoPlaybackTranscoding":       false,
			"EnablePlaybackRemuxing":               false,
			"EnableContentDeletion":                false,
			"RestrictedFeatures":                   []any{},
			"EnableContentDeletionFromFolders":     []any{},
			"EnableContentDownloading":             isCanDown.Bool,
			"EnableSubtitleDownloading":            isCanDown.Bool,
			"EnableSubtitleManagement":             false,
			"EnableSyncTranscoding":                false,
			"EnableMediaConversion":                false,
			"EnabledChannels":                      []any{},
			"EnableAllChannels":                    true,
			"EnabledFolders":                       []any{},
			"EnableAllFolders":                     true,
			"InvalidLoginAttemptCount":             0,
			"EnablePublicSharing":                  false,
			"RemoteClientBitrateLimit":             0,
			"AuthenticationProviderId":             "emotion",
			"ExcludedSubFolders":                   []any{},
			"SimultaneousStreamLimit":              0,
			"EnabledDevices":                       []any{},
			"EnableAllDevices":                     true,
			"AllowCameraUpload":                    false,
			"AllowSharingPersonalItems":            false,
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
		fmt.Sprintf("SELECT id, name FROM library WHERE id IN (%s) ORDER BY id ASC", placeholders),
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []any
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		libID := emby.ItemID(emby.ItemIDTypeVideoLibrary, id)
		out = append(out, map[string]any{
			"Name":                     name,
			"ServerId":                 t.cfg.EmbyID,
			"Id":                       libID,
			"Guid":                     libID,
			"Etag":                     libID,
			"DateCreated":              emby.DefaultTime,
			"DateModified":             emby.DefaultTime,
			"CanDelete":                false,
			"CanDownload":              false,
			"PresentationUniqueKey":    libID,
			"SortName":                 name,
			"ForcedSortName":           name,
			"ExternalUrls":             []any{},
			"Taglines":                 []any{},
			"RemoteTrailers":           []any{},
			"ProviderIds":              map[string]any{},
			"IsFolder":                 true,
			"ParentId":                 "0",
			"Type":                     "CollectionFolder",
			"UserData": map[string]any{
				"PlaybackPositionTicks": 0,
				"IsFavorite":            false,
				"Played":                false,
			},
			"ChildCount":             1,
			"DisplayPreferencesId":   libID,
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
