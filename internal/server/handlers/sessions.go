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
		s.recordActivity(context.Background(), sess, time.Now())
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
			"AdditionalUsers":       []any{},
			"RemoteEndPoint":        sess.RemoteAddress,
			"Protocol":              "HTTP/1.1",
			"PlayableMediaTypes":    []any{"Video"},
			"Id":                    sess.PlaySessionID,
			"ServerId":              s.cfg.EmbyID,
			"UserId":                itoa(sess.UserID),
			"UserName":              "",
			"Client":                sess.Client,
			"LastActivityDate":      sess.LastProgressAt.UTC().Format(time.RFC3339),
			"DeviceName":            sess.DeviceName,
			"DeviceId":              sess.DeviceID,
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
		WriteStatus(w, http.StatusNoContent)
		return
	}

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

	s.trackEvent(ctx, &body, kind, numericID, userID)

	WriteStatus(w, http.StatusNoContent)
}

// trackEvent updates the in-memory session for this client and, on stop,
// writes a row into playback_activity.
func (s *Sessions) trackEvent(ctx context.Context, body *playingBody, kind string, numericID, userID int64) {
	psid := body.PlaySessionID
	if psid == "" {
		psid = body.ItemID
	}
	now := time.Now()
	event := strings.ToLower(strings.TrimSpace(body.EventName))
	isStop := event == "playbackstopped" || event == "stopped" || event == "playbackstop"

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
	playDur := int64(stoppedAt.Sub(sess.StartedAt).Seconds())
	pauseDur := int64(sess.PausedAccum.Seconds())
	if playDur < 0 {
		playDur = 0
	}
	if pauseDur < 0 {
		pauseDur = 0
	}
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
