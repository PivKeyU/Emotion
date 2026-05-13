package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Next-Emby/internal/auth"
	"github.com/PivKeyU/Next-Emby/internal/config"
	"github.com/PivKeyU/Next-Emby/internal/db"
	"github.com/PivKeyU/Next-Emby/internal/emby"
	"github.com/PivKeyU/Next-Emby/internal/external"
	"github.com/PivKeyU/Next-Emby/internal/server/ctxpkg"
)

// Users serves /Users/* endpoints.
type Users struct {
	db        *sql.DB
	cfg       *config.Config
	log       *slog.Logger
	transform *Transform
	ext       *external.Client
}

// NewUsers builds the handler.
func NewUsers(database *sql.DB, cfg *config.Config, log *slog.Logger) *Users {
	return &Users{
		db:        database,
		cfg:       cfg,
		log:       log,
		transform: NewTransform(database, cfg),
		ext:       external.NewClient(cfg.APIExternal, cfg.APIKey),
	}
}

// Public returns [] (Emby public users endpoint is always empty here).
func (u *Users) Public(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, []any{})
}

// AuthenticateByName accepts a username/password and returns a SessionInfo + AccessToken.
// Mirrors emya users.controller.ts /AuthenticateByName.
func (u *Users) AuthenticateByName(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteText(w, http.StatusBadRequest, "bad body")
		return
	}
	defer r.Body.Close()

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) == 0 {
		WriteText(w, http.StatusBadRequest, "bad body")
		return
	}

	// Normalize keys to lowercase so Pw / pw / PW all work.
	p := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			p[strings.ToLower(k)] = s
		}
	}

	username := strings.TrimSpace(p["username"])
	if username == "" {
		username = strings.TrimSpace(p["name"])
	}
	password := p["pw"]
	if password == "" {
		password = p["password"]
	}

	if username == "" {
		WriteText(w, http.StatusUnprocessableEntity, "用户名不能为空")
		return
	}
	// Reserved names that emya rejects.
	reserved := []string{"emos", "root", "admin", "system", "test", "null", "true", "false", "emby"}
	for _, rv := range reserved {
		if strings.EqualFold(rv, username) {
			WriteText(w, http.StatusUnprocessableEntity, "不能使用这个昵称耶")
			return
		}
	}

	// Parse device info from X-Emby-Authorization or query.
	device := parseEmbyDevice(r)
	if device.DeviceID == "" {
		u.log.Warn("missing X-Emby-Authorization", "ua", r.Header.Get("User-Agent"))
		WriteText(w, http.StatusUnauthorized, "暂不兼容此设备 无 x-emby-authorization")
		return
	}

	ctx := r.Context()

	var userID int64

	// If an external API is configured, delegate login to it (emya behavior).
	if u.ext.Enabled() {
		resp, err := u.ext.Post(ctx, "/emby/userLogin", map[string]any{
			"username": username,
			"password": password,
			"client":   device.Client,
			"device":   device.Device,
			"deviceid": device.DeviceID,
			"version":  device.Version,
		})
		if err != nil {
			u.log.Error("external login error", "err", err, "user", username)
			WriteText(w, http.StatusInternalServerError, "外部验证错误 请稍后再试")
			return
		}
		if resp.Code != 200 {
			WriteText(w, resp.Code, resp.Message)
			return
		}
		var data struct {
			UserID    int64   `json:"user_id"`
			Username  string  `json:"username"`
			Folders   []int64 `json:"folders"`
			IsCanDown bool    `json:"is_can_down"`
		}
		_ = json.Unmarshal(resp.Data, &data)

		if data.UserID <= 0 {
			u.log.Error("external login returned invalid user_id", "user", username)
			WriteText(w, http.StatusInternalServerError, "外部验证错误")
			return
		}
		userID = data.UserID

		foldersJSON, _ := json.Marshal(data.Folders)
		update := `INSERT INTO user (id, username, folders, is_can_down)
			VALUES (?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				username = VALUES(username),
				folders  = VALUES(folders),
				is_can_down = VALUES(is_can_down)`
		if _, err := u.db.ExecContext(ctx, update, userID, data.Username, foldersJSON, data.IsCanDown); err != nil {
			u.log.Error("upsert user failed", "err", err)
			WriteText(w, http.StatusInternalServerError, "保存用户失败")
			return
		}
	} else {
		// Local DB auth.
		var (
			id         int64
			storedPw   db.NullString
			isDisable  db.NullBool
		)
		err := u.db.QueryRowContext(ctx,
			"SELECT id, password, is_disable FROM user WHERE username = ? AND deleted_at IS NULL LIMIT 1",
			username,
		).Scan(&id, &storedPw, &isDisable)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				u.log.Error("user lookup failed", "err", err)
			}
			WriteText(w, http.StatusUnauthorized, "用户名或密码错误")
			return
		}
		if storedPw.Valid && storedPw.String != "" && !auth.VerifyPassword(storedPw.String, password) {
			WriteText(w, http.StatusUnauthorized, "用户名或密码错误")
			return
		}
		if isDisable.Valid && isDisable.Bool {
			WriteText(w, http.StatusUnauthorized, "账号已被封禁")
			return
		}
		userID = id
	}

	// Enforce authorized user count (emya APP_AUTH_NUMBER).
	if u.cfg.AppAuthNumber > 0 && userID > int64(u.cfg.AppAuthNumber) {
		WriteText(w, http.StatusUnauthorized, "登陆失败 已超授权数")
		return
	}

	// Generate token.
	token := auth.RandomToken(16)
	if _, err := u.db.ExecContext(ctx, `
		INSERT INTO token (token, user_id, device_client, device_name, device_id, device_version)
		VALUES (?, ?, ?, ?, ?, ?)
	`, token, userID, device.Client, device.Device, device.DeviceID, device.Version); err != nil {
		u.log.Error("failed to store token", "err", err)
		WriteText(w, http.StatusInternalServerError, "保存会话失败")
		return
	}

	embyUser, err := u.transform.User(ctx, userID)
	if err != nil {
		u.log.Error("transform user failed", "err", err)
		WriteText(w, http.StatusInternalServerError, "加载用户失败")
		return
	}

	now := emby.FormatTimeNow()
	WriteJSON(w, http.StatusOK, map[string]any{
		"User": embyUser,
		"SessionInfo": map[string]any{
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
			"RemoteEndPoint":      "next-emby",
			"PlayableMediaTypes":  []any{},
			"PlaylistIndex":       0,
			"PlaylistLength":      0,
			"Id":                  embyUser["Id"],
			"ServerId":            u.cfg.EmbyID,
			"UserId":              embyUser["Id"],
			"UserName":            embyUser["Name"],
			"Client":              device.Client,
			"LastActivityDate":    now,
			"DeviceName":          device.Device,
			"InternalDeviceId":    0,
			"DeviceId":            device.DeviceID,
			"ApplicationVersion":  device.Version,
			"SupportedCommands":   []any{},
			"SupportsRemoteControl": false,
		},
		"AccessToken": token,
		"ServerId":    u.cfg.EmbyID,
	})
}

// Base returns the authenticated user's profile (/Users/{userId}).
func (u *Users) Base(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	data, err := u.transform.User(r.Context(), userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			WriteStatus(w, http.StatusNotFound)
			return
		}
		u.log.Error("user transform error", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, data)
}

// Views returns the user's visible libraries (emby calls this at startup).
func (u *Users) Views(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	rows, err := u.transform.GetUserLibrary(r.Context(), userID)
	if err != nil {
		u.log.Error("GetUserLibrary failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, ItemResponse(rows, int64(len(rows))))
}

// Items handles /Users/{userId}/Items with emya's search semantics.
func (u *Users) Items(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	q := r.URL.Query()

	includeTypes := parseCSV(q.Get("includeitemtypes"))
	filters := parseCSV(q.Get("filters"))

	searchTerm := q.Get("searchterm")
	if searchTerm == "" {
		searchTerm = q.Get("namestartswith")
	}

	// "Tag" / "BoxSet" probes without a search term => empty response.
	if searchTerm == "" && (contains(includeTypes, "Tag") || contains(includeTypes, "BoxSet")) {
		WriteJSON(w, http.StatusOK, EmptyItemResponse())
		return
	}

	// Emby "suggested random" page shows a set of default lists.
	if q.Get("sortby") == "IsFavoriteOrLiked,Random" {
		rows := []any{}
		defaults := map[string]int64{}
		_ = json.Unmarshal([]byte(u.cfg.SearchDefaultList), &defaults)
		if len(defaults) == 0 {
			defaults = map[string]int64{"欢迎来到 " + u.cfg.AppName: 0}
		}
		for name, id := range defaults {
			rows = append(rows, map[string]any{
				"Id":   emby.ItemID(emby.ItemIDTypeVideoList, id),
				"Name": name,
			})
		}
		WriteJSON(w, http.StatusOK, ItemResponse(rows, 0))
		return
	}

	// Fire-and-forget external search notification.
	if searchTerm != "" && u.ext.Enabled() {
		go func(term string, uid int64) {
			_, _ = u.ext.Post(r.Context(), "/emby/userSearch", map[string]any{
				"user_id": uid,
				"search":  term,
			})
		}(searchTerm, userID)
	}

	providerMap := map[string]string{}
	for _, part := range parseCSV(q.Get("anyprovideridequals")) {
		kv := strings.SplitN(part, ".", 2)
		if len(kv) == 2 {
			providerMap[strings.ToLower(kv[0])] = kv[1]
		}
	}

	search := VideoListSearch{
		ParentID:         q.Get("parentid"),
		StartIndex:       parseIntQuery(q.Get("startindex"), 0),
		Limit:            parseIntQuery(q.Get("limit"), 20),
		SortOrder:        q.Get("sortorder"),
		SortBy:           q.Get("sortby"),
		Filters:          filters,
		IncludeItemTypes: includeTypes,
		SearchTerm:       q.Get("searchterm"),
		NameStartsWith:   q.Get("namestartswith"),
		AnyProviderTmdb:  providerMap["tmdb"],
	}

	result, err := u.transform.VideoList(r.Context(), userID, search)
	if err != nil {
		u.log.Error("video list search failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, ItemResponse(result.Items, result.Count))
}

// ItemsLatest returns the "latest" set from a library.
func (u *Users) ItemsLatest(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	q := r.URL.Query()
	search := VideoListSearch{
		ParentID:  q.Get("parentid"),
		Limit:     parseIntQuery(q.Get("limit"), 20),
		SortBy:    "DateCreated",
		SortOrder: "Descending",
	}
	result, err := u.transform.VideoList(r.Context(), userID, search)
	if err != nil {
		u.log.Error("latest list failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, result.Items)
}

// ItemsResume returns in-progress items ("Continue Watching").
func (u *Users) ItemsResume(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	ctx := r.Context()
	q := r.URL.Query()

	where := "uvr.user_id = ? AND uvr.play_seconds IS NOT NULL"
	args := []any{userID}
	if parent := q.Get("parentid"); parent != "" {
		if kind, id, ok := emby.ParseItemID(parent); ok && kind == emby.ItemIDTypeVideoLibrary {
			where += " AND vl.video_library_id = ?"
			args = append(args, id)
		}
	}

	sqlStmt := `
		SELECT uvr.video_list_id, uvr.video_season_id, uvr.video_episode_id,
		       uvr.play_seconds, uvr.is_complete,
		       vl.video_type, vl.title, vl.date_air,
		       vs.title, vs.season_number,
		       ve.title, ve.episode_number,
		       vm.file_second
		FROM user_video_record uvr
		LEFT JOIN video_list    vl ON vl.id = uvr.video_list_id
		LEFT JOIN video_season  vs ON vs.id = uvr.video_season_id
		LEFT JOIN video_episode ve ON ve.id = uvr.video_episode_id
		LEFT JOIN video_media   vm ON vm.id = uvr.video_media_id
		WHERE ` + where + `
		ORDER BY uvr.updated_at DESC
		LIMIT 30`

	rows, err := u.db.QueryContext(ctx, sqlStmt, args...)
	if err != nil {
		u.log.Error("resume query failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	seen := map[int64]struct{}{}
	out := []any{}
	for rows.Next() {
		var (
			videoListID    int64
			videoSeasonID  db.NullInt64
			videoEpisodeID db.NullInt64
			playSeconds    db.NullInt64
			isComplete     db.NullBool
			videoType      db.NullString
			videoTitle     db.NullString
			videoDateAir   sql.NullTime
			seasonTitle    db.NullString
			seasonNumber   db.NullInt64
			episodeTitle   db.NullString
			episodeNumber  db.NullInt64
			fileSecond     db.NullInt64
		)
		if err := rows.Scan(&videoListID, &videoSeasonID, &videoEpisodeID,
			&playSeconds, &isComplete,
			&videoType, &videoTitle, &videoDateAir,
			&seasonTitle, &seasonNumber,
			&episodeTitle, &episodeNumber, &fileSecond); err != nil {
			u.log.Error("resume scan failed", "err", err)
			continue
		}
		if _, dup := seen[videoListID]; dup {
			continue
		}
		seen[videoListID] = struct{}{}

		uvr := u.transform.FormatUserVideoRecord(playSeconds.Int64, isComplete.Bool, fileSecond.Int64)
		videoID := emby.ItemID(emby.ItemIDTypeVideoList, videoListID)

		if videoType.String == db.VideoTypeTV && videoEpisodeID.Valid {
			episodeID := emby.ItemID(emby.ItemIDTypeVideoEpisode, videoEpisodeID.Int64)
			item := map[string]any{
				"Name":                    episodeTitle.String,
				"Id":                      episodeID,
				"CanDelete":               false,
				"RunTimeTicks":            int64(0),
				"ProductionYear":          videoDateAir.Time.Year(),
				"IndexNumber":             episodeNumber.Int64,
				"ParentIndexNumber":       seasonNumber.Int64,
				"IsFolder":                false,
				"Type":                    "Episode",
				"ParentBackdropItemId":    videoID,
				"ParentBackdropImageTags": []any{},
				"UserData": map[string]any{
					"PlayedPercentage":      uvr.Percentage,
					"PlaybackPositionTicks": uvr.PlayMs,
					"PlayCount":             0,
					"IsFavorite":            false,
					"Played":                uvr.IsComplete,
				},
				"SeriesName":             videoTitle.String,
				"SeriesId":               videoID,
				"SeriesPrimaryImageTag":  "",
				"SeasonName":             seasonTitle.String,
				"SeasonId":               emby.ItemID(emby.ItemIDTypeVideoSeason, videoSeasonID.Int64),
				"PrimaryImageAspectRatio": 1.7,
				"ImageTags":              map[string]any{"Primary": episodeID},
				"BackdropImageTags":      []any{},
				"MediaType":              "Video",
			}
			out = append(out, item)
		} else {
			out = append(out, map[string]any{
				"Name":                    videoTitle.String,
				"Id":                      videoID,
				"CanDelete":               false,
				"RunTimeTicks":            int64(0),
				"ProductionYear":          videoDateAir.Time.Year(),
				"IsFolder":                false,
				"Type":                    "Movie",
				"UserData": map[string]any{
					"PlayedPercentage":      uvr.Percentage,
					"PlaybackPositionTicks": uvr.PlayMs,
					"PlayCount":             0,
					"IsFavorite":            false,
					"Played":                uvr.IsComplete,
				},
				"PrimaryImageAspectRatio": 0.6,
				"ImageTags":               map[string]any{"Primary": videoID},
				"BackdropImageTags":       []any{},
				"MediaType":               "Video",
			})
		}
	}
	WriteJSON(w, http.StatusOK, ItemResponse(out, int64(len(out))))
}

// ItemInfo returns /Users/{userId}/Items/{itemId}.
func (u *Users) ItemInfo(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	itemID := chi.URLParam(r, "itemId")

	data, err := u.transform.ItemInfo(r.Context(), userID, itemID)
	if err != nil {
		u.log.Error("item info failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	if data == nil {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	WriteJSON(w, http.StatusOK, data)
}

// EmptyArray returns [] (for LocalTrailers / SpecialFeatures).
func (u *Users) EmptyArray(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, []any{})
}

// HideFromResume clears play progress for a given item.
func (u *Users) HideFromResume(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	ctx := r.Context()
	kind, numericID, ok := emby.ParseItemID(chi.URLParam(r, "itemId"))
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	videoListID := numericID
	if kind == emby.ItemIDTypeVideoEpisode {
		_ = u.db.QueryRowContext(ctx,
			"SELECT video_list_id FROM video_episode WHERE id = ? LIMIT 1", numericID,
		).Scan(&videoListID)
	}

	_, _ = u.db.ExecContext(ctx,
		"UPDATE user_video_record SET play_seconds = NULL WHERE user_id = ? AND video_list_id = ?",
		userID, videoListID)

	WriteJSON(w, http.StatusOK, map[string]any{
		"IsFavorite": false,
		"Played":     false,
		"PlayCount":  0,
	})
}

// Favorite toggles a favorite (POST adds, DELETE removes).
func (u *Users) Favorite(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	ctx := r.Context()
	kind, numericID, ok := emby.ParseItemID(chi.URLParam(r, "itemId"))
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	_, _ = u.db.ExecContext(ctx,
		"DELETE FROM favorites WHERE user_id = ? AND relation_type = ? AND relation_id = ?",
		userID, kind, numericID)

	isFavorite := false
	if r.Method == http.MethodPost {
		isFavorite = true
		_, _ = u.db.ExecContext(ctx,
			"INSERT INTO favorites (user_id, relation_type, relation_id) VALUES (?, ?, ?)",
			userID, kind, numericID)
	}

	if u.ext.Enabled() {
		_, _ = u.ext.Post(ctx, "/emby/userFavorite", map[string]any{
			"user_id":       userID,
			"relation_type": kind,
			"relation_id":   numericID,
			"is_favorite":   isFavorite,
		})
	}

	var (
		uvr UserVideoRecord
	)
	if kind == emby.ItemIDTypeVideoList {
		uvr, _ = u.transform.GetUserVideoRecord(ctx, userID, numericID, 0)
	}
	if kind == emby.ItemIDTypeVideoEpisode {
		uvr, _ = u.transform.GetUserVideoRecord(ctx, userID, 0, numericID)
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"IsFavorite":            isFavorite,
		"PlayCount":             0,
		"PlaybackPositionTicks": uvr.PlayMs,
		"Played":                uvr.IsComplete,
	})
}

// Played marks an item played or unplayed.
func (u *Users) Played(w http.ResponseWriter, r *http.Request) {
	userID := ctxpkg.UserID(r.Context())
	ctx := r.Context()
	kind, numericID, ok := emby.ParseItemID(chi.URLParam(r, "itemId"))
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	isPlayed := r.Method == http.MethodPost

	var (
		query string
		args  []any
	)
	switch kind {
	case emby.ItemIDTypeVideoList:
		query = "UPDATE user_video_record SET is_complete = ?, play_seconds = ? WHERE user_id = ? AND video_list_id = ? AND video_episode_id IS NULL"
		playSec := any(nil)
		if isPlayed {
			playSec = 0
		}
		args = []any{isPlayed, playSec, userID, numericID}
	case emby.ItemIDTypeVideoEpisode:
		query = "UPDATE user_video_record SET is_complete = ?, play_seconds = ? WHERE user_id = ? AND video_episode_id = ?"
		playSec := any(nil)
		if isPlayed {
			playSec = 0
		}
		args = []any{isPlayed, playSec, userID, numericID}
	default:
		WriteStatus(w, http.StatusNotFound)
		return
	}
	_, _ = u.db.ExecContext(ctx, query, args...)

	hasFav := u.transform.HasFavorite(ctx, userID, kind, numericID)

	WriteJSON(w, http.StatusOK, map[string]any{
		"IsFavorite": hasFav,
		"PlayCount":  0,
		// Without the raw progress row, echo 0 (Emby clients refresh shortly).
		"PlaybackPositionTicks": 0,
		"Played":                isPlayed,
	})
	// Silence unused strconv import in this file.
	_ = strconv.Itoa
}

// embyDevice is the parsed x-emby-authorization payload.
type embyDevice struct {
	Client   string
	Device   string
	DeviceID string
	Version  string
}

// parseEmbyDevice extracts the device headers Emby clients send.
// Accepts both MediaBrowser and Emby auth schemes, plus legacy x-emby-* query params.
func parseEmbyDevice(r *http.Request) embyDevice {
	d := embyDevice{}
	if raw := r.Header.Get("X-Emby-Authorization"); raw != "" {
		s := strings.TrimPrefix(raw, "MediaBrowser ")
		s = strings.TrimPrefix(s, "Emby ")
		s = strings.ReplaceAll(s, ", ", ",")
		for _, part := range strings.Split(s, ",") {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(kv[0]))
			val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
			if len(val) > 200 {
				val = val[:200]
			}
			switch key {
			case "client":
				d.Client = val
			case "device":
				d.Device = val
			case "deviceid":
				d.DeviceID = val
			case "version":
				d.Version = val
			}
		}
	}
	if d.Client == "" {
		d.Client = r.URL.Query().Get("x-emby-client")
	}
	if d.Device == "" {
		d.Device = r.URL.Query().Get("x-emby-device-name")
	}
	if d.DeviceID == "" {
		d.DeviceID = r.URL.Query().Get("x-emby-device-id")
	}
	if d.Version == "" {
		d.Version = r.URL.Query().Get("x-emby-client-version")
	}
	return d
}
