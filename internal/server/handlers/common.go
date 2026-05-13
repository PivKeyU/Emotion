// Package handlers contains HTTP handlers for Emby-compatible endpoints.
// Responses follow emya's shape (src/controller/emby/*.ts) so that existing
// Emby-compatible clients and admin tools like Sakura_embyboss continue to work.
package handlers

import (
	"encoding/json"
	"net/http"
)

// WriteJSON emits a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// WriteText emits a plain-text response with the given status.
func WriteText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// WriteStatus emits an empty response with just a status code.
func WriteStatus(w http.ResponseWriter, status int) {
	w.WriteHeader(status)
}

// EmptyItemResponse returns {"Items":[],"TotalRecordCount":0} - emya's ItemResponse() default.
func EmptyItemResponse() map[string]any {
	return map[string]any{
		"Items":            []any{},
		"TotalRecordCount": 0,
	}
}

// ItemResponse wraps a slice into the standard Emby envelope.
func ItemResponse(items []any, total int64) map[string]any {
	if items == nil {
		items = []any{}
	}
	if total < 0 {
		total = int64(len(items))
	}
	return map[string]any{
		"Items":            items,
		"TotalRecordCount": total,
	}
}
