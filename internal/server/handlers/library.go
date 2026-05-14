package handlers

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"

	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/emby"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

// Library serves /Library/* endpoints.
type Library struct {
	db        *db.DB
	cfg       *config.Config
	log       *slog.Logger
	transform *Transform
}

// NewLibrary builds the handler.
func NewLibrary(database *db.DB, cfg *config.Config, log *slog.Logger) *Library {
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

// SelectableMediaFolders returns folder locations usable by Emby-compatible
// automation tools such as MoviePilot.
func (l *Library) SelectableMediaFolders(w http.ResponseWriter, r *http.Request) {
	rows, err := l.db.QueryContext(r.Context(), `
		SELECT id, name, COALESCE(root_path, ''), COALESCE(role, '')
		FROM library
		WHERE deleted_at IS NULL
		ORDER BY id ASC`)
	if err != nil {
		l.log.Error("selectable media folders failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []any{}
	for rows.Next() {
		var id int64
		var name, root, role string
		if err := rows.Scan(&id, &name, &root, &role); err != nil {
			continue
		}
		if strings.TrimSpace(root) == "" {
			continue
		}
		out = append(out, map[string]any{
			"Name":         name,
			"Path":         root,
			"Id":           "vb-" + itoa(id),
			"ItemId":       "vb-" + itoa(id),
			"CollectionId": "vb-" + itoa(id),
			"Type":         role,
		})
	}
	WriteJSON(w, http.StatusOK, out)
}

func libraryRootForItem(ctx context.Context, d *db.DB, itemID string) (libraryID int64, root, role string, ok bool) {
	kind, numericID, parsed := emby.ParseItemID(itemID)
	if !parsed {
		return 0, "", "", false
	}
	switch kind {
	case emby.ItemIDTypeVideoLibrary:
		var roleN, rootN sql.NullString
		if err := d.QueryRowContext(ctx,
			"SELECT COALESCE(role, ''), COALESCE(root_path, '') FROM library WHERE id = ? AND deleted_at IS NULL LIMIT 1",
			numericID,
		).Scan(&roleN, &rootN); err != nil {
			return 0, "", "", false
		}
		return numericID, rootN.String, roleN.String, true
	case emby.ItemIDTypeVideoList:
		var lib int64
		var roleN, rootN sql.NullString
		if err := d.QueryRowContext(ctx, `
			SELECT l.id, COALESCE(l.role, ''), COALESCE(l.root_path, '')
			FROM video_list vl
			JOIN library l ON l.id = vl.video_library_id
			WHERE vl.id = ? AND vl.deleted_at IS NULL AND l.deleted_at IS NULL
			LIMIT 1`, numericID).Scan(&lib, &roleN, &rootN); err != nil {
			return 0, "", "", false
		}
		return lib, rootN.String, roleN.String, true
	}
	return 0, "", "", false
}
