package handlers

import (
	_ "embed"
	"net/http"
)

// dashboardHTML is the single-file admin UI, embedded at compile time so the
// binary stays self-contained (no external static-file directory needed).
//
//go:embed dashboard.html
var dashboardHTML []byte

// Dashboard serves the HTML admin panel. It's intentionally registered outside
// the auth guard: the page is just assets (HTML/CSS/JS). The page itself
// prompts the user for an API key and then calls the authenticated REST API
// (/admin/*, /emby/*) using that key as ?api_key= on each request.
type Dashboard struct{}

// NewDashboard constructs the dashboard handler.
func NewDashboard() *Dashboard { return &Dashboard{} }

// Page serves the admin UI HTML.
// GET /admin/ui
func (d *Dashboard) Page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}
