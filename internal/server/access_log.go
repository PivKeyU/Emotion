package server

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/PivKeyU/Emotion/internal/db"
)

// accessLogThrottle dedupes user_access_log upserts so a busy client can't
// hammer the DB with one insert per request. The same (user, device, ip) tuple
// will only round-trip once per accessLogTTL.
var (
	accessLogSeen sync.Map
	accessLogTTL  = 5 * time.Minute
)

// touchAccessLog records that userID just hit the API from devID/ip. It is
// fire-and-forget: failures are logged but never block the request path.
func touchAccessLog(database *db.DB, log *slog.Logger, userID int64, devID, devName, devClient, devVersion, ip, ua string) {
	if database == nil || userID <= 0 {
		return
	}
	key := fmt.Sprintf("%d|%s|%s", userID, devID, ip)
	now := time.Now()
	if prev, ok := accessLogSeen.Load(key); ok {
		if t, ok := prev.(time.Time); ok && now.Sub(t) < accessLogTTL {
			return
		}
	}
	accessLogSeen.Store(key, now)

	uaTrim := ua
	if len(uaTrim) > 1024 {
		uaTrim = uaTrim[:1024]
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := database.ExecContext(ctx, `
			INSERT INTO user_access_log
				(user_id, device_id, device_name, device_client, device_version, ip, user_agent)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, device_id, ip) DO UPDATE SET
				device_name = COALESCE(EXCLUDED.device_name, user_access_log.device_name),
				device_client = COALESCE(EXCLUDED.device_client, user_access_log.device_client),
				device_version = COALESCE(EXCLUDED.device_version, user_access_log.device_version),
				user_agent = COALESCE(EXCLUDED.user_agent, user_access_log.user_agent),
				last_seen_at = NOW(),
				seen_count = user_access_log.seen_count + 1
		`,
			userID,
			devID,
			nullString(devName),
			nullString(devClient),
			nullString(devVersion),
			ip,
			nullString(uaTrim),
		)
		if err != nil && log != nil {
			log.Warn("access log upsert failed", "category", "auth", "err", err)
		}
	}()
}

func nullString(s string) sql.NullString {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
