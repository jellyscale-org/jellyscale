# Access

Who may call each route of jellyscale, and the rule behind it. The model is decided by the owner (2026-11-16): playback and personal data always require authorization, and compatibility never means copying Jellyfin's openings. [DOMAINS.md](../DOMAINS.md) holds it as an invariant.

## The model

- Every route declares its access in `internal/api/routes.go` (`Route.Access`), and the declaration is printed on the route's line of `compat/endpoints.yaml`. There are two words:
  - `viewer`: the request carries a token that Postgres knows, in any of the seven spellings `compat.ParseAuth` reads. Without one the answer is 401 with the body `unauthorized`, the same for every route and every cause (no token, an unknown token, a signed-out token, a deleted user, a replaced device).
  - `anyone`: no token is needed. The route says why (`Route.Why`).
- None is open by default: a route with no `Access`, or a word that is neither, stops `Server.Handler()` from building, whichever roles the server has, so no binary starts.
- Media, item data and anything that names a user are never open. A client must send its token on every media request.
- Pictures by id are the one content open to whoever holds the id, by the owner's decision: jellyfin-web builds picture URLs with no token and an `<img>` cannot send one, and item ids are random. A picture by name needs a token.
- Postgres decides while it answers. The snapshot of active sessions only lets a `Snapshot` route go on for a token it holds while Postgres is away (DOMAINS invariant 37).
- Opening a route takes a change of the open list in `internal/api/access_test.go`, which a reviewer sees.
- A viewer sees every film today; per-user libraries are JS-036. A request naming another user's id is answered as the caller's own today; answering it 403 is JS-127. The `admin` word joins with its first route in JS-033.

## Served outside the table

- `GET /healthz`: liveness probe, the process only. Open to anyone.
- `GET /readyz`: readiness probe for the router. Open to anyone.
- `GET /web/`: the web client's own files, when a web directory is configured. Open to anyone.
- `GET /`: redirects the root path alone to `/web/` when a web directory is configured. Open to anyone.

Every other method and path gets the catch-all's 404, with or without a token.

## Routes

| Route | What it does | Requires | Scope |
|---|---|---|---|
| `GET /Artists` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Branding/Configuration` | Branding of the sign-in page; empty | anyone | none: no token is read |
| `GET /Branding/Css` | Branding of the sign-in page; empty | anyone | none: no token is read |
| `GET /Branding/Css.css` | Branding of the sign-in page; empty | anyone | none: no token is read |
| `GET /DisplayPreferences/{preferenceId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `POST /DisplayPreferences/{preferenceId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `GET /Genres` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/Filters` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/Filters2` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/Latest` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/Suggestions` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/{itemId}` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/{itemId}/Ancestors` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/{itemId}/Collections` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/{itemId}/Download` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/{itemId}/Images/{imageType}` | Sends a picture of an item, by its random id | anyone | whoever holds the item id; a picture by name needs a token |
| `GET /Items/{itemId}/Images/{imageType}/{imageIndex}` | Sends a picture of an item, by its random id | anyone | whoever holds the item id; a picture by name needs a token |
| `GET /Items/{itemId}/LocalTrailers` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/{itemId}/PlaybackInfo` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `POST /Items/{itemId}/PlaybackInfo` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/{itemId}/Similar` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/{itemId}/SpecialFeatures` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Items/{itemId}/ThemeMedia` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /jellyscale/v1/track-preferences` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `PATCH /jellyscale/v1/track-preferences` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `GET /LiveTv/Programs` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /LiveTv/Programs/Recommended` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /Localization/Cultures` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /MediaSegments/{itemId}` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Persons` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Playback/BitrateTest` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /Plugins` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /QuickConnect/Enabled` | Says who the server is, or that it answers, or that Quick Connect is off | anyone | none: no token is read |
| `GET /Search/Hints` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Sessions` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account |
| `POST /Sessions/Capabilities` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account |
| `POST /Sessions/Capabilities/Full` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account |
| `POST /Sessions/Logout` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account |
| `POST /Sessions/Playing` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account |
| `POST /Sessions/Playing/Ping` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account |
| `POST /Sessions/Playing/Progress` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account |
| `POST /Sessions/Playing/Stopped` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account |
| `GET /Shows/NextUp` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Shows/{seriesId}/Episodes` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Shows/{seriesId}/Seasons` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /socket` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account |
| `GET /Studios` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /SyncPlay/List` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /System/Configuration/encoding` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /System/Endpoint` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /System/Info` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /System/Info/Public` | Says who the server is, or that it answers, or that Quick Connect is off | anyone | none: no token is read |
| `GET /System/Ping` | Says who the server is, or that it answers, or that Quick Connect is off | anyone | none: no token is read |
| `POST /System/Ping` | Says who the server is, or that it answers, or that Quick Connect is off | anyone | none: no token is read |
| `DELETE /UserFavoriteItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `POST /UserFavoriteItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `GET /UserItems/Resume` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `POST /UserItems/{itemId}/UserData` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `DELETE /UserPlayedItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `POST /UserPlayedItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `POST /Users/AuthenticateByName` | Signs a viewer in with a name and password and answers a token | anyone | checks the name and password itself |
| `POST /Users/Configuration` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `GET /Users/Me` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `GET /Users/Public` | Lists the users to pick from on the sign-in page; always empty | anyone | none: no token is read |
| `GET /Users/{userId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `POST /Users/{userId}/Configuration` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `DELETE /Users/{userId}/FavoriteItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `POST /Users/{userId}/FavoriteItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `GET /Users/{userId}/GroupingOptions` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `GET /Users/{userId}/Items` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Users/{userId}/Items/Latest` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Users/{userId}/Items/Resume` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Users/{userId}/Items/Root` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Users/{userId}/Items/{itemId}` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Users/{userId}/Items/{itemId}/Intros` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Users/{userId}/Items/{itemId}/LocalTrailers` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Users/{userId}/Items/{itemId}/SpecialFeatures` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `DELETE /Users/{userId}/PlayedItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `POST /Users/{userId}/PlayedItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `GET /UserViews` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /UserViews/GroupingOptions` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `DELETE /Videos/ActiveEncodings` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Videos/{itemId}/hls1/main/{segmentId}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Videos/{itemId}/main.m3u8` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Videos/{itemId}/master.m3u8` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Videos/{itemId}/{file}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Videos/{itemId}/{sourceId}/Attachments/{index}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036) |
