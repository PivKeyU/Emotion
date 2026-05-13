// Package ctxpkg holds shared request-context helpers so handlers and middleware
// can read/write authenticated identity without importing the parent server
// package (which would cycle).
package ctxpkg

import "context"

type ctxKey int

const (
	keyUserID ctxKey = iota
	keyToken
	keyIsAdmin
	keyIsAPIKey
)

// WithAuth stores authenticated user context on the request.
func WithAuth(ctx context.Context, userID int64, token string, isAdmin, isAPIKey bool) context.Context {
	ctx = context.WithValue(ctx, keyUserID, userID)
	ctx = context.WithValue(ctx, keyToken, token)
	ctx = context.WithValue(ctx, keyIsAdmin, isAdmin)
	ctx = context.WithValue(ctx, keyIsAPIKey, isAPIKey)
	return ctx
}

// UserID returns the authenticated user id, or 0 if absent.
func UserID(ctx context.Context) int64 {
	if v, ok := ctx.Value(keyUserID).(int64); ok {
		return v
	}
	return 0
}

// Token returns the authenticated token string.
func Token(ctx context.Context) string {
	if v, ok := ctx.Value(keyToken).(string); ok {
		return v
	}
	return ""
}

// IsAdmin reports whether the caller authenticated as admin.
func IsAdmin(ctx context.Context) bool {
	v, _ := ctx.Value(keyIsAdmin).(bool)
	return v
}

// IsAPIKey reports whether the caller used the admin API key.
func IsAPIKey(ctx context.Context) bool {
	v, _ := ctx.Value(keyIsAPIKey).(bool)
	return v
}
