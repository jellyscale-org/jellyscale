package api

import "net/http"

// Routes is the source of truth for the API surface. compat/endpoints.yaml is
// generated from it and a test keeps the two in step.
func (s *Server) Routes() []Route {
	return []Route{
		// System.
		{Method: "GET", Path: "/System/Info/Public", Status: Implemented, Role: RoleAPI, Access: AccessAnyone, Why: "a client asks who this server is before anyone has signed in", Handler: s.systemInfoPublic},
		{Method: "GET", Path: "/System/Info", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.systemInfo},
		{Method: "GET", Path: "/System/Ping", Status: Implemented, Role: RoleAPI, Access: AccessAnyone, Why: "a client and a load balancer probe that the server answers, with no account", Handler: s.ping},
		{Method: "POST", Path: "/System/Ping", Status: Implemented, Role: RoleAPI, Access: AccessAnyone, Why: "a client and a load balancer probe that the server answers, with no account", Handler: s.ping},
		// On the stream role, with the subtitles it comes before: jellyfin-web reads it before it asks for an ASS
		// track, so a viewer in the snapshot gets the track in an outage only if this is answered too.
		{Method: "GET", Path: "/System/Configuration/encoding", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: encodingConfiguration, Snapshot: true},
		{Method: "GET", Path: "/System/Endpoint", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.systemEndpoint},
		{Method: "GET", Path: "/Branding/Configuration", Status: Stub, Role: RoleAPI, Access: AccessAnyone, Why: "the sign-in page shows the branding before anyone has signed in", Handler: s.branding},
		{Method: "GET", Path: "/Branding/Css", Status: Stub, Role: RoleAPI, Access: AccessAnyone, Why: "the sign-in page loads the branding before anyone has signed in", Handler: noContent},
		{Method: "GET", Path: "/Branding/Css.css", Status: Stub, Role: RoleAPI, Access: AccessAnyone, Why: "the sign-in page loads the branding before anyone has signed in", Handler: noContent},
		{Method: "GET", Path: "/QuickConnect/Enabled", Status: Implemented, Role: RoleAPI, Access: AccessAnyone, Why: "the sign-in page asks whether Quick Connect is offered before anyone has signed in", Handler: s.quickConnectEnabled},

		// Users and sessions.
		{Method: "GET", Path: "/Users/Public", Status: Implemented, Role: RoleAPI, Access: AccessAnyone, Why: "the sign-in page asks for the users to pick from before anyone has signed in; it answers none", Handler: emptyList},
		{Method: "POST", Path: "/Users/AuthenticateByName", Status: Implemented, Role: RoleAPI, Access: AccessAnyone, Why: "it is how a viewer signs in, and it checks the name and password itself", Handler: s.authenticateByName},
		// A device with no token asks for a six-digit code, polls it and redeems it with the secret; only an account
		// that is already signed in approves one (JS-037).
		{Method: "POST", Path: "/QuickConnect/Initiate", Status: Implemented, Role: RoleAPI, Access: AccessAnyone, Why: "a device with no token asks for the code it shows its viewer; the secret is its own answer", Handler: s.quickConnectInitiate},
		{Method: "GET", Path: "/QuickConnect/Connect", Status: Implemented, Role: RoleAPI, Access: AccessAnyone, Why: "a device with no token asks whether its code was approved; the answer carries no sign-in", Handler: s.quickConnectConnect},
		{Method: "POST", Path: "/QuickConnect/Authorize", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.quickConnectAuthorize},
		{Method: "POST", Path: "/Users/AuthenticateWithQuickConnect", Status: Implemented, Role: RoleAPI, Access: AccessAnyone, Why: "a device with no token redeems its secret for the token the approval made for it", Handler: s.authenticateWithQuickConnect},
		{Method: "GET", Path: "/Users/Me", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.currentUser},
		{Method: "GET", Path: "/Users/{userId}", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Users: UsersAdminAny, Handler: s.getUser},
		// User management (JS-033). A viewer lists, renames and changes the password of their own account only.
		{Method: "GET", Path: "/Users", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Users: UsersAdminAny, Handler: s.listUsers},
		{Method: "POST", Path: "/Users", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Users: UsersAdminAny, Handler: s.updateUser},
		{Method: "POST", Path: "/Users/Password", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Users: UsersAdminAny, Handler: s.changePassword},
		// jellyfin-web's password page still calls it (compat/captures/users-admin-12.4.md).
		{Method: "POST", Path: "/Users/{userId}/Password", Status: Alias, Role: RoleAPI, Access: AccessViewer, Users: UsersAdminAny, Handler: s.changePassword},
		{Method: "POST", Path: "/Users/New", Status: Implemented, Role: RoleAPI, Access: AccessAdmin, Users: UsersAdminAny, Handler: s.newUser},
		{Method: "DELETE", Path: "/Users/{userId}", Status: Implemented, Role: RoleAPI, Access: AccessAdmin, Users: UsersAdminAny, Handler: s.deleteUser},
		{Method: "POST", Path: "/Users/{userId}/Policy", Status: Implemented, Role: RoleAPI, Access: AccessAdmin, Users: UsersAdminAny, Handler: s.setPolicy},
		{Method: "GET", Path: "/Auth/Providers", Status: Stub, Role: RoleAPI, Access: AccessAdmin, Users: UsersAdminAny, Handler: authenticationProviders},
		{Method: "GET", Path: "/Auth/PasswordResetProviders", Status: Stub, Role: RoleAPI, Access: AccessAdmin, Users: UsersAdminAny, Handler: passwordResetProviders},
		// What jellyfin-web's user pages also read.
		{Method: "GET", Path: "/Library/MediaFolders", Status: Implemented, Role: RoleAPI, Access: AccessAdmin, Handler: s.mediaFolders},
		{Method: "GET", Path: "/Devices", Status: Implemented, Role: RoleAPI, Access: AccessAdmin, Handler: s.listDevices},
		{Method: "GET", Path: "/Channels", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/Localization/ParentalRatings", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyList},
		{Method: "POST", Path: "/Users/Configuration", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Users: UsersAdminAny, Handler: s.saveConfiguration},
		{Method: "POST", Path: "/Users/{userId}/Configuration", Status: Alias, Role: RoleAPI, Access: AccessViewer, Users: UsersAdminAny, Handler: s.saveConfiguration},
		// jellyscale's own namespace: Jellyfin has no place for these (docs/api.md). Each acts on the caller only.
		{Method: "GET", Path: "/jellyscale/v1/track-preferences", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.getTrackPreferences},
		{Method: "PATCH", Path: "/jellyscale/v1/track-preferences", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.patchTrackPreferences},
		{Method: "POST", Path: "/jellyscale/v1/items/{itemId}/playback-info", Status: Implemented, Role: RoleAPI, Access: AccessViewer, UserIDBody: true, Handler: s.markedPlaybackInfo},
		{Method: "PATCH", Path: "/jellyscale/v1/users/configuration", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.patchConfiguration},
		{Method: "GET", Path: "/Localization/Cultures", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: listCultures},
		{Method: "POST", Path: "/Sessions/Logout", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.logout},
		{Method: "POST", Path: "/Sessions/Capabilities", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: noContent},
		{Method: "POST", Path: "/Sessions/Capabilities/Full", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: noContent},
		{Method: "GET", Path: "/DisplayPreferences/{preferenceId}", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.getDisplayPreferences},
		{Method: "POST", Path: "/DisplayPreferences/{preferenceId}", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.setDisplayPreferences},

		// Items.
		{Method: "GET", Path: "/UserViews", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.userViews},
		{Method: "GET", Path: "/UserViews/GroupingOptions", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyList},
		{Method: "GET", Path: "/Items", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.listItems},
		{Method: "GET", Path: "/Items/Latest", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.latestItems},
		{Method: "GET", Path: "/Search/Hints", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.searchHints},
		{Method: "GET", Path: "/Items/Suggestions", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.suggestedItems},
		{Method: "GET", Path: "/Items/Filters", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: s.itemFilters},
		{Method: "GET", Path: "/Items/Filters2", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: s.itemFilters},
		{Method: "GET", Path: "/Items/{itemId}", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.getItem},
		{Method: "GET", Path: "/Items/{itemId}/Ancestors", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.itemAncestors},
		{Method: "GET", Path: "/Items/{itemId}/Similar", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/Items/{itemId}/ThemeMedia", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: s.themeMedia},
		{Method: "GET", Path: "/Items/{itemId}/Collections", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/Items/{itemId}/SpecialFeatures", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyList},
		{Method: "GET", Path: "/Items/{itemId}/LocalTrailers", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyList},
		{Method: "GET", Path: "/UserItems/Resume", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.resumeItems},
		{Method: "GET", Path: "/Shows/NextUp", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		// Every SDK client asks before playback; an empty list until JS-052 finds intros and credits.
		{Method: "GET", Path: "/MediaSegments/{itemId}", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/Shows/{seriesId}/Seasons", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.seasons},
		{Method: "GET", Path: "/Shows/{seriesId}/Episodes", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.episodes},
		{Method: "GET", Path: "/Genres", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/Studios", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/Artists", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/Persons", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},

		// Out of scope: valid empty answers so the web client's home page loads.
		{Method: "GET", Path: "/LiveTv/Programs", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/LiveTv/Programs/Recommended", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/SyncPlay/List", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyList},
		{Method: "GET", Path: "/Plugins", Status: Stub, Role: RoleAPI, Access: AccessViewer, Handler: emptyList},

		// Playback. Media is served by the stream role. A GET pattern answers HEAD
		// too, and a separate HEAD pattern would collide with the literal
		// siblings below. The stream route takes one wildcard because patterns
		// can't match part of a segment; the handler accepts "stream" and
		// "stream.{container}". Literal siblings, such as master.m3u8, win.
		{Method: "GET", Path: "/Videos/{itemId}/master.m3u8", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.masterPlaylist},
		{Method: "GET", Path: "/Videos/{itemId}/main.m3u8", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.mainPlaylist},
		// Jellyfin's pattern is hls1/{playlistId}/{segmentId}.{container}; the
		// handler splits the file name, since patterns can't match part of one.
		// The only playlist is main, and naming it keeps this pattern from
		// overlapping the attachments one below, which Go's mux refuses.
		{Method: "GET", Path: "/Videos/{itemId}/hls1/main/{segmentId}", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.hlsSegment, Snapshot: true},
		{Method: "DELETE", Path: "/Videos/ActiveEncodings", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.deleteActiveEncodings},
		{Method: "GET", Path: "/Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.subtitleStream, Snapshot: true},
		// Without a start offset the name is Stream.{format}, or subtitles.m3u8,
		// the playlist of segments that are requests for Stream.vtt.
		{Method: "GET", Path: "/Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.subtitleStream, Snapshot: true},
		// jellyfin-web fetches the fonts for ASS from the DeliveryUrl of a media
		// source's attachment, which carries the token (Jellyfin's does not).
		{Method: "GET", Path: "/Videos/{itemId}/{sourceId}/Attachments/{index}", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.attachment, Snapshot: true},
		{Method: "GET", Path: "/Videos/{itemId}/{file}", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.stream, Snapshot: true},
		{Method: "GET", Path: "/Items/{itemId}/PlaybackInfo", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.playbackInfo},
		{Method: "POST", Path: "/Items/{itemId}/PlaybackInfo", Status: Implemented, Role: RoleAPI, Access: AccessViewer, UserIDBody: true, Handler: s.playbackInfo},
		{Method: "GET", Path: "/Items/{itemId}/Download", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.download},
		{Method: "GET", Path: "/Playback/BitrateTest", Status: Implemented, Role: RoleStream, Access: AccessViewer, Handler: s.bitrateTest},
		{Method: "POST", Path: "/Sessions/Playing", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.playbackStarted},
		{Method: "POST", Path: "/Sessions/Playing/Progress", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.playbackProgress},
		{Method: "POST", Path: "/Sessions/Playing/Stopped", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.playbackStopped},
		{Method: "POST", Path: "/Sessions/Playing/Ping", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.playbackPing},
		{Method: "GET", Path: "/Sessions", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.listSessions},
		{Method: "GET", Path: "/socket", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.socket},
		{Method: "POST", Path: "/UserItems/{itemId}/UserData", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Users: UsersAdminAny, Handler: s.setUserData},
		{Method: "POST", Path: "/UserPlayedItems/{itemId}", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.markPlayed(true)},
		{Method: "DELETE", Path: "/UserPlayedItems/{itemId}", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.markPlayed(false)},
		{Method: "POST", Path: "/UserFavoriteItems/{itemId}", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.markFavorite(true)},
		{Method: "DELETE", Path: "/UserFavoriteItems/{itemId}", Status: Implemented, Role: RoleAPI, Access: AccessViewer, Handler: s.markFavorite(false)},

		// Images are served by the stream role, because they scale with traffic.
		{Method: "GET", Path: "/Items/{itemId}/Images/{imageType}", Status: Implemented, Role: RoleStream, Access: AccessAnyone, Why: "a picture is open to whoever holds the item id (random), because an <img> cannot send a token; a picture by name needs one", Handler: s.itemImage},
		{Method: "GET", Path: "/Items/{itemId}/Images/{imageType}/{imageIndex}", Status: Implemented, Role: RoleStream, Access: AccessAnyone, Why: "a picture is open to whoever holds the item id (random), because an <img> cannot send a token; a picture by name needs one", Handler: s.itemImage},

		// Obsolete routes jellyfin-web still calls through jellyfin-apiclient.
		// Each is one line over a current handler. See spikes/README.md.
		{Method: "GET", Path: "/Users/{userId}/Items", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: s.listItems},
		{Method: "GET", Path: "/Users/{userId}/Items/Latest", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: s.latestItems},
		{Method: "GET", Path: "/Users/{userId}/Items/Resume", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: s.resumeItems},
		{Method: "GET", Path: "/Users/{userId}/Items/Root", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: s.rootFolder},
		{Method: "GET", Path: "/Users/{userId}/Items/{itemId}", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: s.getItem},
		{Method: "GET", Path: "/Users/{userId}/Items/{itemId}/Intros", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: emptyResult},
		{Method: "GET", Path: "/Users/{userId}/Items/{itemId}/LocalTrailers", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: emptyList},
		{Method: "GET", Path: "/Users/{userId}/Items/{itemId}/SpecialFeatures", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: emptyList},
		// These Jellyfin aliases act on the caller; the guard refuses another user's id (JS-127).
		{Method: "POST", Path: "/Users/{userId}/PlayedItems/{itemId}", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: s.markPlayed(true)},
		{Method: "DELETE", Path: "/Users/{userId}/PlayedItems/{itemId}", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: s.markPlayed(false)},
		{Method: "POST", Path: "/Users/{userId}/FavoriteItems/{itemId}", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: s.markFavorite(true)},
		{Method: "DELETE", Path: "/Users/{userId}/FavoriteItems/{itemId}", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: s.markFavorite(false)},
		{Method: "GET", Path: "/Users/{userId}/GroupingOptions", Status: Alias, Role: RoleAPI, Access: AccessViewer, Handler: emptyList},
	}
}

func noContent(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func emptyList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, []any{})
}
