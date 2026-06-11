package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// recoverer catches panics so a single bad handler can't crash the server.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte("error"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestLogger logs each request/response with duration and status code.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			log.Info("http",
				"category", "http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// statusWriter captures status codes for logging.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// lowercaseQuery rewrites all query keys to lowercase, emulating emya's preValidation hook.
// Body keys stay as-is because Emby clients often send mixed-case JSON with values
// our handlers want to preserve; handlers parse case-insensitively when needed.
func lowercaseQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query()
		if len(raw) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		q := url.Values{}
		for k, v := range raw {
			q[strings.ToLower(k)] = v
		}
		r.URL.RawQuery = q.Encode()
		next.ServeHTTP(w, r)
	})
}

// tokenRegex extracts Token="..." from an X-Emby-Authorization header.
var tokenRegex = regexp.MustCompile(`Token="([^"]+)"`)

// extractToken pulls an auth token from query, headers, or X-Emby-Authorization header.
// Mirrors emya's AuthGuard.
func extractToken(r *http.Request) string {
	q := r.URL.Query()
	if t := q.Get("x-emby-token"); t != "" {
		return t
	}
	if t := q.Get("api_key"); t != "" {
		return t
	}
	if t := q.Get("apikey"); t != "" {
		return t
	}
	if t := q.Get("x-mediabrowser-token"); t != "" {
		return t
	}
	if t := r.Header.Get("X-Emby-Token"); t != "" {
		return t
	}
	if t := r.Header.Get("X-MediaBrowser-Token"); t != "" {
		return t
	}
	if raw := r.Header.Get("X-Emby-Authorization"); raw != "" {
		if m := tokenRegex.FindStringSubmatch(raw); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

func isCompatAnonymousRead(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	path := r.URL.Path
	return strings.HasSuffix(path, "/Sessions") ||
		strings.HasSuffix(path, "/Items/Counts") ||
		strings.HasSuffix(path, "/System/Info")
}

// authGuardBuilder returns a middleware that authenticates using either:
//   - the configured admin API key (grants admin context), or
//   - a user token from the token table (looked up and refreshed).
//
// Endpoints registered as "ignore auth" must skip this middleware entirely.
func authGuardBuilder(deps *Dependencies) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r)
			if token == "" {
				if isCompatAnonymousRead(r) {
					next.ServeHTTP(w, r)
					return
				}
				writeText(w, http.StatusUnauthorized, "登录失效 请重新登录")
				return
			}
			remoteAddr := clientIP(r)

			// Keep the bootstrap admin key valid as the master credential.
			if deps.Config.APIKey != "" && token == deps.Config.APIKey {
				ctx := ctxpkg.WithAuth(r.Context(), 0, token, true, true)
				ctx = ctxpkg.WithDevice(ctx, "", "", "", remoteAddr)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Check dashboard session tokens minted by /admin/login.
			var (
				adminSessionID int64
				adminAccountID sql.NullInt64
			)
			err := deps.DB.QueryRowContext(r.Context(), `
				SELECT id, admin_account_id FROM admin_session
				WHERE token = ? AND revoked_at IS NULL
				  AND (expires_at IS NULL OR expires_at > NOW())
				LIMIT 1`, token,
			).Scan(&adminSessionID, &adminAccountID)
			if err == nil {
				_, _ = deps.DB.ExecContext(r.Context(),
					"UPDATE admin_session SET last_used_at = NOW() WHERE id = ?", adminSessionID)
				ctx := ctxpkg.WithAuth(r.Context(), 0, token, true, false)
				if adminAccountID.Valid {
					ctx = ctxpkg.WithAdminAccountID(ctx, adminAccountID.Int64)
				}
				ctx = ctxpkg.WithDevice(ctx, "", "", "", remoteAddr)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				deps.Logger.Error("admin session lookup failed", "category", "auth", "err", err)
			}

			// Check server-generated third-party API keys.
			hash := tokenHash(token)
			var adminKeyID int64
			err = deps.DB.QueryRowContext(r.Context(), `
				SELECT id FROM admin_api_key
				WHERE token_hash = ? AND revoked_at IS NULL
				LIMIT 1`, hash,
			).Scan(&adminKeyID)
			if err == nil {
				_, _ = deps.DB.ExecContext(r.Context(),
					"UPDATE admin_api_key SET last_used_at = NOW() WHERE id = ?", adminKeyID)
				ctx := ctxpkg.WithAuth(r.Context(), 0, token, true, true)
				ctx = ctxpkg.WithDevice(ctx, "", "", "", remoteAddr)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				deps.Logger.Error("admin api key lookup failed", "category", "auth", "err", err)
			}

			// Look up user token.
			var (
				tokenID    int64
				userID     int64
				devClient  sql.NullString
				devName    sql.NullString
				devID      sql.NullString
				devVersion sql.NullString
			)
			err = deps.DB.QueryRowContext(r.Context(),
				"SELECT id, user_id, device_client, device_name, device_id, device_version FROM token WHERE token = ? LIMIT 1", token,
			).Scan(&tokenID, &userID, &devClient, &devName, &devID, &devVersion)
			if err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					deps.Logger.Error("auth token lookup failed", "err", err)
				}
				if isCompatAnonymousRead(r) {
					next.ServeHTTP(w, r)
					return
				}
				writeText(w, http.StatusUnauthorized, "登录失效 请重新登录")
				return
			}

			// Touch last_used_at async-ish, but keep it simple and synchronous.
			_, _ = deps.DB.ExecContext(r.Context(),
				"UPDATE token SET last_used_at = NOW() WHERE id = ?", tokenID,
			)

			// Also compute is_admin by checking the user record.
			var isAdmin sql.NullBool
			_ = deps.DB.QueryRowContext(r.Context(),
				"SELECT is_admin FROM app_user WHERE id = ? LIMIT 1", userID,
			).Scan(&isAdmin)

			ctx := ctxpkg.WithAuth(r.Context(), userID, token, isAdmin.Bool, false)
			ctx = ctxpkg.WithDevice(ctx, devClient.String, devName.String, devID.String, remoteAddr)
			touchAccessLog(deps.DB, deps.Logger, userID, devID.String, devName.String, devClient.String, devVersion.String, remoteAddr, r.Header.Get("User-Agent"))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// clientIP returns the caller's remote IP. Honors the first hop of
// X-Forwarded-For (set by typical reverse proxies) before falling back to
// the direct connection address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if comma := strings.Index(xff, ","); comma >= 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return strings.TrimSpace(xff)
	}
	if rip := r.Header.Get("X-Real-IP"); rip != "" {
		return strings.TrimSpace(rip)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
