package api

import "net/http"

// Routes is the source of truth for the API surface. compat/endpoints.yaml is
// generated from it and a test keeps the two in step.
func (s *Server) Routes() []Route {
	return []Route{
		// System.
		{Method: "GET", Path: "/System/Info/Public", Status: Implemented, Role: RoleAPI, Public: true, Handler: s.systemInfoPublic},
		{Method: "GET", Path: "/System/Info", Status: Implemented, Role: RoleAPI, Handler: s.systemInfo},
		{Method: "GET", Path: "/System/Ping", Status: Implemented, Role: RoleAPI, Public: true, Handler: s.ping},
		{Method: "POST", Path: "/System/Ping", Status: Implemented, Role: RoleAPI, Public: true, Handler: s.ping},
		{Method: "GET", Path: "/System/Endpoint", Status: Implemented, Role: RoleAPI, Handler: s.systemEndpoint},
		{Method: "GET", Path: "/Branding/Configuration", Status: Stub, Role: RoleAPI, Public: true, Handler: s.branding},
		{Method: "GET", Path: "/Branding/Css", Status: Stub, Role: RoleAPI, Public: true, Handler: noContent},
		{Method: "GET", Path: "/Branding/Css.css", Status: Stub, Role: RoleAPI, Public: true, Handler: noContent},
		{Method: "GET", Path: "/QuickConnect/Enabled", Status: Stub, Role: RoleAPI, Public: true, Handler: quickConnectDisabled},

		// Users and sessions.
		{Method: "GET", Path: "/Users/Public", Status: Implemented, Role: RoleAPI, Public: true, Handler: emptyList},
		{Method: "POST", Path: "/Users/AuthenticateByName", Status: Implemented, Role: RoleAPI, Public: true, Handler: s.authenticateByName},
		{Method: "GET", Path: "/Users/Me", Status: Implemented, Role: RoleAPI, Handler: s.currentUser},
		{Method: "GET", Path: "/Users/{userId}", Status: Implemented, Role: RoleAPI, Handler: s.getUser},
		{Method: "POST", Path: "/Sessions/Logout", Status: Implemented, Role: RoleAPI, Handler: s.logout},
		{Method: "POST", Path: "/Sessions/Capabilities", Status: Stub, Role: RoleAPI, Handler: noContent},
		{Method: "POST", Path: "/Sessions/Capabilities/Full", Status: Stub, Role: RoleAPI, Handler: noContent},
		{Method: "GET", Path: "/DisplayPreferences/{preferenceId}", Status: Implemented, Role: RoleAPI, Handler: s.getDisplayPreferences},
		{Method: "POST", Path: "/DisplayPreferences/{preferenceId}", Status: Implemented, Role: RoleAPI, Handler: s.setDisplayPreferences},

		// Items.
		{Method: "GET", Path: "/UserViews", Status: Implemented, Role: RoleAPI, Handler: s.userViews},
		{Method: "GET", Path: "/UserViews/GroupingOptions", Status: Stub, Role: RoleAPI, Handler: emptyList},
		{Method: "GET", Path: "/Items", Status: Implemented, Role: RoleAPI, Handler: s.listItems},
		{Method: "GET", Path: "/Items/Latest", Status: Implemented, Role: RoleAPI, Handler: s.latestItems},
		{Method: "GET", Path: "/Items/Filters", Status: Stub, Role: RoleAPI, Handler: s.itemFilters},
		{Method: "GET", Path: "/Items/Filters2", Status: Stub, Role: RoleAPI, Handler: s.itemFilters},
		{Method: "GET", Path: "/Items/{itemId}", Status: Implemented, Role: RoleAPI, Handler: s.getItem},
		{Method: "GET", Path: "/Items/{itemId}/Ancestors", Status: Implemented, Role: RoleAPI, Handler: s.itemAncestors},
		{Method: "GET", Path: "/Items/{itemId}/Similar", Status: Stub, Role: RoleAPI, Handler: emptyResult},
		{Method: "GET", Path: "/Items/{itemId}/ThemeMedia", Status: Stub, Role: RoleAPI, Handler: s.themeMedia},
		{Method: "GET", Path: "/Items/{itemId}/Collections", Status: Stub, Role: RoleAPI, Handler: emptyList},
		{Method: "GET", Path: "/Items/{itemId}/SpecialFeatures", Status: Stub, Role: RoleAPI, Handler: emptyList},
		{Method: "GET", Path: "/Items/{itemId}/LocalTrailers", Status: Stub, Role: RoleAPI, Handler: emptyList},
		{Method: "GET", Path: "/UserItems/Resume", Status: Implemented, Role: RoleAPI, Handler: s.resumeItems},
		{Method: "GET", Path: "/Shows/NextUp", Status: Stub, Role: RoleAPI, Handler: emptyResult},
		{Method: "GET", Path: "/Shows/{seriesId}/Seasons", Status: Implemented, Role: RoleAPI, Handler: s.seasons},
		{Method: "GET", Path: "/Shows/{seriesId}/Episodes", Status: Implemented, Role: RoleAPI, Handler: s.episodes},
		{Method: "GET", Path: "/Genres", Status: Stub, Role: RoleAPI, Handler: emptyResult},
		{Method: "GET", Path: "/Studios", Status: Stub, Role: RoleAPI, Handler: emptyResult},
		{Method: "GET", Path: "/Persons", Status: Stub, Role: RoleAPI, Handler: emptyResult},

		// Out of scope: valid empty answers so the web client's home page loads.
		{Method: "GET", Path: "/LiveTv/Programs", Status: Stub, Role: RoleAPI, Handler: emptyResult},
		{Method: "GET", Path: "/LiveTv/Programs/Recommended", Status: Stub, Role: RoleAPI, Handler: emptyResult},
		{Method: "GET", Path: "/SyncPlay/List", Status: Stub, Role: RoleAPI, Handler: emptyList},
		{Method: "GET", Path: "/Plugins", Status: Stub, Role: RoleAPI, Handler: emptyList},

		// Playback. Media is served by the stream role. The stream route takes one
		// wildcard because patterns can't match part of a segment; the handler
		// accepts "stream" and "stream.{container}". Literal siblings, such as
		// master.m3u8 later, win over it.
		{Method: "GET", Path: "/Videos/{itemId}/{file}", Status: Implemented, Role: RoleStream, Handler: s.stream},
		{Method: "HEAD", Path: "/Videos/{itemId}/{file}", Status: Implemented, Role: RoleStream, Handler: s.stream},
		{Method: "GET", Path: "/Items/{itemId}/PlaybackInfo", Status: Implemented, Role: RoleAPI, Handler: s.playbackInfo},
		{Method: "POST", Path: "/Items/{itemId}/PlaybackInfo", Status: Implemented, Role: RoleAPI, Handler: s.playbackInfo},
		{Method: "GET", Path: "/Playback/BitrateTest", Status: Implemented, Role: RoleStream, Handler: s.bitrateTest},
		{Method: "POST", Path: "/Sessions/Playing", Status: Implemented, Role: RoleAPI, Handler: s.playbackStarted},
		{Method: "POST", Path: "/Sessions/Playing/Progress", Status: Implemented, Role: RoleAPI, Handler: s.playbackProgress},
		{Method: "POST", Path: "/Sessions/Playing/Stopped", Status: Implemented, Role: RoleAPI, Handler: s.playbackStopped},
		{Method: "POST", Path: "/Sessions/Playing/Ping", Status: Implemented, Role: RoleAPI, Handler: s.playbackPing},
		{Method: "GET", Path: "/Sessions", Status: Implemented, Role: RoleAPI, Handler: s.listSessions},
		{Method: "GET", Path: "/socket", Status: Implemented, Role: RoleAPI, Handler: s.socket},
		{Method: "POST", Path: "/UserPlayedItems/{itemId}", Status: Implemented, Role: RoleAPI, Handler: s.markPlayed(true)},
		{Method: "DELETE", Path: "/UserPlayedItems/{itemId}", Status: Implemented, Role: RoleAPI, Handler: s.markPlayed(false)},
		{Method: "POST", Path: "/UserFavoriteItems/{itemId}", Status: Implemented, Role: RoleAPI, Handler: s.markFavorite(true)},
		{Method: "DELETE", Path: "/UserFavoriteItems/{itemId}", Status: Implemented, Role: RoleAPI, Handler: s.markFavorite(false)},

		// Images are served by the stream role, because they scale with traffic.
		{Method: "GET", Path: "/Items/{itemId}/Images/{imageType}", Status: Implemented, Role: RoleStream, Public: true, Handler: s.itemImage},
		{Method: "GET", Path: "/Items/{itemId}/Images/{imageType}/{imageIndex}", Status: Implemented, Role: RoleStream, Public: true, Handler: s.itemImage},

		// Obsolete routes jellyfin-web still calls through jellyfin-apiclient.
		// Each is one line over a current handler. See spikes/README.md.
		{Method: "GET", Path: "/Users/{userId}/Items", Status: Alias, Role: RoleAPI, Handler: s.listItems},
		{Method: "GET", Path: "/Users/{userId}/Items/Latest", Status: Alias, Role: RoleAPI, Handler: s.latestItems},
		{Method: "GET", Path: "/Users/{userId}/Items/Resume", Status: Alias, Role: RoleAPI, Handler: s.resumeItems},
		{Method: "GET", Path: "/Users/{userId}/Items/Root", Status: Alias, Role: RoleAPI, Handler: s.rootFolder},
		{Method: "GET", Path: "/Users/{userId}/Items/{itemId}", Status: Alias, Role: RoleAPI, Handler: s.getItem},
		{Method: "GET", Path: "/Users/{userId}/Items/{itemId}/Intros", Status: Alias, Role: RoleAPI, Handler: emptyResult},
		{Method: "GET", Path: "/Users/{userId}/Items/{itemId}/LocalTrailers", Status: Alias, Role: RoleAPI, Handler: emptyList},
		{Method: "GET", Path: "/Users/{userId}/Items/{itemId}/SpecialFeatures", Status: Alias, Role: RoleAPI, Handler: emptyList},
		// These act on the caller whatever userId says.
		{Method: "POST", Path: "/Users/{userId}/PlayedItems/{itemId}", Status: Alias, Role: RoleAPI, Handler: s.markPlayed(true)},
		{Method: "DELETE", Path: "/Users/{userId}/PlayedItems/{itemId}", Status: Alias, Role: RoleAPI, Handler: s.markPlayed(false)},
		{Method: "POST", Path: "/Users/{userId}/FavoriteItems/{itemId}", Status: Alias, Role: RoleAPI, Handler: s.markFavorite(true)},
		{Method: "DELETE", Path: "/Users/{userId}/FavoriteItems/{itemId}", Status: Alias, Role: RoleAPI, Handler: s.markFavorite(false)},
		{Method: "GET", Path: "/Users/{userId}/GroupingOptions", Status: Alias, Role: RoleAPI, Handler: emptyList},
	}
}

func noContent(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func emptyList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, []any{})
}
