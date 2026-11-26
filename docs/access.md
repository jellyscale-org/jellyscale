# Access

Who may call each route of jellyscale, and the rule behind it. The model is decided by the owner (2026-11-16): playback and personal data always require authorization, and compatibility never means copying Jellyfin's openings. [DOMAINS.md](../DOMAINS.md) holds it as an invariant.

## The model

- Every route declares its access in `internal/api/routes.go` (`Route.Access`), and the declaration is printed on the route's line of `compat/endpoints.yaml`. There are two words:
  - `viewer`: the request carries a token that Postgres knows, in any of the seven spellings `compat.ParseAuth` reads. Without one the answer is 401 with the body `unauthorized`, the same for every route and every cause (no token, an unknown token, a signed-out token, a deleted user, a replaced device).
  - `anyone`: no token is needed. The route says why (`Route.Why`).
- None is open by default: a route with no `Access`, or a word that is neither, stops `Server.Handler()` from building, whichever roles the server has, so no binary starts.
- Media, item data and anything that names a user are never open. A client must send its token on every media request. `master.m3u8` and `main.m3u8` carry the token in their lines only when the request carried it in the query (as `ApiKey` or `api_key`, with the request's own token; any other value is not echoed). A client that sends its token in a header must send that header on every playlist and segment request; `TranscodingUrl`, the `DeliveryUrls` and the subtitle playlist still carry it, because jellyfin-web loads them as URLs.
- Pictures by id are the one content open to whoever holds the id, by the owner's decision: jellyfin-web builds picture URLs with no token and an `<img>` cannot send one, and item ids are random. A picture by name needs a token.
- Postgres decides while it answers. The snapshot of active sessions only lets a `Snapshot` route go on for a token it holds while Postgres is away (DOMAINS invariant 37).
- Opening a route takes a change of the open list in `internal/api/access_test.go`, which a reviewer sees.
- A viewer sees every film today; per-user libraries are JS-036. A request naming another user's id is answered as the caller's own today; answering it 403 is JS-127. The `admin` word joins with its first route in JS-033.

## What a client must send

Every route that requires a viewer reads the token from the first of these that has one (`compat.ParseAuth`). A client may send it in any of the seven spellings:

- `Authorization: MediaBrowser Token="..."`, with the other `Client`, `Device`, `DeviceId` and `Version` fields the SDKs send.
- `Authorization: Emby Token="..."`, the same fields under the older scheme.
- `X-Emby-Authorization`, with the value of either scheme above.
- `X-Emby-Token: <token>`.
- `X-MediaBrowser-Token: <token>`.
- `ApiKey=<token>` in the query.
- `api_key=<token>` in the query.

This holds for every media request: the direct-play stream, the playlists and segments, subtitles, fonts and downloads. A player that cannot set a header puts the token in the query, and the TranscodingUrl and DeliveryUrl that PlaybackInfo gives already end in `ApiKey=`. A client that builds the stream URL itself and sends no token gets 401 and is not supported (D-026). Findroid and Android TV do that on direct play (`compat/clients.yaml`, `player`; the table of `docs/status.md`), where Jellyfin 12.4 serves the stream to anyone.

## Stricter than Jellyfin

Where Jellyfin 12.4 answers a request that carries no token and jellyscale answers 401. Each row names the row of `compat/captures/access-12.4.json` that observed Jellyfin's answer (`access-12.4.md` describes it), or says `source only` with the file and line at tag v12.4. `go test ./test/differential` holds the table to the capture. Pictures by id are open here too, and are not in it (see The model).

| Route | Jellyfin with no token | Observed |
|---|---|---|
| `GET /Videos/{id}/stream?static=true` | 200 | capture: `GET /Videos/{id}/stream?static=true` |
| `HEAD /Videos/{id}/stream?static=true` | 200 | capture: `HEAD /Videos/{id}/stream?static=true` |
| `GET /Videos/{id}/stream.mkv?VideoCodec=h264&AudioCodec=aac` | 200 | capture: `GET /Videos/{id}/stream.mkv?VideoCodec=h264&AudioCodec=aac` |
| `GET /Videos/{id}/{id}/Attachments/3` | 200 | capture: `GET /Videos/{id}/{id}/Attachments/3` |
| `GET /Videos/{id}/{id}/Subtitles/2/Stream.vtt` | 200 | capture: `GET /Videos/{id}/{id}/Subtitles/2/Stream.vtt` |
| `GET /Videos/{id}/{id}/Subtitles/2/Stream.sup` | 200 | capture: `GET /Videos/{id}/{id}/Subtitles/2/Stream.sup` |
| `GET /Videos/{id}/{id}/Subtitles/2/10000000/Stream.vtt` | 200 | capture: `GET /Videos/{id}/{id}/Subtitles/2/10000000/Stream.vtt` |
| any signed-in user reading every account | 200 | source only: `UserController.cs:91` and `:127` at tag v12.4 |

The last row is not a route with no token: Jellyfin lets any signed-in user list and read every account (`GET /Users` and `GET /Users/{userId}` are `[Authorize]`, not administrator-only), and jellyscale shows a viewer no other viewer's name (owner's decision, 2026-11-16).

## Failed sign-ins

Failed sign-ins are bounded (JS-145, D-020), because with any origin allowed (D-016) a web page in a viewer's browser can guess names and passwords and read the answers, and a server on the internet is guessed at anyway. Three bounds count wrong passwords, each over 15 minutes (any 15 minutes, not a clock window) and each refusing for 15 minutes from the failure that reached it:

| Bound | Counts | Limit |
|---|---|---|
| per name | wrong passwords for one name (letter case ignored, an account or not) from devices that have not signed in to that account before | 20 |
| per known device | wrong passwords for one account from one `DeviceId` that is known to it | 10 |
| per address | wrong passwords from one address, for any name or device | 10 |

A device is known to an account while it holds a token for it, and for 30 days after that token ended (it signed out, or the token was replaced); a device whose last token ended more than 30 days ago, one known only to another account and an empty `DeviceId` are strangers. A known device is outside the per name bound and has its own count. The per address bound counts an IPv6 address as its /64 and an IPv4-mapped one as its IPv4 address, and it applies only where a request has one client's usable address: it is on only when the operator declares `trustedProxies`, and a request from a declared proxy with no usable forwarded entry (none, every entry a declared proxy, or the nearest not an address) is not counted or refused per address, whatever the proxy's own address is (docs/running.md says which address counts behind an ingress); without the key the bounds per name and per known device hold and the start-up log says the per address bound is off. A refused attempt checks no password, counts toward no bound and does not lengthen the refusal, so a refusal is over 15 minutes after the failure that began it, and then ends by itself. No account is ever disabled by failures, and `LoginAttemptsBeforeLockout` reads -1 for every user, also while one is refused. Jellyfin 12.4 has no bound by default and disables an account at a count an administrator sets (`compat/captures/signin-12.4.md`); jellyscale does not honour that setting.

A refusal is `403` with `Retry-After`: whole seconds, rounded up, 1 to 900, until the sign-in would no longer be refused. The status, body and headers are the same for a name that exists and one that does not, and an unknown name counts toward the per name bound exactly as an account does, so a guesser learns nothing from them. A wrong password, for an account or not, answers `401 invalid username or password` and makes one bcrypt comparison either way. Every route that takes a password is bounded the same way: today `POST /Users/AuthenticateByName`, in every spelling of its path. A token already issued is never touched: a film that plays, its progress reports, its socket and the account's other devices go on while the account, a device and an address are refused.

What a guesser gets: 20 checked passwords per name per 15 minutes from devices new to the account, so at most 1,920 a day (96 rounds of 20), and 10 more per round (960 a day) for each known `DeviceId` they hold, since the device's failures count toward it alone. One address alone never causes a name to be refused to others while the per address bound is on: it is refused itself after 10.

What a viewer with the right password can lose: on a device that has signed in before, nothing, unless the guesser sends that device's `DeviceId` or shares the viewer's address or /64; then the viewer is kept out until 15 minutes after that guessing stops. On a new device the viewer is kept out until 15 minutes after the guessing of the name stops (**Quick Connect is the way in while the name is refused**: the device asks for a code, a signed-in account approves it and the device redeems that approval, never a password). A viewer who mistypes 9 times and then types the right password signs in, on a known device whatever others do to the name and on a new one while others' failures on the name in the last 15 minutes are at most 10.

jellyfin-web has no "try again later": for a 403 it shows the toast `You are not authorized to access the server at this time. Please contact your server administrator for more information.` and stays on the sign-in form (recorded by `test/e2e/cors.mjs`; `compat/captures/signin-12.4.md` has the other statuses). That is what viewers read, so an administrator finds the reason in the log, which says `sign-in refused` at WARN with the bound, the address counted (or, when none was, the peer's, which behind an ingress is the ingress's own; docs/running.md says what the line holds) and the account's stored name (never the text of a name that is no account, and never a password), and in the metrics `jellyscale_signin_refusals_total{bound}` and `jellyscale_signin_failures_total`. What the apps JS-075 plays show is recorded there.

The state is in Postgres (tables `signin_*`), so the bounds hold across replicas and restarts, and an attempt stores its failure before it is answered. When Postgres cannot be read or written no password is answered as right or wrong (the caller gets the `500` of any failed write). Names that are no account are remembered within a fixed bound of 100,000 unknown names, hashed so that what a viewer typed into the name box is not stored: past it the one with the fewest failures, then the oldest, is forgotten, and what is stored stops growing, so a flood of invented names changes no account's count. The bound is held to within the sign-ins in flight at once: while names are entered the store may hold more than 100,000 by no more than that many, which on all api replicas together is the sum of each replica's `databaseMaxConns` (16 by default), and it is back to at most 100,000 with the next unknown name that fails alone, so no excess passes from one flood to the next (D-050; observed in docs/testing.md).

## Quick Connect

A device that has no token shows six decimal digits: it asks `POST /QuickConnect/Initiate` with only its own device fields, polls `GET /QuickConnect/Connect?Secret=` every few seconds, and redeems the secret at `POST /Users/AuthenticateWithQuickConnect`. An account already signed in approves the digits at `POST /QuickConnect/Authorize`, always for its own account (an administrator included: naming anyone else is a plain 403, not a guessing refusal). Approval, not redemption, makes the sign-in: the device's token belongs to the device that asked for the code, and a second redemption gives the same token. The code and secret are drawn from `crypto/rand`, a live code is unique across the whole cluster, and the secret alone reads a code's state or redeems it; the six digits never yield a token and the poll's answer carries none. All of it is in Postgres (`quickconnect_codes`), so any api replica answers any step and one killed between steps loses nothing.

A pending code is gone 10 minutes after Initiate; an approved one can be redeemed for 10 minutes from its approval, however late that came. Expired codes are removed by the next Initiate, so nothing grows with codes that came and went. At most **100** codes are live at once across all replicas (pending or approved and not redeemed); this cap is not configurable. Past it Initiate is refused with 503 (a status jellyfin-web shows as `Quick Connect is not active on this server`, and which Moonfin does not read as "turned off"), no issued code is touched, and each refusal is logged with `cap=live_codes` and counted in `jellyscale_quickconnect_cap_refusals_total{cap}`. Password sign-in is unaffected by the cap.

Guessing at the digits is bounded **per approving account** (D-078), not per name or address: an approval reserves its failure in `signin_*` before it looks the code up; a successful approval gives the reservation back, and so does an approval the database definitely fails, which says nothing about the code. A wrong code keeps it, and so does an approval the database never answers (its timeout, or a lost connection). An error the database does answer with, a cancelled statement or a terminated connection included, gives it back. And the approval and its release run detached from the client, so a client that hangs up changes neither whether a guess counts nor whether its code is approved. Ten failures in any 15 minutes refuse that account's approvals for 15 minutes with 403 and `Retry-After`, checking no code, counting toward nothing and lengthening nothing. That is at most **960 guesses a day per account** (96 rounds of ten), and a hit signs a stranger's device into that account, which is why the cap and the uniqueness both matter. The bounds the account's own tokens, playback and password sign-in do not change, and the password bounds neither count nor refuse any Quick Connect route, so a viewer whose name and address are refused for passwords can still sign in by code and is then a known device of the account in JS-145's per-device bound. Connect polling and redemption are never refused for guessing; ten devices behind one address can poll forever past their codes' expiry. Each refusal is logged as a structured record with the bound `approval` and the account's name (never a code, secret or token) and counted in `jellyscale_quickconnect_bound_refusals_total{bound}`.

When an api replica has `quickConnect: false`, `GET /QuickConnect/Enabled` answers false and the other four routes answer 401; a code started on another replica keeps working there. Jellyfin 12.4 has Quick Connect on by default and no bounds; the differences from it are listed in `compat/differences.yaml` (the second approval is 409 here, not 500, and approval is always for the account that is signed in).

## Served outside the table

- `GET /healthz`: liveness probe, the process only. Open to anyone.
- `GET /readyz`: readiness probe for the router. Open to anyone.
- `GET /web/`: the web client's own files, when a web directory is configured. Open to anyone.
- `GET /`: redirects the root path alone to `/web/` when a web directory is configured. Open to anyone.

A preflight (an `OPTIONS` request with an `Origin` and `Access-Control-Request-Method`) of a route is answered 204 with no body, and every other method and path gets the catch-all's 404, with or without a token.

## Other origins

Any origin may call the API and media routes, and never with credentials (D-016): what has been shown is Chromium, on a page served from another origin, reading the open routes, signing in, browsing and playing, and every route answering a preflight for an origin with a scheme of its own (`app://localhost`) and for `http://` and `https://` localhost origins, as an app's embedded web view sends. Octave itself has not been run against this server, and jellyscale does not run it: Octave is engineered by its own project to work with jellyscale (D-027). Every answer to a request with an `Origin` carries `Access-Control-Allow-Origin: *`, and the answers a page reads ranges from list `Content-Range`, `Accept-Ranges` and `Content-Length` in `Access-Control-Expose-Headers` (with `Retry-After`, which a refused sign-in carries).

A preflight is open to anyone, with no token: a browser sends it before any request that carries a token in a header, and it cannot carry one. It reads no token and sends Postgres no statement, runs no handler and tells nothing of the server; it allows the asked method where the path serves it and the headers asked, `Authorization` by name, since a browser does not take `*` for it. It is answered in `internal/api/cors.go` from the request and the route table alone.

That does not open a viewer's account. No route reads or sets a cookie, so the browser sends nothing of the viewer's by itself: a page can only use a token it already holds, and a viewer route asked with none is still 401 `unauthorized` (invariant 63). What it does expose is that, on a server reachable only on a private network, a web page in a viewer's browser can read the open routes and the result of a sign-in attempt.

`/healthz`, `/readyz`, `/metrics` and `/web/` are not opened: they carry no `Access-Control-*` header. A preflight of `/healthz`, `/readyz` or `/web/` gets the catch-all's 404; `/metrics` is on its own listener, which answers a preflight 405.

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
| `POST /jellyscale/v1/items/{itemId}/playback-info` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `PATCH /jellyscale/v1/users/configuration` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account |
| `GET /LiveTv/Programs` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /LiveTv/Programs/Recommended` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /Localization/Cultures` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /MediaSegments/{itemId}` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Persons` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036) |
| `GET /Playback/BitrateTest` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `GET /Plugins` | Answers a question about the server, or a stub for a feature not built | viewer | the server |
| `POST /QuickConnect/Authorize` | Approves a displayed code for the signed-in account only | viewer | the caller's own account; naming anyone else is refused |
| `GET /QuickConnect/Connect` | Tells a device whether its code was approved; carries no token | anyone | the secret alone, never a token |
| `GET /QuickConnect/Enabled` | Says whether this replica offers Quick Connect | anyone | none: no token is read |
| `POST /QuickConnect/Initiate` | Gives an unauthenticated device a code and its secret | anyone | its own device fields; subject to the 100-live-code cap |
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
| `POST /Users/AuthenticateWithQuickConnect` | Signs the asking device in with the secret of an approved code | anyone | the secret alone; the device was named at Initiate |
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
