package handlers

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/PivKeyU/Emotion/internal/cache"
	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// Sessions serves /Sessions/* endpoints. We report a single synthetic session
// for the server (emby clients require at least one), and record playback
// progress / stops from the client.
type Sessions struct {
	db    *sql.DB
	cache cache.Cache
	cfg   *config.Config
	log   *slog.Logger
}

// NewSessions builds the handler.
func NewSessions(database *sql.DB, c cache.Cache, cfg *config.Config, log *slog.Logger) *Sessions {
	return &Sessions{db: database, cache: c, cfg: cfg, log: log}
}

// List returns the current set of sessions.
// emya returns a single stub session; real Emby would track each client. We follow emya.
func (s *Sessions) List(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, []any{
		map[string]any{
			"PlayState": map[string]any{
				"CanSeek":        false,
				"IsPaused":       false,
				"IsMuted":        false,
				"RepeatMode":     "RepeatNone",
				"SleepTimerMode": "None",
				"SubtitleOffset": 0,
				"Shuffle":        false,
				"PlaybackRate":   1,
			},
			"AdditionalUsers":     []any{},
			"RemoteEndPoint":      "emotion",
			"Protocol":            "HTTP/1.1",
			"PlayableMediaTypes":  []any{"Audio", "Video"},
			"PlaylistIndex":       0,
			"PlaylistLength":      0,
			"Id":                  s.cfg.EmbyID,
			"ServerId":            s.cfg.EmbyID,
			"UserId":              "",
			"UserName":            "",
			"Client":              "",
			"LastActivityDate":    emby.FormatTimeNow(),
			"DeviceName":          "",
			"InternalDeviceId":    0,
			"DeviceId":            "",
			"ApplicationVersion":  s.cfg.EmbyVersion,
			"AppIconUrl":          "",
			"SupportedCommands":   []any{},
			"SupportsRemoteControl": false,
		},
	})
}

// Capabilities handles /Sessions/Capabilities/Full which clients POST at startup.
func (s *Sessions) Capabilities(w http.ResponseWriter, r *http.Request) {
	// Discard body; we don't model per-session capabilities.
	_, _ = io.Copy(io.Discard, r.Body)
	defer r.Body.Close()
	WriteStatus(w, http.StatusNoContent)
}

// Ping handles /Sessions/Playing/Ping (keep-alive from a playing client).
func (s *Sessions) Ping(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	defer r.Body.Close()
	WriteStatus(w, http.StatusNoContent)
}

// playingBody is a lenient decoder for all the progress/start/stop payloads.
// emya lower-cases all body keys, so we do the same by normalizing in code.
type playingBody struct {
	ItemID           string `json:"itemid"`
	MediaSourceID    string `json:"mediasourceid"`
	PositionTicks    int64  `json:"positionticks"`
	IsPaused         bool   `json:"ispaused"`
	PlaySessionID    string `json:"playsessionid"`
	PlayMethod       string `json:"playmethod"`
	EventName        string `json:"eventname"`
}

// Playing records play / progress / stopped events.
// Same endpoint handles all three because emya does the same.
func (s *Sessions) Playing(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteStatus(w, http.StatusForbidden)
		return
	}
	defer r.Body.Close()

	// Decode case-insensitively by lower-casing keys first.
	var mixed map[string]any
	if err := json.Unmarshal(raw, &mixed); err != nil {
		// Some clients (e.g. Cinetry) may send a stringified JSON body.
		_ = json.Unmarshal([]byte(strings.ToLower(string(raw))), &mixed)
	}
	lowered := map[string]any{}
	for k, v := range mixed {
		lowered[strings.ToLower(k)] = v
	}
	normalized, _ := json.Marshal(lowered)

	var body playingBody
	_ = json.Unmarshal(normalized, &body)

	_, numericID, ok := emby.ParseItemID(body.ItemID)
	if !ok {
		WriteStatus(w, http.StatusUnprocessableEntity)
		return
	}

	kind, _, _ := emby.ParseItemID(body.ItemID)

	ctx := r.Context()
	userID := ctxpkg.UserID(ctx)

	// Find the media row by uuid (the prefix of MediaSourceId before "_").
	mediaUUID := body.MediaSourceID
	if idx := strings.Index(mediaUUID, "_"); idx > 0 {
		mediaUUID = mediaUUID[:idx]
	}
	if mediaUUID == "" {
		// Nothing to record at stream level, but still return 204 so clients don't retry.
		WriteStatus(w, http.StatusNoContent)
		return
	}

	// Look up media (cached).
	var (
		mediaID    int64
		fileSecond db.NullInt64
	)
	cacheKey := "playing_media_" + mediaUUID
	if cached, ok := s.cache.Get(ctx, cacheKey); ok {
		var c struct {
			ID         int64 `json:"id"`
			FileSecond int64 `json:"file_second"`
		}
		_ = json.Unmarshal([]byte(cached), &c)
		mediaID = c.ID
		fileSecond.Valid = true
		fileSecond.Int64 = c.FileSecond
	} else {
		err := s.db.QueryRowContext(ctx,
			"SELECT id, file_second FROM video_media WHERE uuid = ? LIMIT 1", mediaUUID,
		).Scan(&mediaID, &fileSecond)
		if err == nil && mediaID > 0 {
			payload, _ := json.Marshal(map[string]any{"id": mediaID, "file_second": fileSecond.Int64})
			s.cache.Set(ctx, cacheKey, string(payload), time.Hour)
		}
	}
	if mediaID == 0 {
		WriteStatus(w, http.StatusUnprocessableEntity)
		return
	}

	// Build WHERE clause for the target record. Mirrors emya logic:
	// user_id is always required; then add video_list_id OR video_episode_id
	// depending on the Emby item kind.
	wheres := []string{"user_id = ?"}
	args := []any{userID}
	switch kind {
	case emby.ItemIDTypeVideoList:
		wheres = append(wheres, "video_list_id = ?")
		args = append(args, numericID)
	case emby.ItemIDTypeVideoEpisode:
		wheres = append(wheres, "video_episode_id = ?")
		args = append(args, numericID)
	}

	playSeconds := body.PositionTicks / emby.TicksPerSecond
	if playSeconds < 0 {
		playSeconds = 0
	}
	isComplete := false
	if fileSecond.Valid && fileSecond.Int64 > 0 && fileSecond.Int64-playSeconds < 300 {
		isComplete = true
	}

	stmt := "UPDATE user_video_record SET play_seconds = ?, video_media_id = ?, is_complete = ? WHERE " + strings.Join(wheres, " AND ")
	fullArgs := append([]any{playSeconds, mediaID, isComplete}, args...)
	_, _ = s.db.ExecContext(ctx, stmt, fullArgs...)

	WriteStatus(w, http.StatusNoContent)
}
