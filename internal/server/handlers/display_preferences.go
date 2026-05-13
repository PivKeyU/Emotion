package handlers

import "net/http"

// DisplayPreferences serves the /DisplayPreferences/Usersettings endpoint.
// Emby web clients request this to load per-user UI preferences at login.
type DisplayPreferences struct{}

// NewDisplayPreferences constructs the handler.
func NewDisplayPreferences() *DisplayPreferences { return &DisplayPreferences{} }

// Usersettings returns a minimal "everything default" preferences doc.
// Mirrors emya's displayPreferences.controller.ts.
func (d *DisplayPreferences) Usersettings(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"Id":          "usersettings",
		"CustomPrefs": map[string]any{},
		"SortOrder":   "Ascending",
	})
}
