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
	out, err := l.virtualFolderItems(r)
	if err != nil {
		l.log.Error("virtual folders failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}

	WriteJSON(w, http.StatusOK, out)
}

// VirtualFoldersQuery returns Emby's paginated virtual folder envelope. MoviePilot
// reads LibraryOptions.PathInfos from this endpoint when importing library paths.
func (l *Library) VirtualFoldersQuery(w http.ResponseWriter, r *http.Request) {
	out, err := l.virtualFolderItems(r)
	if err != nil {
		l.log.Error("virtual folders query failed", "err", err)
		WriteStatus(w, http.StatusInternalServerError)
		return
	}
	total := int64(len(out))
	out = pageItems(out, r.URL.Query().Get("startindex"), r.URL.Query().Get("limit"))
	WriteJSON(w, http.StatusOK, ItemResponse(out, total))
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
		out = append(out, selectableMediaFolderObject(emby.ItemID(emby.ItemIDTypeVideoLibrary, id), name, root, role))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (l *Library) virtualFolderItems(r *http.Request) ([]any, error) {
	// For the admin API key, expose all libraries. Management tools use this
	// endpoint to discover global library ids and paths.
	if ctxpkg.IsAPIKey(r.Context()) {
		rows, err := l.db.QueryContext(r.Context(), `
			SELECT id, name, COALESCE(root_path, ''), COALESCE(role, '')
			FROM library
			WHERE deleted_at IS NULL
			ORDER BY id ASC`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []any{}
		for rows.Next() {
			var id int64
			var name, root, role string
			if err := rows.Scan(&id, &name, &root, &role); err != nil {
				return nil, err
			}
			out = append(out, virtualFolderObject(emby.ItemID(emby.ItemIDTypeVideoLibrary, id), name, root, role))
		}
		return out, rows.Err()
	}

	datas, err := l.transform.GetUserLibrary(r.Context(), ctxpkg.UserID(r.Context()))
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, d := range datas {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["Id"].(string)
		name, _ := m["Name"].(string)
		root, _ := m["Path"].(string)
		collectionType, _ := m["CollectionType"].(string)
		out = append(out, virtualFolderObject(id, name, root, collectionType))
	}
	return out, nil
}

func virtualFolderObject(id, name, root, role string) map[string]any {
	root = strings.TrimSpace(root)
	if root == "" {
		// MoviePilot drops libraries whose PathInfos is empty, so always emit
		// at least one entry even before a scan has populated root_path.
		root = "/" + name
	}
	pathInfos := []any{
		map[string]any{
			"Path":        root,
			"NetworkPath": "",
		},
	}
	locations := []any{root}
	subFolders := []any{mediaSubFolder(id, name, root)}
	return map[string]any{
		"Name":      name,
		"Locations": locations,
		"LibraryOptions": map[string]any{
			"PathInfos": pathInfos,
		},
		"ItemId":         id,
		"Id":             id,
		"Guid":           id,
		"CollectionType": embyCollectionType(role),
		"SubFolders":     subFolders,
	}
}

func selectableMediaFolderObject(id, name, root, role string) map[string]any {
	root = libraryRootOrFallback(name, root)
	subFolders := []any{mediaSubFolder(id, name, root)}
	return map[string]any{
		"Name":           name,
		"Path":           root,
		"Id":             id,
		"ItemId":         id,
		"CollectionId":   id,
		"Guid":           id,
		"Type":           role,
		"CollectionType": embyCollectionType(role),
		"SubFolders":     subFolders,
		"Locations":      []any{root},
		"LibraryOptions": map[string]any{
			"PathInfos": []any{
				map[string]any{
					"Path":        root,
					"NetworkPath": "",
				},
			},
		},
	}
}

func mediaSubFolder(id, name, root string) map[string]any {
	return map[string]any{
		"Name": name,
		"Path": root,
		"Id":   id,
	}
}

func libraryRootOrFallback(name, root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return "/" + name
	}
	return root
}

func pageItems(items []any, startRaw, limitRaw string) []any {
	start := parseIntQuery(startRaw, 0)
	if start < 0 {
		start = 0
	}
	if start >= len(items) {
		return []any{}
	}
	items = items[start:]

	limit := parseIntQuery(limitRaw, 0)
	if limit > 0 && limit < len(items) {
		return items[:limit]
	}
	return items
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
