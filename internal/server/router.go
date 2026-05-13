package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Emotion/internal/server/handlers"
)

// NewRouter assembles the full chi router.
func NewRouter(deps *Dependencies) http.Handler {
	r := chi.NewRouter()
	r.Use(recoverer(deps.Logger))
	r.Use(requestLogger(deps.Logger))
	r.Use(lowercaseQuery)

	// Public routes outside the auth guard.
	sys := handlers.NewSystem(deps.Config)
	userH := handlers.NewUsers(deps.DB, deps.Config, deps.Logger)
	items := handlers.NewItems(deps.DB, deps.Cache, deps.Config, deps.Logger)
	dash := handlers.NewDashboard()

	// Visual admin dashboard. The HTML is just a static asset; it prompts for
	// an API key at runtime and calls authenticated /admin/* and /emby/* APIs
	// on the user's behalf, so we expose it without the auth guard.
	r.Get("/admin/ui", dash.Page)
	r.Get("/admin/ui/", dash.Page)
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/admin/ui", http.StatusTemporaryRedirect)
	})

	// emby paths are accepted both with and without the "/emby" prefix so native clients
	// and direct integrations both work (real Emby accepts both too).
	for _, prefix := range []string{"/emby", ""} {
		prefix := prefix

		// --- public endpoints, no auth ---
		r.Get(prefix+"/System/Info/Public", sys.InfoPublic)
		r.Get(prefix+"/System/Ping", sys.Ping)
		r.Head(prefix+"/System/Ping", sys.Ping)
		r.Get(prefix+"/Users/Public", userH.Public)
		r.Post(prefix+"/Users/AuthenticateByName", userH.AuthenticateByName)
		// Image endpoints are anonymous in emya.
		r.Get(prefix+"/Items/{itemId}/Images/{imageType}", items.Image)

		// --- authenticated endpoints ---
		r.Group(func(r chi.Router) {
			r.Use(authGuardBuilder(deps))
			registerAuthedRoutes(r, prefix, deps)
		})
	}

	return r
}

func registerAuthedRoutes(r chi.Router, prefix string, deps *Dependencies) {
	sys := handlers.NewSystem(deps.Config)
	userH := handlers.NewUsers(deps.DB, deps.Config, deps.Logger)
	items := handlers.NewItems(deps.DB, deps.Cache, deps.Config, deps.Logger)
	lib := handlers.NewLibrary(deps.DB, deps.Config, deps.Logger)
	shows := handlers.NewShows(deps.DB, deps.Config, deps.Logger)
	videos := handlers.NewVideos(deps.DB, deps.Cache, deps.Config, deps.Logger)
	sess := handlers.NewSessions(deps.DB, deps.Cache, deps.Config, deps.Logger)
	base := handlers.NewBase()
	disp := handlers.NewDisplayPreferences()
	mgmt := handlers.NewManagement(deps.DB, deps.Config, deps.Logger)

	// System
	r.Get(prefix+"/System/Info", sys.Info)
	r.Get(prefix+"/System/Ext/ServerDomains", sys.ExtServerDomains)

	// Base catalog stubs
	r.Get(prefix+"/Persons", base.Empty)
	r.Get(prefix+"/Genres", base.Empty)
	r.Get(prefix+"/Tags", base.Empty)
	r.Get(prefix+"/OfficialRatings", base.Empty)
	r.Get(prefix+"/Years", base.Empty)
	r.Get(prefix+"/Studios", base.Empty)

	// DisplayPreferences
	r.Get(prefix+"/DisplayPreferences/Usersettings", disp.Usersettings)

	// Users
	r.Get(prefix+"/Users", mgmt.UsersList)
	r.Get(prefix+"/Users/Query", mgmt.UsersQuery)
	r.Post(prefix+"/Users/New", mgmt.UserNew)
	r.Get(prefix+"/Users/{userId}", userH.Base)
	r.Delete(prefix+"/Users/{userId}", mgmt.UserDelete)
	r.Post(prefix+"/Users/{userId}/Password", mgmt.UserPassword)
	r.Post(prefix+"/Users/{userId}/Policy", mgmt.UserPolicy)
	r.Get(prefix+"/Users/{userId}/Views", userH.Views)
	r.Get(prefix+"/Users/{userId}/Items", userH.Items)
	r.Get(prefix+"/Users/{userId}/Items/Resume", userH.ItemsResume)
	r.Get(prefix+"/Users/{userId}/Items/Latest", userH.ItemsLatest)
	r.Get(prefix+"/Users/{userId}/Items/{itemId}", userH.ItemInfo)
	r.Get(prefix+"/Users/{userId}/Items/{itemId}/LocalTrailers", userH.EmptyArray)
	r.Get(prefix+"/Users/{userId}/Items/{itemId}/SpecialFeatures", userH.EmptyArray)
	r.Post(prefix+"/Users/{userId}/Items/{itemId}/HideFromResume", userH.HideFromResume)
	r.Post(prefix+"/Users/{userId}/FavoriteItems/{itemId}", userH.Favorite)
	r.Delete(prefix+"/Users/{userId}/FavoriteItems/{itemId}", userH.Favorite)
	r.Post(prefix+"/Users/{userId}/PlayedItems/{itemId}", userH.Played)
	r.Delete(prefix+"/Users/{userId}/PlayedItems/{itemId}", userH.Played)

	// Library
	r.Get(prefix+"/Library/MediaFolders", lib.MediaFolders)
	r.Get(prefix+"/Library/VirtualFolders", lib.VirtualFolders)

	// Items (top-level)
	r.Get(prefix+"/Items", items.Items)
	r.Get(prefix+"/Items/Counts", items.Counts)
	r.Get(prefix+"/Items/{itemId}/Similar", items.Similar)
	r.Get(prefix+"/Items/{itemId}/PlaybackInfo", items.PlaybackInfo)
	r.Post(prefix+"/Items/{itemId}/PlaybackInfo", items.PlaybackInfo)

	// Shows
	r.Get(prefix+"/Shows/NextUp", shows.NextUp)
	r.Get(prefix+"/Shows/{itemId}/Seasons", shows.Seasons)
	r.Get(prefix+"/Shows/{itemId}/Episodes", shows.Episodes)

	// Videos
	r.Get(prefix+"/Videos/{mediaUUID}/AdditionalParts", items.Similar)
	r.Get(prefix+"/Videos/{mediaUUID}/Subtitles/{subtitleId}", videos.Subtitle)
	r.Get(prefix+"/Videos/{mediaUUID}/{mediaName}", videos.Play)

	// Sessions
	r.Get(prefix+"/Sessions", sess.List)
	r.Post(prefix+"/Sessions/Playing", sess.Playing)
	r.Post(prefix+"/Sessions/Playing/Progress", sess.Playing)
	r.Post(prefix+"/Sessions/Playing/Stopped", sess.Playing)
	r.Post(prefix+"/Sessions/Playing/Ping", sess.Ping)
	r.Post(prefix+"/Sessions/Capabilities/Full", sess.Capabilities)
	r.Post(prefix+"/Sessions/{sessionId}/Message", mgmt.SessionMessage)
	r.Post(prefix+"/Sessions/{sessionId}/Playing/Stop", mgmt.SessionStop)

	// Devices (sakura-embyboss)
	r.Get(prefix+"/Devices/Info", mgmt.DeviceInfo)

	// user_usage_stats (sakura-embyboss)
	r.Post(prefix+"/user_usage_stats/submit_custom_query", mgmt.UsageStatsQuery)

	// --- admin (manual library management) ---
	// These sit under /admin (no /emby prefix parity — they're native to Emotion).
	if prefix == "" {
		admin := handlers.NewAdmin(deps.DB, deps.Config, deps.Logger)
		r.Get("/admin/libraries", admin.LibrariesList)
		r.Post("/admin/libraries", admin.LibraryCreate)
		r.Delete("/admin/libraries/{id}", admin.LibraryDelete)
		r.Get("/admin/media", admin.AdminMediaList)
		r.Get("/admin/media/{id}/children", admin.AdminMediaChildren)
		r.Post("/admin/library/scan", admin.LibraryScan)
		r.Post("/admin/items/{id}/tmdb/refresh", admin.TMDBRefreshOne)
		r.Post("/admin/tmdb/refresh-all", admin.TMDBRefreshAll)
	}
}
