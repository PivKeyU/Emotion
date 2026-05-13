package emby

import "time"

// FormatTime formats a time.Time into the Emby-expected ISO8601 with 7-digit fractional seconds.
// Zero values return DefaultTime.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return DefaultTime
	}
	// Emby uses 7-digit fractional seconds with trailing Z.
	return t.UTC().Format("2006-01-02T15:04:05.0000000Z")
}

// FormatTimeNow returns the current time formatted for Emby.
func FormatTimeNow() string {
	return FormatTime(time.Now())
}

// Year returns the 4-digit year string for t, or "" if zero.
func Year(t time.Time) int {
	if t.IsZero() {
		return 0
	}
	return t.Year()
}
