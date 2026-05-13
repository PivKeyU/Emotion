package handlers

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// Library serves /Library/* endpoints.
type Library struct {
	db        *sql.DB
	cfg       *config.Config
	log       *slog.Logger
	transform *Transform
}

// NewLibrary builds the handler.
func NewLibrary(database *sql.DB, cfg *config.Config, log *slog.Logger) *Library {
	return &Library{
		db:        database,
		cfg:       cfg,
		log:       log,
		transform: NewTransform(database, cfg),
	}
}

// MediaFolders is the list of top-level libraries visible to the caller.
func (l *Library) MediaFolders(w http.ResponseWriter, r *http.Request) {
	rows, err := l.transform.GetUserLibrary(r.Context(), ctxpkg.UserID(r.Context()))
	if err != nil {
		l.log.Error("media folders failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, http.StatusOK, ItemResponse(rows, int64(len(rows))))
}

// VirtualFolders is the Emby admin-style library listing. Sakura_embyboss uses this
// to discover library IDs before toggling access.
func (l *Library) VirtualFolders(w http.ResponseWriter, r *http.Request) {
	datas, err := l.transform.GetUserLibrary(r.Context(), ctxpkg.UserID(r.Context()))
	if err != nil {
		l.log.Error("virtual folders failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	out := []any{}
	for _, d := range datas {
		m := d.(map[string]any)
		out = append(out, map[string]any{
			"Name":           m["Name"],
			"Locations":      []any{},
			"LibraryOptions": map[string]any{},
			"ItemId":         m["Id"],
			"Id":             m["Id"],
			"Guid":           m["Guid"],
		})
	}

	// For the admin API key, expose *all* libraries (Sakura_embyboss needs the
	// complete set when configuring blocks).
	if ctxpkg.IsAPIKey(r.Context()) {
		rows, err := l.db.QueryContext(r.Context(),
			"SELECT id, name FROM library WHERE deleted_at IS NULL ORDER BY id ASC")
		if err == nil {
			out = out[:0]
			defer rows.Close()
			for rows.Next() {
				var id int64
				var name string
				if err := rows.Scan(&id, &name); err == nil {
					libID := "vb-" + itoa(id)
					out = append(out, map[string]any{
						"Name":           name,
						"Locations":      []any{},
						"LibraryOptions": map[string]any{},
						"ItemId":         libID,
						"Id":             libID,
						"Guid":           libID,
					})
				}
			}
		}
	}

	WriteJSON(w, http.StatusOK, out)
}
