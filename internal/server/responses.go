package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON writes any value as application/json with the given status code.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// writeText writes plain text (used by Emby /Ping and error messages).
func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// writeStatus writes only a status code with no body.
func writeStatus(w http.ResponseWriter, status int) {
	w.WriteHeader(status)
}

// writeError writes an error response. Mirrors emya ExceptionFilter behavior.
func writeError(w http.ResponseWriter, log *slog.Logger, status int, msg string, err error) {
	if err != nil && log != nil {
		log.Error("http error", "status", status, "msg", msg, "err", err)
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	writeText(w, status, msg)
}
