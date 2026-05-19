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
	admin := handlers.NewAdmin(deps.DB, deps.Config, deps.Logger)

	// Visual admin dashboard. The HTML is a public asset; /admin/login validates
	// the bootstrap admin secret and returns a dashboard session token.
	r.Post("/admin/login", admin.Login)
	r.Get("/admin/ui", dash.Page)
	r.Get("/admin/ui/", dash.Page)
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/admin/ui", http.StatusTemporaryRedirect)
	})

	r.Get("/web", dash.WebStub)
	r.Get("/web/", dash.WebStub)
	r.Get("/web/index.html", dash.WebStub)

	// Emby paths are accepted both with and without the "/emby" prefix so native
	// clients and direct integrations both work. "/emby/emby" is tolerated for
	// MoviePilot setups where the base URL already includes /emby while MP also
	// appends emby/ for several endpoints.
	for _, prefix := range []string{"/emby/emby", "/emby", ""} {
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
			registerAuthedRoutes(r, prefix, deps, admin)
		})
	}

	return r
}

func registerAuthedRoutes(r chi.Router, prefix string, deps *Dependencies, admin *handlers.Admin) {
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
	subs := handlers.NewSubscriptions(deps.DB, deps.Logger)

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

	// Series subscriptions (追更)
	r.Post(prefix+"/Users/{userId}/SubscribedSeries/{itemId}", subs.Subscribe)
	r.Delete(prefix+"/Users/{userId}/SubscribedSeries/{itemId}", subs.Unsubscribe)
	r.Get(prefix+"/Users/{userId}/SubscribedSeries", subs.List)

	// Library
	r.Get(prefix+"/Library/MediaFolders", lib.MediaFolders)
	r.Get(prefix+"/Library/VirtualFolders/Query", lib.VirtualFoldersQuery)
	r.Get(prefix+"/Library/VirtualFolders", lib.VirtualFolders)
	r.Get(prefix+"/Library/SelectableMediaFolders", lib.SelectableMediaFolders)
	r.Post(prefix+"/Library/Refresh", admin.EmbyLibraryRefresh)
	r.Post(prefix+"/Library/Media/Updated", admin.EmbyLibraryRefresh)

	// Items (top-level)
	r.Get(prefix+"/Items", items.Items)
	r.Get(prefix+"/Items/Counts", items.Counts)
	r.Get(prefix+"/Items/{itemId}", items.ItemInfo)
	r.Get(prefix+"/Items/{itemId}/Similar", items.Similar)
	r.Get(prefix+"/Items/{itemId}/PlaybackInfo", items.PlaybackInfo)
	r.Post(prefix+"/Items/{itemId}/PlaybackInfo", items.PlaybackInfo)
	r.Post(prefix+"/Items/{itemId}/Refresh", admin.EmbyItemRefresh)

	// Shows
	r.Get(prefix+"/Shows/NextUp", shows.NextUp)
	r.Get(prefix+"/Shows/{itemId}/Seasons", shows.Seasons)
	r.Get(prefix+"/Shows/{itemId}/Episodes", shows.Episodes)

	// Videos
	r.Get(prefix+"/Videos/{mediaUUID}/AdditionalParts", items.Similar)
	r.Get(prefix+"/Videos/{mediaUUID}/Subtitles/{subtitleId}", videos.Subtitle)
	r.Get(prefix+"/Videos/{mediaUUID}/{mediaName}", videos.Play)
	r.Get(prefix+"/videos/{mediaUUID}/AdditionalParts", items.Similar)
	r.Get(prefix+"/videos/{mediaUUID}/subtitles/{subtitleId}", videos.Subtitle)
	r.Get(prefix+"/videos/{mediaUUID}/{mediaName}", videos.Play)

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
		r.Get("/admin/libraries", admin.LibrariesList)
		r.Post("/admin/libraries", admin.LibraryCreate)
		r.Patch("/admin/libraries/{id}", admin.LibraryUpdate)
		r.Delete("/admin/libraries/{id}", admin.LibraryDelete)
		r.Get("/admin/files", admin.FilesBrowse)
		r.Get("/admin/media", admin.AdminMediaList)
		r.Get("/admin/media/stats", admin.AdminMediaStats)
		r.Post("/admin/media/probe/start", admin.MediaProbeStart)
		r.Get("/admin/media/probe/{id}", admin.MediaProbeStatus)
		r.Patch("/admin/media/{id}", admin.AdminMediaUpdate)
		r.Get("/admin/media/{id}/children", admin.AdminMediaChildren)
		r.Get("/admin/logs", admin.Logs)
		r.Patch("/admin/users/{userId}", mgmt.AdminUserUpdate)
		r.Get("/admin/api-keys", admin.APIKeysList)
		r.Post("/admin/api-keys", admin.APIKeyCreate)
		r.Delete("/admin/api-keys/{id}", admin.APIKeyRevoke)
		r.Get("/admin/tmdb/settings", admin.TMDBSettingsGet)
		r.Post("/admin/tmdb/settings", admin.TMDBSettingsUpdate)
		r.Post("/admin/tmdb/settings/test", admin.TMDBSettingsTest)
		r.Post("/admin/library/scan", admin.LibraryScan)
		r.Post("/admin/library/scan/start", admin.LibraryScanStart)
		r.Get("/admin/library/scan/{id}", admin.LibraryScanStatus)
		r.Post("/admin/library/watch/start", admin.LibraryWatchStart)
		r.Get("/admin/library/watch", admin.LibraryWatchStatus)
		r.Get("/admin/library/watch/{id}", admin.LibraryWatchStatus)
		r.Delete("/admin/library/watch/{id}", admin.LibraryWatchStop)
		r.Post("/admin/items/{id}/tmdb/refresh", admin.TMDBRefreshOne)
		r.Post("/admin/tmdb/refresh-all", admin.TMDBRefreshAll)
		r.Post("/admin/tmdb/refresh-all/start", admin.TMDBRefreshAllStart)
		r.Get("/admin/tmdb/refresh-all/{id}", admin.TMDBRefreshAllStatus)

		// Series subscription events (bot polling)
		r.Get("/admin/series-events", subs.EventsPending)
		r.Post("/admin/series-events/ack", subs.EventsAck)
	}
}
