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
	keyAdminAccountID
	keyClient
	keyDeviceName
	keyDeviceID
	keyRemoteAddr
)

// WithAuth stores authenticated user context on the request.
func WithAuth(ctx context.Context, userID int64, token string, isAdmin, isAPIKey bool) context.Context {
	ctx = context.WithValue(ctx, keyUserID, userID)
	ctx = context.WithValue(ctx, keyToken, token)
	ctx = context.WithValue(ctx, keyIsAdmin, isAdmin)
	ctx = context.WithValue(ctx, keyIsAPIKey, isAPIKey)
	return ctx
}

// WithAdminAccountID stores the dashboard admin account that owns this session.
func WithAdminAccountID(ctx context.Context, adminAccountID int64) context.Context {
	ctx = context.WithValue(ctx, keyAdminAccountID, adminAccountID)
	return ctx
}

// WithDevice attaches the caller's device descriptors for downstream handlers
// that record session activity (sessions.go, playback_activity).
func WithDevice(ctx context.Context, client, deviceName, deviceID, remoteAddr string) context.Context {
	ctx = context.WithValue(ctx, keyClient, client)
	ctx = context.WithValue(ctx, keyDeviceName, deviceName)
	ctx = context.WithValue(ctx, keyDeviceID, deviceID)
	ctx = context.WithValue(ctx, keyRemoteAddr, remoteAddr)
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

// AdminAccountID returns the dashboard admin account id, or 0 when the caller
// did not authenticate with a dashboard session.
func AdminAccountID(ctx context.Context) int64 {
	if v, ok := ctx.Value(keyAdminAccountID).(int64); ok {
		return v
	}
	return 0
}

// Client returns the X-Emby-Client value associated with this token.
func Client(ctx context.Context) string {
	v, _ := ctx.Value(keyClient).(string)
	return v
}

// DeviceName returns the X-Emby-Device-Name associated with this token.
func DeviceName(ctx context.Context) string {
	v, _ := ctx.Value(keyDeviceName).(string)
	return v
}

// DeviceID returns the X-Emby-Device-Id associated with this token.
func DeviceID(ctx context.Context) string {
	v, _ := ctx.Value(keyDeviceID).(string)
	return v
}

// RemoteAddr returns the caller's IP, with X-Forwarded-For honored upstream.
func RemoteAddr(ctx context.Context) string {
	v, _ := ctx.Value(keyRemoteAddr).(string)
	return v
}
