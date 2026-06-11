package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PivKeyU/Emotion/internal/cache"
	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// Sessions serves /Sessions/* endpoints. Live playback state is held in
// memory and finalized into playback_activity on stop / janitor sweep.
type Sessions struct {
	db    *db.DB
	cache cache.Cache
	cfg   *config.Config
	log   *slog.Logger

	mu       sync.Mutex
	live     map[string]*liveSession
	stopOnce sync.Once
	stop     chan struct{}
}

// liveSession is the in-memory record of one playing client.
type liveSession struct {
	UserID         int64
	ItemID         string
	ItemType       string
	ItemName       string
	Client         string
	DeviceName     string
	DeviceID       string
	RemoteAddress  string
	PlayMethod     string
	PlaySessionID  string
	StartedAt      time.Time
	LastProgressAt time.Time
	PausedAccum    time.Duration
	PausedSince    time.Time
	IsPaused       bool
}

// NewSessions builds the handler and starts the janitor goroutine.
func NewSessions(database *db.DB, c cache.Cache, cfg *config.Config, log *slog.Logger) *Sessions {
	s := &Sessions{
		db:    database,
		cache: c,
		cfg:   cfg,
		log:   log,
		live:  map[string]*liveSession{},
		stop:  make(chan struct{}),
	}
	go s.janitor()
	return s
}

// Close stops the janitor goroutine. Safe to call multiple times.
func (s *Sessions) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *Sessions) janitor() {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

// sweep finalizes any live session whose last progress was over an hour ago.
func (s *Sessions) sweep() {
	cutoff := time.Now().Add(-1 * time.Hour)
	var stale []*liveSession
	s.mu.Lock()
	for id, sess := range s.live {
		if sess.LastProgressAt.Before(cutoff) {
			stale = append(stale, sess)
			delete(s.live, id)
		}
	}
	s.mu.Unlock()
	for _, sess := range stale {
		stoppedAt := sess.LastProgressAt
		if stoppedAt.IsZero() {
			stoppedAt = time.Now()
		}
		s.recordActivity(context.Background(), sess, stoppedAt)
	}
}

// List returns the active live sessions, plus a synthetic server stub so
// clients that always expect at least one entry don't break.
func (s *Sessions) List(w http.ResponseWriter, r *http.Request) {
	out := []any{
		map[string]any{
			"PlayState": map[string]any{
				"CanSeek": false, "IsPaused": false, "IsMuted": false,
				"RepeatMode": "RepeatNone", "SleepTimerMode": "None",
				"SubtitleOffset": 0, "Shuffle": false, "PlaybackRate": 1,
			},
			"AdditionalUsers":       []any{},
			"RemoteEndPoint":        "emotion",
			"Protocol":              "HTTP/1.1",
			"PlayableMediaTypes":    []any{"Audio", "Video"},
			"PlaylistIndex":         0,
			"PlaylistLength":        0,
			"Id":                    s.cfg.EmbyID,
			"ServerId":              s.cfg.EmbyID,
			"UserId":                "",
			"UserName":              "",
			"Client":                "",
			"LastActivityDate":      emby.FormatTimeNow(),
			"DeviceName":            "",
			"InternalDeviceId":      0,
			"DeviceId":              "",
			"ApplicationVersion":    s.cfg.EmbyVersion,
			"AppIconUrl":            "",
			"SupportedCommands":     []any{},
			"SupportsRemoteControl": false,
		},
	}
	s.mu.Lock()
	for _, sess := range s.live {
		out = append(out, map[string]any{
			"PlayState": map[string]any{
				"CanSeek": true, "IsPaused": sess.IsPaused, "IsMuted": false,
				"RepeatMode": "RepeatNone", "SleepTimerMode": "None",
				"SubtitleOffset": 0, "Shuffle": false, "PlaybackRate": 1,
				"PlayMethod": sess.PlayMethod,
			},
			"AdditionalUsers":    []any{},
			"RemoteEndPoint":     sess.RemoteAddress,
			"Protocol":           "HTTP/1.1",
			"PlayableMediaTypes": []any{"Video"},
			"Id":                 sess.PlaySessionID,
			"ServerId":           s.cfg.EmbyID,
			"UserId":             itoa(sess.UserID),
			"UserName":           "",
			"Client":             sess.Client,
			"LastActivityDate":   sess.LastProgressAt.UTC().Format(time.RFC3339),
			"DeviceName":         sess.DeviceName,
			"DeviceId":           sess.DeviceID,
			"NowPlayingItem": map[string]any{
				"Id":   sess.ItemID,
				"Name": sess.ItemName,
				"Type": sess.ItemType,
			},
			"ApplicationVersion":    s.cfg.EmbyVersion,
			"SupportedCommands":     []any{},
			"SupportsRemoteControl": false,
		})
	}
	s.mu.Unlock()
	WriteJSON(w, http.StatusOK, out)
}

// Capabilities handles /Sessions/Capabilities/Full which clients POST at startup.
func (s *Sessions) Capabilities(w http.ResponseWriter, r *http.Request) {
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
type playingBody struct {
	ItemID        string `json:"itemid"`
	MediaSourceID string `json:"mediasourceid"`
	PositionTicks int64  `json:"positionticks"`
	IsPaused      bool   `json:"ispaused"`
	PlaySessionID string `json:"playsessionid"`
	PlayMethod    string `json:"playmethod"`
	EventName     string `json:"eventname"`
}

type playingMediaInfo struct {
	ID              int64 `json:"id"`
	EffectiveSecond int64 `json:"effective_second"`
	VideoListID     int64 `json:"video_list_id"`
	VideoSeasonID   int64 `json:"video_season_id"`
	VideoEpisodeID  int64 `json:"video_episode_id"`
}

// Playing records play / progress / stopped events.
func (s *Sessions) Playing(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteStatus(w, http.StatusForbidden)
		return
	}
	defer r.Body.Close()

	var mixed map[string]any
	if err := json.Unmarshal(raw, &mixed); err != nil {
		_ = json.Unmarshal([]byte(strings.ToLower(string(raw))), &mixed)
	}
	lowered := map[string]any{}
	for k, v := range mixed {
		lowered[strings.ToLower(k)] = v
	}
	normalized, _ := json.Marshal(lowered)

	var body playingBody
	_ = json.Unmarshal(normalized, &body)

	kind, numericID, ok := emby.ParseItemID(body.ItemID)
	if !ok {
		WriteStatus(w, http.StatusUnprocessableEntity)
		return
	}

	ctx := r.Context()
	userID := ctxpkg.UserID(ctx)

	mediaUUID := body.MediaSourceID
	if idx := strings.Index(mediaUUID, "_"); idx > 0 {
		mediaUUID = mediaUUID[:idx]
	}
	if mediaUUID == "" {
		mediaUUID = s.lookupMediaUUID(ctx, kind, numericID)
	}
	media, ok := s.lookupPlayingMedia(ctx, mediaUUID)
	if !ok {
		WriteStatus(w, http.StatusUnprocessableEntity)
		return
	}

	playSeconds := body.PositionTicks / emby.TicksPerSecond
	if playSeconds < 0 {
		playSeconds = 0
	}
	isComplete := false
	if media.EffectiveSecond > 0 && media.EffectiveSecond-playSeconds < 300 {
		isComplete = true
	}

	if err := s.upsertProgress(ctx, userID, kind, numericID, media, playSeconds, isComplete); err != nil {
		s.log.Warn("progress upsert failed", "category", "session", "err", err)
	}

	s.trackEvent(ctx, r, &body, kind, numericID, userID)

	WriteStatus(w, http.StatusNoContent)
}

func (s *Sessions) lookupMediaUUID(ctx context.Context, kind string, numericID int64) string {
	var col string
	switch kind {
	case emby.ItemIDTypeVideoList:
		col = "video_list_id"
	case emby.ItemIDTypeVideoEpisode:
		col = "video_episode_id"
	default:
		return ""
	}
	var uuid string
	_ = s.db.QueryRowContext(ctx,
		"SELECT uuid FROM video_media WHERE "+col+" = ? AND deleted_at IS NULL ORDER BY id ASC LIMIT 1",
		numericID,
	).Scan(&uuid)
	return uuid
}

func (s *Sessions) lookupPlayingMedia(ctx context.Context, mediaUUID string) (playingMediaInfo, bool) {
	if strings.TrimSpace(mediaUUID) == "" {
		return playingMediaInfo{}, false
	}
	cacheKey := "playing_media_" + mediaUUID
	if cached, ok := s.cache.Get(ctx, cacheKey); ok {
		var c playingMediaInfo
		if err := json.Unmarshal([]byte(cached), &c); err == nil && c.ID > 0 {
			return c, true
		}
	}

	var m playingMediaInfo
	err := s.db.QueryRowContext(ctx, `
		SELECT
			vm.id,
			COALESCE(NULLIF(vm.file_second, 0), ve.runtime * 60, vl.runtime * 60, 0) AS effective_second,
			vm.video_list_id,
			COALESCE(vm.video_season_id, 0),
			COALESCE(vm.video_episode_id, 0)
		FROM video_media vm
		JOIN video_list vl ON vl.id = vm.video_list_id
		LEFT JOIN video_episode ve ON ve.id = vm.video_episode_id
		WHERE vm.uuid = ? AND vm.deleted_at IS NULL
		LIMIT 1
	`, mediaUUID).Scan(&m.ID, &m.EffectiveSecond, &m.VideoListID, &m.VideoSeasonID, &m.VideoEpisodeID)
	if err != nil || m.ID <= 0 || m.VideoListID <= 0 {
		return playingMediaInfo{}, false
	}
	payload, _ := json.Marshal(m)
	s.cache.Set(ctx, cacheKey, string(payload), time.Hour)
	return m, true
}

func (s *Sessions) upsertProgress(ctx context.Context, userID int64, kind string, numericID int64, media playingMediaInfo, playSeconds int64, isComplete bool) error {
	videoListID := media.VideoListID
	videoSeasonID := media.VideoSeasonID
	videoEpisodeID := media.VideoEpisodeID

	if kind == emby.ItemIDTypeVideoEpisode && videoEpisodeID == 0 {
		videoEpisodeID = numericID
		_ = s.db.QueryRowContext(ctx,
			"SELECT video_list_id, COALESCE(video_season_id, 0) FROM video_episode WHERE id = ? LIMIT 1",
			numericID,
		).Scan(&videoListID, &videoSeasonID)
	}
	if videoListID <= 0 {
		return nil
	}

	where := "user_id = ? AND video_list_id = ? AND video_episode_id IS NULL"
	whereArgs := []any{userID, videoListID}
	if videoEpisodeID > 0 {
		where = "user_id = ? AND video_episode_id = ?"
		whereArgs = []any{userID, videoEpisodeID}
	}

	updateArgs := []any{
		videoListID,
		nullableProgressID(videoSeasonID),
		nullableProgressID(videoEpisodeID),
		playSeconds,
		media.ID,
		isComplete,
	}
	updateArgs = append(updateArgs, whereArgs...)
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_video_record
		SET video_list_id = ?, video_season_id = ?, video_episode_id = ?,
		    play_seconds = ?, video_media_id = ?, is_complete = ?, updated_at = NOW()
		WHERE `+where, updateArgs...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO user_video_record
			(video_list_id, video_season_id, video_episode_id, video_media_id,
			 play_seconds, is_complete, user_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NOW())
	`, videoListID, nullableProgressID(videoSeasonID), nullableProgressID(videoEpisodeID),
		media.ID, playSeconds, isComplete, userID)
	return err
}

func nullableProgressID(v int64) db.NullInt64 {
	if v <= 0 {
		return db.NullInt64{}
	}
	return db.NullInt64{Valid: true, Int64: v}
}

// trackEvent updates the in-memory session for this client and, on stop,
// writes a row into playback_activity.
func (s *Sessions) trackEvent(ctx context.Context, r *http.Request, body *playingBody, kind string, numericID, userID int64) {
	psid := body.PlaySessionID
	if psid == "" {
		psid = body.ItemID
	}
	now := time.Now()
	isStop := isPlaybackStopEvent(r, body.EventName)

	s.mu.Lock()
	sess, ok := s.live[psid]
	if !ok {
		sess = &liveSession{
			UserID:         userID,
			ItemID:         body.ItemID,
			ItemType:       embyItemTypeFromKind(kind),
			Client:         ctxpkg.Client(ctx),
			DeviceName:     ctxpkg.DeviceName(ctx),
			DeviceID:       ctxpkg.DeviceID(ctx),
			RemoteAddress:  ctxpkg.RemoteAddr(ctx),
			PlayMethod:     body.PlayMethod,
			PlaySessionID:  psid,
			StartedAt:      now,
			LastProgressAt: now,
		}
		s.live[psid] = sess
	}
	if sess.PlayMethod == "" && body.PlayMethod != "" {
		sess.PlayMethod = body.PlayMethod
	}
	if !sess.IsPaused && body.IsPaused {
		sess.PausedSince = now
	} else if sess.IsPaused && !body.IsPaused && !sess.PausedSince.IsZero() {
		sess.PausedAccum += now.Sub(sess.PausedSince)
		sess.PausedSince = time.Time{}
	}
	sess.IsPaused = body.IsPaused
	sess.LastProgressAt = now

	if isStop {
		if sess.IsPaused && !sess.PausedSince.IsZero() {
			sess.PausedAccum += now.Sub(sess.PausedSince)
			sess.PausedSince = time.Time{}
			sess.IsPaused = false
		}
		delete(s.live, psid)
	}
	finalize := isStop
	captured := *sess
	s.mu.Unlock()

	if sess.ItemName == "" {
		captured.ItemName = s.lookupItemName(ctx, kind, numericID)
		s.mu.Lock()
		if cur, ok := s.live[psid]; ok {
			cur.ItemName = captured.ItemName
		}
		s.mu.Unlock()
	}

	if finalize {
		s.recordActivity(ctx, &captured, now)
	}
}

func (s *Sessions) lookupItemName(ctx context.Context, kind string, numericID int64) string {
	var name string
	switch kind {
	case emby.ItemIDTypeVideoList:
		_ = s.db.QueryRowContext(ctx,
			"SELECT title FROM video_list WHERE id = ? LIMIT 1", numericID,
		).Scan(&name)
	case emby.ItemIDTypeVideoEpisode:
		var seriesTitle, epTitle string
		_ = s.db.QueryRowContext(ctx, `
			SELECT vl.title, ve.title FROM video_episode ve
			JOIN video_list vl ON vl.id = ve.video_list_id
			WHERE ve.id = ? LIMIT 1`, numericID,
		).Scan(&seriesTitle, &epTitle)
		if seriesTitle != "" && epTitle != "" {
			name = seriesTitle + " - " + epTitle
		} else if seriesTitle != "" {
			name = seriesTitle
		} else {
			name = epTitle
		}
	}
	return name
}

func (s *Sessions) recordActivity(ctx context.Context, sess *liveSession, stoppedAt time.Time) {
	playDur, pauseDur := activityDurations(sess, stoppedAt)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO playback_activity
			(date_created, user_id, item_id, item_type, item_name, play_method,
			 client, device_name, remote_address, play_duration, pause_duration)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		sess.StartedAt,
		itoa(sess.UserID),
		sess.ItemID,
		sess.ItemType,
		sess.ItemName,
		sess.PlayMethod,
		sess.Client,
		sess.DeviceName,
		sess.RemoteAddress,
		playDur,
		pauseDur,
	)
	if err != nil {
		s.log.Warn("playback_activity insert failed", "category", "session", "err", err)
	}
}

func isPlaybackStopEvent(r *http.Request, eventName string) bool {
	event := strings.ToLower(strings.TrimSpace(eventName))
	if event == "playbackstopped" || event == "stopped" || event == "playbackstop" {
		return true
	}
	if r == nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(r.URL.Path), "/sessions/playing/stopped")
}

func activityDurations(sess *liveSession, stoppedAt time.Time) (playDur, pauseDur int64) {
	playDur = int64(stoppedAt.Sub(sess.StartedAt).Seconds())
	pause := sess.PausedAccum
	if sess.IsPaused && !sess.PausedSince.IsZero() && stoppedAt.After(sess.PausedSince) {
		pause += stoppedAt.Sub(sess.PausedSince)
	}
	pauseDur = int64(pause.Seconds())
	if playDur < 0 {
		playDur = 0
	}
	if pauseDur < 0 {
		pauseDur = 0
	}
	if pauseDur > playDur {
		pauseDur = playDur
	}
	return playDur, pauseDur
}

func embyItemTypeFromKind(kind string) string {
	switch kind {
	case emby.ItemIDTypeVideoList:
		return "Movie"
	case emby.ItemIDTypeVideoEpisode:
		return "Episode"
	case emby.ItemIDTypeVideoSeason:
		return "Season"
	case emby.ItemIDTypeVideoLibrary:
		return "CollectionFolder"
	}
	return ""
}
