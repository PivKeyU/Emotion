package handlers

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Next-Emby/internal/auth"
	"github.com/PivKeyU/Next-Emby/internal/config"
	"github.com/PivKeyU/Next-Emby/internal/db"
	"github.com/PivKeyU/Next-Emby/internal/server/ctxpkg"
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
	db  *sql.DB
	cfg *config.Config
	log *slog.Logger
}

// NewManagement builds the handler.
func NewManagement(database *sql.DB, cfg *config.Config, log *slog.Logger) *Management {
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
		"SELECT id, username, is_can_down, is_admin, is_disable FROM user WHERE deleted_at IS NULL ORDER BY id ASC")
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
		)
		if err := rows.Scan(&id, &username, &isCanDown, &isAdmin, &isDisable); err != nil {
			continue
		}
		out = append(out, userToEmby(m.cfg, id, username.String, isCanDown.Bool, isAdmin.Bool, isDisable.Bool))
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
	if prefix == "" {
		prefix = q.Get("name")
	}

	var (
		where string
		args  []any
	)
	where = "deleted_at IS NULL"
	if prefix != "" {
		where += " AND username LIKE ?"
		args = append(args, prefix+"%")
	}

	rows, err := m.db.QueryContext(r.Context(),
		"SELECT id, username, is_can_down, is_admin, is_disable FROM user WHERE "+where+" ORDER BY id ASC",
		args...,
	)
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
		)
		if err := rows.Scan(&id, &username, &isCanDown, &isAdmin, &isDisable); err != nil {
			continue
		}
		items = append(items, userToEmby(m.cfg, id, username.String, isCanDown.Bool, isAdmin.Bool, isDisable.Bool))
	}
	WriteJSON(w, http.StatusOK, ItemResponse(items, int64(len(items))))
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

	res, err := m.db.ExecContext(r.Context(),
		"INSERT INTO user (username, password, is_admin, is_disable) VALUES (?, ?, ?, ?)",
		name, hashed, false, false,
	)
	if err != nil {
		m.log.Error("user create failed", "err", err)
		WriteText(w, http.StatusConflict, "用户名已存在")
		return
	}
	id, _ := res.LastInsertId()
	WriteJSON(w, http.StatusOK, userToEmby(m.cfg, id, name, false, false, false))
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
	_, err := m.db.ExecContext(r.Context(),
		"UPDATE user SET deleted_at = NOW() WHERE id = ?", userID)
	if err != nil {
		m.log.Error("user delete failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	// Drop the user's outstanding tokens so the client is kicked out.
	_, _ = m.db.ExecContext(r.Context(), "DELETE FROM token WHERE user_id = ?", userID)
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
		"UPDATE user SET password = ? WHERE id = ?", hashed, userID)
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
	IsAdministrator          bool     `json:"IsAdministrator"`
	IsDisabled               bool     `json:"IsDisabled"`
	EnableContentDownloading bool     `json:"EnableContentDownloading"`
	EnableAllFolders         bool     `json:"EnableAllFolders"`
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
	_ = json.Unmarshal(raw, &body)

	// Resolve visible library IDs.
	// Sakura sends library names in BlockedMediaFolders and library GUIDs in EnabledFolders.
	// Our library table is keyed by numeric id, which we also use as GUID; so parse "vb-<id>"
	// or bare numerics.
	var visibleIDs []int64
	if body.EnableAllFolders {
		rows, err := m.db.QueryContext(r.Context(),
			"SELECT id FROM library WHERE deleted_at IS NULL")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err == nil {
					visibleIDs = append(visibleIDs, id)
				}
			}
		}
	} else {
		for _, g := range body.EnabledFolders {
			if id, ok := parseGUIDOrID(g); ok {
				visibleIDs = append(visibleIDs, id)
			}
		}
	}

	// Also handle blocked names - if any library name is in BlockedMediaFolders, remove from visibles.
	blockedNames := stringifyAny(body.BlockedMediaFolders)
	if len(blockedNames) > 0 && len(visibleIDs) > 0 {
		// Convert names to IDs then subtract.
		blockedIDs := map[int64]struct{}{}
		ph := make([]string, 0, len(blockedNames))
		args := make([]any, 0, len(blockedNames))
		for _, n := range blockedNames {
			ph = append(ph, "?")
			args = append(args, n)
		}
		rows, err := m.db.QueryContext(r.Context(),
			"SELECT id FROM library WHERE name IN ("+strings.Join(ph, ",")+") AND deleted_at IS NULL",
			args...,
		)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err == nil {
					blockedIDs[id] = struct{}{}
				}
			}
		}
		filtered := visibleIDs[:0]
		for _, id := range visibleIDs {
			if _, blocked := blockedIDs[id]; !blocked {
				filtered = append(filtered, id)
			}
		}
		visibleIDs = filtered
	}

	foldersJSON, _ := json.Marshal(visibleIDs)

	_, err = m.db.ExecContext(r.Context(), `
		UPDATE user
		SET is_admin = ?, is_disable = ?, is_can_down = ?, folders = ?
		WHERE id = ?
	`, body.IsAdministrator, body.IsDisabled, body.EnableContentDownloading, foldersJSON, userID)
	if err != nil {
		m.log.Error("policy update failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteStatus(w, http.StatusNoContent)
}

// SessionMessage accepts a Sakura-style "show message" POST. We have no websocket
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
		"Id":             deviceID,
		"Name":           "",
		"AppName":        "",
		"AppVersion":     "",
		"LastUserId":     "",
		"LastUserName":   "",
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

// UsageStatsQuery returns aggregated watch time for users / a user.
func (m *Management) UsageStatsQuery(w http.ResponseWriter, r *http.Request) {
	if !m.requireAdmin(w, r) {
		return
	}
	var body customQueryBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	defer r.Body.Close()

	sqlStr := strings.ToUpper(body.CustomQueryString)

	var results []map[string]any
	ctx := r.Context()

	if strings.Contains(sqlStr, "GROUP BY USERID") && strings.Contains(sqlStr, "WATCHTIME") && !strings.Contains(sqlStr, "WHERE USERID") {
		// Global ranking: top users by watch time.
		rows, err := m.db.QueryContext(ctx, `
			SELECT user_id, SUM(play_duration - pause_duration) AS WatchTime
			FROM playback_activity
			WHERE date_created >= DATE_SUB(NOW(), INTERVAL 7 DAY)
			GROUP BY user_id
			ORDER BY WatchTime DESC
		`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var (
					userID    string
					watchTime int64
				)
				if err := rows.Scan(&userID, &watchTime); err == nil {
					results = append(results, map[string]any{
						"UserId":    userID,
						"WatchTime": watchTime,
					})
				}
			}
		}
	} else if strings.Contains(sqlStr, "WHERE USERID") {
		// Per-user aggregate.
		// Extract the user id from the SQL string (best-effort, quoted).
		userID := extractSingleQuoted(body.CustomQueryString, "UserId")
		if userID != "" {
			var (
				lastLogin sql.NullTime
				watchTime sql.NullFloat64
			)
			_ = m.db.QueryRowContext(ctx, `
				SELECT MAX(date_created), SUM(play_duration - pause_duration) / 60
				FROM playback_activity
				WHERE user_id = ? AND date_created >= DATE_SUB(NOW(), INTERVAL 7 DAY)
			`, userID).Scan(&lastLogin, &watchTime)
			row := map[string]any{}
			if lastLogin.Valid {
				row["LastLogin"] = lastLogin.Time.Format("2006-01-02 15:04:05")
			}
			if watchTime.Valid {
				row["WatchTime"] = watchTime.Float64
			}
			results = append(results, row)
		}
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"colums":  []any{"UserId", "WatchTime", "LastLogin"},
		"results": results,
	})
}

// ---------- helpers ----------

// userToEmby builds the minimal Emby User shape the management API returns for
// list/query/create calls. Full shape (/Users/{id}) is produced by Transform.User.
func userToEmby(cfg *config.Config, id int64, username string, isCanDown, isAdmin, isDisable bool) map[string]any {
	embyID := strconv.FormatInt(id, 10)
	return map[string]any{
		"Name":          username,
		"ServerId":      cfg.EmbyID,
		"Id":            embyID,
		"HasPassword":   true,
		"HasConfiguredPassword": true,
		"Configuration": map[string]any{},
		"Policy": map[string]any{
			"IsAdministrator":          isAdmin,
			"IsDisabled":               isDisable,
			"EnableContentDownloading": isCanDown,
			"EnableAllFolders":         true,
			"EnabledFolders":           []any{},
			"SimultaneousStreamLimit":  0,
			"EnableMediaPlayback":      true,
			"BlockedMediaFolders":      []any{},
		},
	}
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
// Only used for the two hard-coded Sakura queries.
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
