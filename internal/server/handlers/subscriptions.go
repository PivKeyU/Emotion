package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// Subscriptions serves series subscription endpoints for the "follow a series"
// feature. Users subscribe to a video_list (TV series); when new episodes are
// imported, events are fanned out to all subscribers. The bot polls the event
// queue and delivers notifications.
type Subscriptions struct {
	db  *db.DB
	log *slog.Logger
}

func NewSubscriptions(database *db.DB, log *slog.Logger) *Subscriptions {
	return &Subscriptions{db: database, log: log}
}

// Subscribe adds a subscription. POST /Users/{userId}/SubscribedSeries/{itemId}
func (s *Subscriptions) Subscribe(w http.ResponseWriter, r *http.Request) {
	userID := subscriptionUserID(r)
	if userID == 0 {
		WriteStatus(w, http.StatusUnauthorized)
		return
	}
	kind, numericID, ok := emby.ParseItemID(chi.URLParam(r, "itemId"))
	if !ok || kind != emby.ItemIDTypeVideoList {
		WriteText(w, http.StatusBadRequest, "itemId must be a video list (vl-N)")
		return
	}
	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO series_subscription (user_id, video_list_id)
		VALUES (?, ?)
		ON CONFLICT (user_id, video_list_id) DO NOTHING
	`, userID, numericID)
	if err != nil {
		s.log.Error("subscribe failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"subscribed": true, "video_list_id": numericID})
}

// Unsubscribe removes a subscription. DELETE /Users/{userId}/SubscribedSeries/{itemId}
func (s *Subscriptions) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	userID := subscriptionUserID(r)
	if userID == 0 {
		WriteStatus(w, http.StatusUnauthorized)
		return
	}
	kind, numericID, ok := emby.ParseItemID(chi.URLParam(r, "itemId"))
	if !ok || kind != emby.ItemIDTypeVideoList {
		WriteText(w, http.StatusBadRequest, "itemId must be a video list (vl-N)")
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `
		DELETE FROM series_subscription WHERE user_id = ? AND video_list_id = ?
	`, userID, numericID)
	WriteJSON(w, http.StatusOK, map[string]any{"subscribed": false, "video_list_id": numericID})
}

// List returns the user's subscribed series. GET /Users/{userId}/SubscribedSeries
func (s *Subscriptions) List(w http.ResponseWriter, r *http.Request) {
	userID := subscriptionUserID(r)
	if userID == 0 {
		WriteStatus(w, http.StatusUnauthorized)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT ss.video_list_id, vl.title, vl.video_type, ss.created_at
		FROM series_subscription ss
		JOIN video_list vl ON vl.id = ss.video_list_id
		WHERE ss.user_id = ?
		ORDER BY ss.created_at DESC
	`, userID)
	if err != nil {
		s.log.Error("list subscriptions failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var (
			vlID      int64
			title     string
			videoType string
			createdAt string
		)
		if err := rows.Scan(&vlID, &title, &videoType, &createdAt); err != nil {
			continue
		}
		items = append(items, map[string]any{
			"Id":        emby.ItemID(emby.ItemIDTypeVideoList, vlID),
			"Name":      title,
			"Type":      videoType,
			"Subscribed": createdAt,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"Items":            items,
		"TotalRecordCount": len(items),
	})
}

// EventsPending returns undelivered subscription events for the bot to consume.
// GET /admin/series-events?limit=100
func (s *Subscriptions) EventsPending(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	limit := parseIntQuery(r.URL.Query().Get("limit"), 100)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT e.id, e.user_id, e.video_list_id, e.video_episode_id, e.created_at,
		       vl.title AS series_title,
		       ve.title AS episode_title,
		       ve.episode_number,
		       vs.season_number
		FROM series_subscription_event e
		JOIN video_list vl ON vl.id = e.video_list_id
		JOIN video_episode ve ON ve.id = e.video_episode_id
		LEFT JOIN video_season vs ON vs.id = ve.video_season_id
		WHERE e.delivered_at IS NULL
		ORDER BY e.created_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		s.log.Error("events pending query failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	events := []any{}
	for rows.Next() {
		var (
			id, userID, vlID, veID int64
			createdAt              string
			seriesTitle, epTitle   string
			epNum, seasonNum      db.NullInt64
		)
		if err := rows.Scan(&id, &userID, &vlID, &veID, &createdAt, &seriesTitle, &epTitle, &epNum, &seasonNum); err != nil {
			continue
		}
		events = append(events, map[string]any{
			"id":             id,
			"user_id":        userID,
			"video_list_id":  vlID,
			"video_episode_id": veID,
			"created_at":     createdAt,
			"series_title":   seriesTitle,
			"episode_title":  epTitle,
			"episode_number": epNum.Int64,
			"season_number":  seasonNum.Int64,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}

// EventsAck marks events as delivered. POST /admin/series-events/ack
func (s *Subscriptions) EventsAck(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.IDs) == 0 {
		WriteText(w, http.StatusBadRequest, "ids required")
		return
	}
	defer r.Body.Close()
	ph := make([]string, 0, len(body.IDs))
	args := make([]any, 0, len(body.IDs))
	for _, id := range body.IDs {
		ph = append(ph, "?")
		args = append(args, id)
	}
	_, _ = s.db.ExecContext(r.Context(),
		"UPDATE series_subscription_event SET delivered_at = NOW() WHERE id IN ("+join(ph, ",")+")", args...)
	WriteJSON(w, http.StatusOK, map[string]any{"acked": len(body.IDs)})
}

func (s *Subscriptions) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if ctxpkg.IsAPIKey(r.Context()) || ctxpkg.IsAdmin(r.Context()) {
		return true
	}
	WriteText(w, http.StatusForbidden, "需要管理员权限")
	return false
}

func subscriptionUserID(r *http.Request) int64 {
	if ctxpkg.IsAdmin(r.Context()) || ctxpkg.IsAPIKey(r.Context()) {
		if id, err := strconv.ParseInt(chi.URLParam(r, "userId"), 10, 64); err == nil && id > 0 {
			return id
		}
	}
	return ctxpkg.UserID(r.Context())
}

func join(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}
