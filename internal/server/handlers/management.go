package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Emotion/internal/auth"
	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// Management serves Emby admin endpoints used by Sakura_embyboss:
//
//   - /Users             (list)
//   - /Users/Query       (search)
//   - /Users/New         (create)
//   - /Users/{id}/Password
//   - /Users/{id}/Policy
//   - DELETE /Users/{id}
//   - /Sessions/{id}/Message
//   - /Sessions/{id}/Playing/Stop
//   - /Devices/Info
//   - /user_usage_stats/submit_custom_query
type Management struct {
	db  *db.DB
	cfg *config.Config
	log *slog.Logger
}

// NewManagement builds the handler.
func NewManagement(database *db.DB, cfg *config.Config, log *slog.Logger) *Management {
	return &Management{db: database, cfg: cfg, log: log}
}

// requireAdmin enforces admin (API key or admin user). Returns false when rejected.
func (m *Management) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if ctxpkg.IsAPIKey(r.Context()) || ctxpkg.IsAdmin(r.Context()) {
		return true
	}
	WriteText(w, http.StatusForbidden, "需要管理员权限")
	return false
}

// UsersList returns every user. Emby's GET /Users.
func (m *Management) UsersList(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	rows, err := m.db.QueryContext(r.Context(),
		"SELECT id, username, is_can_down, is_admin, is_disable, folders FROM app_user WHERE deleted_at IS NULL ORDER BY id ASC")
	if err != nil {
		m.log.Error("users list query failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var (
			id        int64
			username  db.NullString
			isCanDown db.NullBool
			isAdmin   db.NullBool
			isDisable db.NullBool
			folders   db.NullString
		)
		if err := rows.Scan(&id, &username, &isCanDown, &isAdmin, &isDisable, &folders); err != nil {
			continue
		}
		enableAll, enabled := userFolderPolicy(folders)
		user := userToEmby(m.cfg, id, username.String, isCanDown.Bool, isAdmin.Bool, isDisable.Bool, enableAll, enabled)
		m.attachUserDeviceAnomaly(r.Context(), id, user)
		out = append(out, user)
	}
	WriteJSON(w, http.StatusOK, out)
}

// UsersQuery is Emby's paginated user search (NameStartsWithOrGreater, etc).
func (m *Management) UsersQuery(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	prefix := q.Get("namestartswithorgreater")
	term := q.Get("searchterm")
	if term == "" {
		term = q.Get("search")
	}
	if term == "" {
		term = q.Get("name")
	}

	var (
		where string
		args  []any
	)
	where = "deleted_at IS NULL"
	if term != "" {
		where += " AND username ILIKE ?"
		args = append(args, "%"+term+"%")
	} else if prefix != "" {
		where += " AND username ILIKE ?"
		args = append(args, prefix+"%")
	}
	total := m.countUsers(r, where, args...)
	startIndex := parseIntQuery(q.Get("startindex"), 0)
	limit := parseIntQuery(q.Get("limit"), 0)
	if limit < 0 {
		limit = 0
	}
	if limit > 200 {
		limit = 200
	}

	query := "SELECT id, username, is_can_down, is_admin, is_disable, folders FROM app_user WHERE " + where + " ORDER BY id ASC"
	queryArgs := append([]any{}, args...)
	if limit > 0 {
		query += " LIMIT ?"
		queryArgs = append(queryArgs, limit)
	}
	if startIndex > 0 {
		if limit <= 0 {
			query += " LIMIT ALL"
		}
		query += " OFFSET ?"
		queryArgs = append(queryArgs, startIndex)
	}

	rows, err := m.db.QueryContext(r.Context(), query, queryArgs...)
	if err != nil {
		m.log.Error("users query failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var (
			id        int64
			username  db.NullString
			isCanDown db.NullBool
			isAdmin   db.NullBool
			isDisable db.NullBool
			folders   db.NullString
		)
		if err := rows.Scan(&id, &username, &isCanDown, &isAdmin, &isDisable, &folders); err != nil {
			continue
		}
		enableAll, enabled := userFolderPolicy(folders)
		user := userToEmby(m.cfg, id, username.String, isCanDown.Bool, isAdmin.Bool, isDisable.Bool, enableAll, enabled)
		m.attachUserDeviceAnomaly(r.Context(), id, user)
		items = append(items, user)
	}
	if total == 0 && prefix == "" && len(items) > 0 {
		total = int64(len(items))
	}
	WriteJSON(w, http.StatusOK, ItemResponse(items, total))
}

// userCreateBody is the POST body of /Users/New.
type userCreateBody struct {
	Name     string `json:"Name"`
	Username string `json:"username"`
	Password string `json:"Password"`
}

// UserNew creates a new user and returns the Emby user object.
func (m *Management) UserNew(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	var body userCreateBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(body.Username)
	}
	if name == "" {
		WriteText(w, http.StatusBadRequest, "name required")
		return
	}

	hashed := ""
	if body.Password != "" {
		h, err := auth.HashPassword(body.Password)
		if err != nil {
			WriteText(w, http.StatusInternalServerError, "hash failed")
			return
		}
		hashed = h
	}

	isAdmin := m.activeUserCount(r) == 0
	res, err := m.db.ExecContext(r.Context(),
		"INSERT INTO app_user (username, password, folders, is_can_down, is_admin, is_disable) VALUES (?, ?, ?, ?, ?, ?)",
		name, hashed, nil, true, isAdmin, false,
	)
	if err != nil {
		m.log.Error("user create failed", "err", err)
		if m.activeUsernameExists(r, name, 0) {
			WriteText(w, http.StatusConflict, "用户名已存在")
			return
		}
		WriteText(w, http.StatusInternalServerError, "创建用户失败")
		return
	}
	id, _ := res.LastInsertId()
	WriteJSON(w, http.StatusOK, userToEmby(m.cfg, id, name, true, isAdmin, false, true, idsToFolderStrings(m.allLibraryIDs(r))))
}

// UserDelete soft-deletes a user (DELETE /Users/{id}).
func (m *Management) UserDelete(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	userID, ok := resolveUserID(chi.URLParam(r, "userId"))
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	_, err := m.db.ExecContext(ctx,
		"UPDATE app_user SET deleted_at = NOW() WHERE id = ?", userID)
	if err != nil {
		m.log.Error("user delete failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	// Drop the user's outstanding tokens so the client is kicked out.
	_, _ = m.db.ExecContext(ctx, "DELETE FROM token WHERE user_id = ?", userID)
	WriteStatus(w, http.StatusNoContent)
}

// userPasswordBody matches Sakura_embyboss pwd_policy output.
// Either reset = true / false (with no NewPw) or NewPw is provided.
type userPasswordBody struct {
	ID            string `json:"Id"`
	ResetPassword bool   `json:"ResetPassword"`
	NewPw         string `json:"NewPw"`
	CurrentPw     string `json:"CurrentPw"`
}

// UserPassword sets or resets a user's password.
func (m *Management) UserPassword(w http.ResponseWriter, r *http.Request) {
	// Any user can change their own password; admins can change anyone's.
	userID, ok := resolveUserID(chi.URLParam(r, "userId"))
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	callerID := ctxpkg.UserID(r.Context())
	isAdmin := ctxpkg.IsAPIKey(r.Context()) || ctxpkg.IsAdmin(r.Context())
	if !isAdmin && callerID != userID {
		WriteStatus(w, http.StatusForbidden)
		return
	}

	var body userPasswordBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	hashed := ""
	if body.ResetPassword {
		hashed = ""
	} else if body.NewPw != "" {
		h, err := auth.HashPassword(body.NewPw)
		if err != nil {
			WriteStatus(w, http.StatusInternalServerError)
			return
		}
		hashed = h
	}

	// If the client indicates "reset to blank" (ResetPassword=true, no NewPw),
	// we store an empty string hash which VerifyPassword treats as "no password".
	_, err := m.db.ExecContext(r.Context(),
		"UPDATE app_user SET password = ? WHERE id = ?", hashed, userID)
	if err != nil {
		m.log.Error("password update failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	// Invalidate any active tokens on password change.
	_, _ = m.db.ExecContext(r.Context(), "DELETE FROM token WHERE user_id = ?", userID)

	WriteStatus(w, http.StatusNoContent)
}

// userPolicyBody only captures the fields Sakura_embyboss actually changes.
// We store the full payload as JSON on the user row so future reads round-trip cleanly.
type userPolicyBody struct {
	IsAdministrator          *bool    `json:"IsAdministrator"`
	IsDisabled               *bool    `json:"IsDisabled"`
	EnableContentDownloading *bool    `json:"EnableContentDownloading"`
	EnableAllFolders         *bool    `json:"EnableAllFolders"`
	EnabledFolders           []string `json:"EnabledFolders"`
	BlockedMediaFolders      []any    `json:"BlockedMediaFolders"`
}

// UserPolicy updates per-user policy (admin, disabled, downloadable, visible folders).
// Sakura_embyboss uses this heavily via create_policy() / update_user_enabled_folder().
func (m *Management) UserPolicy(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	userID, ok := resolveUserID(chi.URLParam(r, "userId"))
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteStatus(w, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var body userPolicyBody
	if err := json.Unmarshal(raw, &body); err != nil {
		WriteText(w, http.StatusBadRequest, "invalid body")
		return
	}

	var (
		currentAdmin   db.NullBool
		currentDisable db.NullBool
		currentCanDown db.NullBool
		currentFolders db.NullString
	)
	err = m.db.QueryRowContext(r.Context(), `
		SELECT is_admin, is_disable, is_can_down, folders
		FROM app_user
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`, userID).Scan(&currentAdmin, &currentDisable, &currentCanDown, &currentFolders)
	if err != nil {
		if err == sql.ErrNoRows {
			WriteStatus(w, http.StatusNotFound)
			return
		}
		m.log.Error("policy load failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	blockedNames := stringifyAny(body.BlockedMediaFolders)
	allIDs := m.allLibraryIDs(r)
	blockedIDs := m.blockedLibraryIDsByName(r, blockedNames)
	visibleIDs, storeAllFolders := resolvePolicyFolderIDs(allIDs, currentFolders, body.EnableAllFolders, body.EnabledFolders, blockedIDs)

	var foldersValue any
	if storeAllFolders {
		foldersValue = nil
	} else {
		foldersJSON, _ := json.Marshal(visibleIDs)
		foldersValue = string(foldersJSON)
	}

	_, err = m.db.ExecContext(r.Context(), `
		UPDATE app_user
		SET is_admin = ?, is_disable = ?, is_can_down = ?, folders = ?
		WHERE id = ?
	`, boolValue(body.IsAdministrator, currentAdmin.Bool), boolValue(body.IsDisabled, currentDisable.Bool), boolValue(body.EnableContentDownloading, currentCanDown.Bool), foldersValue, userID)
	if err != nil {
		m.log.Error("policy update failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteStatus(w, http.StatusNoContent)
}

type adminUserUpdateBody struct {
	Name                     string  `json:"name"`
	Password                 *string `json:"password"`
	IsAdministrator          bool    `json:"is_administrator"`
	IsDisabled               bool    `json:"is_disabled"`
	EnableContentDownloading bool    `json:"enable_content_downloading"`
	EnableAllFolders         bool    `json:"enable_all_folders"`
	EnabledFolders           []int64 `json:"enabled_folders"`
}

// AdminUserUpdate updates a local user's editable dashboard fields.
func (m *Management) AdminUserUpdate(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	userID, ok := resolveUserID(chi.URLParam(r, "userId"))
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	var body adminUserUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteText(w, http.StatusBadRequest, "invalid body")
		return
	}
	defer r.Body.Close()
	name := strings.TrimSpace(body.Name)
	if name == "" {
		WriteText(w, http.StatusBadRequest, "name required")
		return
	}
	visibleIDs := body.EnabledFolders
	var foldersValue any
	if body.EnableAllFolders {
		foldersValue = nil
	} else {
		foldersJSON, _ := json.Marshal(visibleIDs)
		foldersValue = foldersJSON
	}

	args := []any{name, body.IsAdministrator, body.IsDisabled, body.EnableContentDownloading, foldersValue}
	sets := []string{"username = ?", "is_admin = ?", "is_disable = ?", "is_can_down = ?", "folders = ?", "updated_at = NOW()"}
	if body.Password != nil {
		hashed := ""
		if *body.Password != "" {
			h, err := auth.HashPassword(*body.Password)
			if err != nil {
				WriteStatus(w, http.StatusInternalServerError)
				return
			}
			hashed = h
		}
		sets = append(sets, "password = ?")
		args = append(args, hashed)
	}
	args = append(args, userID)
	res, err := m.db.ExecContext(r.Context(),
		"UPDATE app_user SET "+strings.Join(sets, ", ")+" WHERE id = ? AND deleted_at IS NULL", args...)
	if err != nil {
		m.log.Error("admin user update failed", "err", err)
		if m.activeUsernameExists(r, name, userID) {
			WriteText(w, http.StatusConflict, "用户名已存在")
			return
		}
		WriteText(w, http.StatusInternalServerError, "更新用户失败")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	_, _ = m.db.ExecContext(r.Context(), "DELETE FROM token WHERE user_id = ?", userID)
	WriteStatus(w, http.StatusNoContent)
}

// AdminUserAccessLog returns recent device/IP history rows and unresolved
// anomaly records for a user. The dashboard renders this in the "设备记录" modal.
func (m *Management) AdminUserAccessLog(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	userID, ok := resolveUserID(chi.URLParam(r, "userId"))
	if !ok {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	rows, err := m.db.QueryContext(r.Context(), `
		SELECT device_id, COALESCE(device_name,''), COALESCE(device_client,''),
		       COALESCE(device_version,''), ip, COALESCE(user_agent,''),
		       first_seen_at, last_seen_at, seen_count
		FROM user_access_log
		WHERE user_id = ?
		ORDER BY last_seen_at DESC
		LIMIT 100
	`, userID)
	if err != nil {
		m.log.Error("access log query failed", "category", "auth", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 32)
	for rows.Next() {
		var (
			devID, devName, devClient, devVersion, ip, ua string
			firstSeen, lastSeen                           sql.NullTime
			seenCount                                     int64
		)
		if err := rows.Scan(&devID, &devName, &devClient, &devVersion, &ip, &ua, &firstSeen, &lastSeen, &seenCount); err != nil {
			m.log.Error("access log scan failed", "category", "auth", "err", err)
			WriteStatus(w, http.StatusInternalServerError)
			return
		}
		out = append(out, map[string]any{
			"DeviceId":    devID,
			"DeviceName":  devName,
			"Client":      devClient,
			"Version":     devVersion,
			"Ip":          ip,
			"UserAgent":   ua,
			"FirstSeenAt": nullTimeToISO(firstSeen),
			"LastSeenAt":  nullTimeToISO(lastSeen),
			"SeenCount":   seenCount,
		})
	}
	if err := rows.Err(); err != nil {
		m.log.Error("access log iterate failed", "category", "auth", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"items":     out,
		"anomalies": m.userDeviceAnomalies(r.Context(), userID),
	})
}

func nullTimeToISO(nt sql.NullTime) string {
	if !nt.Valid {
		return ""
	}
	return nt.Time.UTC().Format("2006-01-02T15:04:05Z")
}

// to push to, but we succeed silently so the bot's UX doesn't break.
func (m *Management) SessionMessage(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	defer r.Body.Close()
	WriteStatus(w, http.StatusNoContent)
}

// SessionStop accepts a "stop this session" POST. We invalidate all tokens tied
// to the given "session" (emby's sessionId is our token id for the stub session).
// The simplest behavior that matches intent: delete the most recent token.
func (m *Management) SessionStop(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	sessionID := chi.URLParam(r, "sessionId")
	// Best-effort: sessionId might be a token, token id, or our server id.
	_, _ = m.db.ExecContext(r.Context(), "DELETE FROM token WHERE token = ?", sessionID)
	WriteStatus(w, http.StatusNoContent)
}

// DeviceInfo satisfies Sakura's GET /Devices/Info?Id=<deviceId> probe used to detect unauthorized clients.
// Returning an empty device record is fine - Sakura only checks presence.
func (m *Management) DeviceInfo(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	deviceID := r.URL.Query().Get("id")
	WriteJSON(w, http.StatusOK, map[string]any{
		"Id":               deviceID,
		"Name":             "",
		"AppName":          "",
		"AppVersion":       "",
		"LastUserId":       "",
		"LastUserName":     "",
		"DateLastActivity": "",
	})
}

// UsageStatsQuery implements the subset of Emby's user_usage_stats plugin that
// Sakura_embyboss calls: POST /user_usage_stats/submit_custom_query with a
// CustomQueryString that is one of two well-known statements (see emby.py).
//
// We detect those two patterns and return structured results. Arbitrary SQL is
// rejected for safety.
type customQueryBody struct {
	CustomQueryString string `json:"CustomQueryString"`
	ReplaceUserId     bool   `json:"ReplaceUserId"`
}

// UsageStatsQuery handles the pivkeyu_emby bot's custom SQL queries against
// playback_activity. We detect known patterns and translate them into safe
// parameterized PostgreSQL queries. Arbitrary SQL is never executed.
func (m *Management) UsageStatsQuery(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	var body customQueryBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	sqlStr := strings.ToUpper(body.CustomQueryString)
	ctx := r.Context()

	var (
		columns []any
		results [][]any
	)

	startTime, endTime := extractDateRange(body.CustomQueryString)

	switch {
	case strings.Contains(sqlStr, "COUNT(DISTINCT") && strings.Contains(sqlStr, "GROUP BY USERID"):
		// Pattern 8: per-user device count + IP count, paginated.
		columns = []any{"UserId", "device_count", "ip_count"}
		limit, offset := extractLimitOffset(sqlStr)
		if limit <= 0 {
			limit = 20
		}
		rows, err := m.db.QueryContext(ctx, `
			SELECT user_id,
			       COUNT(DISTINCT device_name || '|' || client) AS device_count,
			       COUNT(DISTINCT remote_address) AS ip_count
			FROM playback_activity
			GROUP BY user_id
			ORDER BY device_count DESC
			LIMIT ? OFFSET ?
		`, limit+1, offset)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var uid string
				var dc, ic int64
				if rows.Scan(&uid, &dc, &ic) == nil {
					results = append(results, []any{uid, dc, ic})
				}
			}
		}

	case strings.Contains(sqlStr, "DEVICENAME") && strings.Contains(sqlStr, "CLIENTNAME") && strings.Contains(sqlStr, "REMOTEADDRESS") && strings.Contains(sqlStr, "WHERE USERID"):
		// Pattern 4: user's distinct devices/IPs.
		columns = []any{"DeviceName", "ClientName", "RemoteAddress"}
		userID := extractSingleQuoted(body.CustomQueryString, "UserId")
		if userID != "" {
			rows, err := m.db.QueryContext(ctx, `
				SELECT DISTINCT device_name, client, remote_address
				FROM playback_activity WHERE user_id = ?
			`, userID)
			if err == nil {
				defer rows.Close()
				for rows.Next() {
					var dn, cn, ra string
					if rows.Scan(&dn, &cn, &ra) == nil {
						results = append(results, []any{dn, cn, ra})
					}
				}
			}
		}

	case strings.Contains(sqlStr, "REMOTEADDRESS") && strings.Contains(sqlStr, "GROUP BY USERID"):
		// Pattern 5: reverse lookup users by IP.
		columns = []any{"UserId", "DeviceName", "ClientName", "RemoteAddress", "LastActivity", "ActivityCount"}
		ip := extractSingleQuoted(body.CustomQueryString, "RemoteAddress")
		if ip != "" {
			q := `SELECT user_id, device_name, client, remote_address, MAX(date_created), COUNT(*)
				FROM playback_activity WHERE remote_address = ?`
			args := []any{ip}
			if startTime != "" {
				q += " AND date_created >= ? AND date_created <= ?"
				args = append(args, startTime, endTime)
			}
			q += " GROUP BY user_id, device_name, client, remote_address ORDER BY MAX(date_created) DESC"
			rows, err := m.db.QueryContext(ctx, q, args...)
			if err == nil {
				defer rows.Close()
				for rows.Next() {
					var uid, dn, cn, ra string
					var la sql.NullTime
					var cnt int64
					if rows.Scan(&uid, &dn, &cn, &ra, &la, &cnt) == nil {
						laStr := ""
						if la.Valid {
							laStr = la.Time.Format("2006-01-02 15:04:05")
						}
						results = append(results, []any{uid, dn, cn, ra, laStr, cnt})
					}
				}
			}
		}

	case strings.Contains(sqlStr, "DEVICENAME LIKE"):
		// Pattern 6: reverse lookup users by device substring.
		columns = []any{"UserId", "DeviceName", "ClientName", "RemoteAddress", "LastActivity", "ActivityCount"}
		pattern := extractLikePattern(body.CustomQueryString, "DeviceName")
		if pattern != "" {
			q := `SELECT user_id, device_name, client, remote_address, MAX(date_created), COUNT(*)
				FROM playback_activity WHERE device_name ILIKE ?`
			args := []any{"%" + pattern + "%"}
			if startTime != "" {
				q += " AND date_created >= ? AND date_created <= ?"
				args = append(args, startTime, endTime)
			}
			q += " GROUP BY user_id, device_name, client, remote_address ORDER BY MAX(date_created) DESC"
			rows, err := m.db.QueryContext(ctx, q, args...)
			if err == nil {
				defer rows.Close()
				for rows.Next() {
					var uid, dn, cn, ra string
					var la sql.NullTime
					var cnt int64
					if rows.Scan(&uid, &dn, &cn, &ra, &la, &cnt) == nil {
						laStr := ""
						if la.Valid {
							laStr = la.Time.Format("2006-01-02 15:04:05")
						}
						results = append(results, []any{uid, dn, cn, ra, laStr, cnt})
					}
				}
			}
		}

	case strings.Contains(sqlStr, "CLIENTNAME LIKE"):
		// Pattern 7: reverse lookup users by client substring.
		columns = []any{"UserId", "DeviceName", "ClientName", "RemoteAddress", "LastActivity", "ActivityCount"}
		pattern := extractLikePattern(body.CustomQueryString, "ClientName")
		if pattern != "" {
			q := `SELECT user_id, device_name, client, remote_address, MAX(date_created), COUNT(*)
				FROM playback_activity WHERE client ILIKE ?`
			args := []any{"%" + pattern + "%"}
			if startTime != "" {
				q += " AND date_created >= ? AND date_created <= ?"
				args = append(args, startTime, endTime)
			}
			q += " GROUP BY user_id, device_name, client, remote_address ORDER BY MAX(date_created) DESC"
			rows, err := m.db.QueryContext(ctx, q, args...)
			if err == nil {
				defer rows.Close()
				for rows.Next() {
					var uid, dn, cn, ra string
					var la sql.NullTime
					var cnt int64
					if rows.Scan(&uid, &dn, &cn, &ra, &la, &cnt) == nil {
						laStr := ""
						if la.Valid {
							laStr = la.Time.Format("2006-01-02 15:04:05")
						}
						results = append(results, []any{uid, dn, cn, ra, laStr, cnt})
					}
				}
			}
		}

	case strings.Contains(sqlStr, "GROUP BY NAME") || strings.Contains(sqlStr, "GROUP BY ITEMNAME"):
		// Pattern 3: top items by watch time, optionally filtered by ItemType and/or UserId.
		columns = []any{"UserId", "ItemId", "ItemType", "name", "play_count", "total_duarion"}
		itemType := extractSingleQuoted(body.CustomQueryString, "ItemType")
		userID := extractSingleQuoted(body.CustomQueryString, "UserId")
		limit := 10
		if idx := strings.Index(sqlStr, "LIMIT"); idx >= 0 {
			fmt.Sscanf(sqlStr[idx:], "LIMIT %d", &limit)
		}
		q := `SELECT user_id, item_id, item_type, item_name, COUNT(*) AS play_count,
		      SUM(play_duration - pause_duration) AS total_duration
		      FROM playback_activity WHERE 1=1`
		args := []any{}
		if itemType != "" {
			q += " AND item_type = ?"
			args = append(args, itemType)
		}
		if userID != "" {
			q += " AND user_id = ?"
			args = append(args, userID)
		}
		if startTime != "" {
			q += " AND date_created >= ? AND date_created <= ?"
			args = append(args, startTime, endTime)
		}
		q += " GROUP BY user_id, item_id, item_type, item_name ORDER BY total_duration DESC LIMIT ?"
		args = append(args, limit)
		rows, err := m.db.QueryContext(ctx, q, args...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var uid, iid, itype string
				var iname sql.NullString
				var pc, td int64
				if rows.Scan(&uid, &iid, &itype, &iname, &pc, &td) == nil {
					results = append(results, []any{uid, iid, itype, iname.String, pc, td})
				}
			}
		}

	case strings.Contains(sqlStr, "GROUP BY USERID") && strings.Contains(sqlStr, "WATCHTIME") && !strings.Contains(sqlStr, "WHERE USERID"):
		// Pattern 1 (existing): Global ranking — top users by watch time.
		columns = []any{"UserId", "WatchTime"}
		q := `SELECT user_id, SUM(play_duration - pause_duration) AS WatchTime
			FROM playback_activity WHERE 1=1`
		args := []any{}
		if startTime != "" {
			q += " AND date_created >= ? AND date_created < ?"
			args = append(args, startTime, endTime)
		} else {
			q += " AND date_created >= now() - interval '7 days'"
		}
		q += " GROUP BY user_id ORDER BY WatchTime DESC"
		rows, err := m.db.QueryContext(ctx, q, args...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var uid string
				var wt int64
				if rows.Scan(&uid, &wt) == nil {
					results = append(results, []any{uid, wt})
				}
			}
		}

	case strings.Contains(sqlStr, "WHERE USERID"):
		// Pattern 2 (existing): Per-user aggregate.
		columns = []any{"LastLogin", "WatchTime"}
		userID := extractSingleQuoted(body.CustomQueryString, "UserId")
		if userID != "" {
			q := `SELECT MAX(date_created), SUM(play_duration - pause_duration) / 60
				FROM playback_activity WHERE user_id = ?`
			args := []any{userID}
			if startTime != "" {
				q += " AND date_created >= ? AND date_created < ?"
				args = append(args, startTime, endTime)
			} else {
				q += " AND date_created >= now() - interval '7 days'"
			}
			var lastLogin sql.NullTime
			var watchTime sql.NullFloat64
			_ = m.db.QueryRowContext(ctx, q, args...).Scan(&lastLogin, &watchTime)
			row := []any{"", float64(0)}
			if lastLogin.Valid {
				row[0] = lastLogin.Time.Format("2006-01-02 15:04:05")
			}
			if watchTime.Valid {
				row[1] = watchTime.Float64
			}
			results = append(results, row)
		}

	default:
		columns = []any{}
	}

	if columns == nil {
		columns = []any{}
	}
	if results == nil {
		results = [][]any{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"colums":  columns,
		"results": results,
	})
}

// ---------- helpers ----------

// userToEmby builds the minimal Emby User shape the management API returns for
// list/query/create calls. Full shape (/Users/{id}) is produced by Transform.User.
func userToEmby(cfg *config.Config, id int64, username string, isCanDown, isAdmin, isDisable, enableAllFolders bool, enabledFolders []string) map[string]any {
	embyID := strconv.FormatInt(id, 10)
	return map[string]any{
		"Name":                  username,
		"ServerId":              cfg.EmbyID,
		"Id":                    embyID,
		"HasPassword":           true,
		"HasConfiguredPassword": true,
		"Configuration":         map[string]any{},
		"Policy": map[string]any{
			"IsAdministrator":          isAdmin,
			"IsDisabled":               isDisable,
			"EnableContentDownloading": isCanDown,
			"EnableAllFolders":         enableAllFolders,
			"EnabledFolders":           enabledFolders,
			"SimultaneousStreamLimit":  0,
			"EnableMediaPlayback":      true,
			"BlockedMediaFolders":      []any{},
		},
	}
}

func (m *Management) attachUserDeviceAnomaly(ctx context.Context, userID int64, user map[string]any) {
	anomalies := m.userDeviceAnomalies(ctx, userID)
	if len(anomalies) == 0 {
		user["DeviceAnomaly"] = nil
		return
	}
	user["DeviceAnomaly"] = anomalies[0]
	policy, _ := user["Policy"].(map[string]any)
	if policy != nil {
		policy["DeviceAnomaly"] = anomalies[0]
	}
}

func (m *Management) userDeviceAnomalies(ctx context.Context, userID int64) []map[string]any {
	if userID <= 0 {
		return []map[string]any{}
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT reason, COALESCE(detail, ''), first_seen_at, last_seen_at
		FROM user_device_anomaly
		WHERE user_id = ? AND resolved_at IS NULL
		ORDER BY last_seen_at DESC
		LIMIT 10
	`, userID)
	if err != nil {
		if m.log != nil {
			m.log.Warn("device anomaly query failed", "category", "auth", "err", err)
		}
		return []map[string]any{}
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			reason, detail      string
			firstSeen, lastSeen sql.NullTime
		)
		if err := rows.Scan(&reason, &detail, &firstSeen, &lastSeen); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"Reason":      reason,
			"Detail":      detail,
			"FirstSeenAt": nullTimeToISO(firstSeen),
			"LastSeenAt":  nullTimeToISO(lastSeen),
		})
	}
	return out
}

func (m *Management) allLibraryIDs(r *http.Request) []int64 {
	rows, err := m.db.QueryContext(r.Context(), "SELECT id FROM library WHERE deleted_at IS NULL AND COALESCE(is_hidden, false) = false ORDER BY id ASC")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func (m *Management) activeUserCount(r *http.Request) int64 {
	var count int64
	_ = m.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM app_user WHERE deleted_at IS NULL").Scan(&count)
	return count
}

func (m *Management) activeUsernameExists(r *http.Request, username string, exceptUserID int64) bool {
	var count int64
	query := "SELECT COUNT(*) FROM app_user WHERE deleted_at IS NULL AND username = ?"
	args := []any{username}
	if exceptUserID > 0 {
		query += " AND id <> ?"
		args = append(args, exceptUserID)
	}
	_ = m.db.QueryRowContext(r.Context(), query, args...).Scan(&count)
	return count > 0
}

func (m *Management) countUsers(r *http.Request, where string, args ...any) int64 {
	var count int64
	_ = m.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM app_user WHERE "+where, args...).Scan(&count)
	return count
}

func idsToFolderStrings(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, emby.ItemID(emby.ItemIDTypeVideoLibrary, id))
	}
	return out
}

func boolValue(next *bool, fallback bool) bool {
	if next == nil {
		return fallback
	}
	return *next
}

func userFolderPolicy(folders db.NullString) (bool, []string) {
	if !folders.Valid || strings.TrimSpace(folders.String) == "" {
		return true, []string{}
	}
	var ids []int64
	if err := json.Unmarshal([]byte(folders.String), &ids); err != nil {
		return true, []string{}
	}
	return false, idsToFolderStrings(ids)
}

func resolvePolicyFolderIDs(allIDs []int64, currentFolders db.NullString, enableAllFolders *bool, enabledFolders []string, blockedIDs map[int64]struct{}) ([]int64, bool) {
	hasBlocked := len(blockedIDs) > 0
	hasEnabledFolders := enabledFolders != nil

	if enableAllFolders != nil && *enableAllFolders {
		if hasBlocked {
			return removeBlockedIDs(allIDs, blockedIDs), false
		}
		return append([]int64{}, allIDs...), true
	}

	if hasEnabledFolders {
		ids := folderIDsFromStrings(enabledFolders)
		if hasBlocked {
			ids = removeBlockedIDs(ids, blockedIDs)
		}
		return ids, false
	}

	if hasBlocked {
		return removeBlockedIDs(allIDs, blockedIDs), false
	}

	if enableAllFolders != nil && !*enableAllFolders {
		return []int64{}, false
	}

	if !currentFolders.Valid || strings.TrimSpace(currentFolders.String) == "" {
		return append([]int64{}, allIDs...), true
	}
	return folderIDsFromJSON(currentFolders), false
}

func folderIDsFromJSON(folders db.NullString) []int64 {
	if !folders.Valid || strings.TrimSpace(folders.String) == "" {
		return []int64{}
	}
	var ids []int64
	if err := json.Unmarshal([]byte(folders.String), &ids); err != nil {
		return []int64{}
	}
	return ids
}

func folderIDsFromStrings(folders []string) []int64 {
	ids := make([]int64, 0, len(folders))
	for _, raw := range folders {
		if id, ok := parseGUIDOrID(raw); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func removeBlockedIDs(ids []int64, blocked map[int64]struct{}) []int64 {
	if len(blocked) == 0 {
		return append([]int64{}, ids...)
	}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := blocked[id]; ok {
			continue
		}
		out = append(out, id)
	}
	return out
}

func (m *Management) blockedLibraryIDsByName(r *http.Request, names []string) map[int64]struct{} {
	out := map[int64]struct{}{}
	if len(names) == 0 {
		return out
	}
	ph := make([]string, 0, len(names))
	args := make([]any, 0, len(names))
	for _, n := range names {
		ph = append(ph, "?")
		args = append(args, n)
	}
	rows, err := m.db.QueryContext(r.Context(),
		"SELECT id FROM library WHERE name IN ("+strings.Join(ph, ",")+") AND deleted_at IS NULL",
		args...,
	)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out[id] = struct{}{}
		}
	}
	return out
}

// resolveUserID accepts either a numeric id or a hex id string.
func resolveUserID(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err == nil && n > 0 {
		return n, true
	}
	return 0, false
}

// parseGUIDOrID decodes "vb-<id>" or a bare numeric id.
func parseGUIDOrID(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if strings.HasPrefix(raw, "vb-") {
		n, err := strconv.ParseInt(strings.TrimPrefix(raw, "vb-"), 10, 64)
		return n, err == nil && n > 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	return n, err == nil && n > 0
}

// stringifyAny turns an array of arbitrary JSON values into their string forms.
func stringifyAny(in []any) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// extractSingleQuoted pulls a value out of "Field = 'value'" style SQL clauses.
// Only used for the hard-coded pivkeyu queries.
func extractSingleQuoted(s, field string) string {
	up := strings.ToUpper(s)
	fieldUp := strings.ToUpper(field)
	idx := strings.Index(up, fieldUp+" = '")
	if idx < 0 {
		idx = strings.Index(up, fieldUp+"='")
	}
	if idx < 0 {
		return ""
	}
	start := idx + strings.Index(up[idx:], "'") + 1
	end := strings.Index(s[start:], "'")
	if end < 0 {
		return ""
	}
	return s[start : start+end]
}

// extractLikePattern pulls the value from "Field LIKE '%value%'" clauses.
func extractLikePattern(s, field string) string {
	up := strings.ToUpper(s)
	fieldUp := strings.ToUpper(field)
	idx := strings.Index(up, fieldUp+" LIKE ")
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(field)+6:]
	qStart := strings.Index(rest, "'")
	if qStart < 0 {
		return ""
	}
	rest = rest[qStart+1:]
	qEnd := strings.Index(rest, "'")
	if qEnd < 0 {
		return ""
	}
	val := rest[:qEnd]
	val = strings.TrimPrefix(val, "%")
	val = strings.TrimSuffix(val, "%")
	return val
}

// extractDateRange pulls DateCreated >= '...' AND DateCreated <= '...' timestamps.
func extractDateRange(s string) (string, string) {
	up := strings.ToUpper(s)
	startIdx := strings.Index(up, "DATECREATED >= '")
	if startIdx < 0 {
		startIdx = strings.Index(up, "DATECREATED >='")
	}
	if startIdx < 0 {
		return "", ""
	}
	startVal := extractSingleQuoted(s[startIdx:], "DateCreated >=")
	if startVal == "" {
		startVal = extractSingleQuoted(s[startIdx:], "DateCreated>=")
	}
	if startVal == "" {
		afterQuote := strings.Index(s[startIdx:], "'")
		if afterQuote >= 0 {
			rest := s[startIdx+afterQuote+1:]
			end := strings.Index(rest, "'")
			if end > 0 {
				startVal = rest[:end]
			}
		}
	}

	endIdx := strings.Index(up, "DATECREATED <= '")
	if endIdx < 0 {
		endIdx = strings.Index(up, "DATECREATED <='")
	}
	if endIdx < 0 {
		endIdx = strings.Index(up, "DATECREATED < '")
	}
	endVal := ""
	if endIdx >= 0 {
		afterQuote := strings.Index(s[endIdx:], "'")
		if afterQuote >= 0 {
			rest := s[endIdx+afterQuote+1:]
			end := strings.Index(rest, "'")
			if end > 0 {
				endVal = rest[:end]
			}
		}
	}
	return startVal, endVal
}

// extractLimitOffset pulls LIMIT N OFFSET M from the SQL string.
func extractLimitOffset(s string) (int, int) {
	up := strings.ToUpper(s)
	limit, offset := 0, 0
	if idx := strings.Index(up, "LIMIT "); idx >= 0 {
		fmt.Sscanf(up[idx:], "LIMIT %d", &limit)
	}
	if idx := strings.Index(up, "OFFSET "); idx >= 0 {
		fmt.Sscanf(up[idx:], "OFFSET %d", &offset)
	}
	return limit, offset
}
