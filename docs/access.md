# Access

Who may call each route of jellyscale, and the rule behind it. The model is decided by the owner (2026-11-16): playback and personal data always require authorization, and compatibility never means copying Jellyfin's openings. [DOMAINS.md](../DOMAINS.md) holds it as an invariant.

## The model

- Every route declares its access in `internal/api/routes.go` (`Route.Access`), and the declaration is printed on the route's line of `compat/endpoints.yaml`. There are three words:
  - `viewer`: the request carries a token that Postgres knows, in any of the seven spellings `compat.ParseAuth` reads. Without one the answer is 401 with the body `unauthorized`, the same for every route and every cause (no token, an unknown token, a signed-out token, a deleted user, a replaced device).
  - `admin`: a viewer's token that belongs to an administrator. Another viewer's gets 403, whatever the request names, a malformed id included; no token or an unknown one gets 401 first. A refusal sends Postgres the token lookup and nothing else. These are `POST /Users/New`, `DELETE /Users/{userId}`, `POST /Users/{userId}/Policy`, `GET /Auth/Providers`, `GET /Auth/PasswordResetProviders`, `GET /Library/MediaFolders` and `GET /Devices`. Whether the viewer is an administrator is read with the token on every request, so a promotion or demotion acts on the next one through any replica.
  - `anyone`: no token is needed. The route says why (`Route.Why`).
- A viewer route also declares whose account it acts on (`Route.Users`). `own` is the default: every user id it names must be the caller's, and an administrator gets no other viewer's view. `admin-any` is `GET /Users/{userId}`, `POST /Users/Configuration`, `POST /Users/{userId}/Configuration`, `POST /UserItems/{itemId}/UserData`, `GET /Users`, `POST /Users`, `POST /Users/Password`, `POST /Users/{userId}/Password` and the first five `admin` routes above: an administrator may name another account there. The guard runs after authentication and before the handler, on every role.
- None is open by default: a route with no `Access`, or a word that is neither, stops `Server.Handler()` from building, whichever roles the server has, so no binary starts. A viewer route with an unknown user scope, or an open route with a user scope, stops it too.
- Media, item data and anything that names a user are never open. A client must send its token on every media request. `master.m3u8` and `main.m3u8` carry the token in their lines only when the request carried it in the query (as `ApiKey` or `api_key`, with the request's own token; any other value is not echoed). A client that sends its token in a header must send that header on every playlist and segment request; `TranscodingUrl`, the `DeliveryUrls` and the subtitle playlist still carry it, because jellyfin-web loads them as URLs.
- Pictures by id are the one content open to whoever holds the id, by the owner's decision: jellyfin-web builds picture URLs with no token and an `<img>` cannot send one, and item ids are random. A picture by name needs a token.
- Postgres decides while it answers. The snapshot of active sessions only lets a `Snapshot` route go on for a token it holds while Postgres is away (DOMAINS invariant 37).
- Opening a route takes a change of the open list in `internal/api/access_test.go`, which a reviewer sees.
- A viewer sees every film today; per-user libraries are JS-036. A user id in a path, any `userId` or `controllableByUserId` query value, or a PlaybackInfo body's `UserId` names the caller or the request is refused. The caller's own id may be dashed or compact, in any letter case; an empty value is absent, and the all-zero id is another id. On an `own` route another, unknown or malformed id is the same 403 for everyone, after the token is checked. On an `admin-any` route a non-administrator naming another or unknown id gets 403, a malformed id is 400 whoever asks, an administrator naming nobody gets 404, and different ids in one request are 400 for an administrator and 403 for anyone else. An `anyone` route ignores ids. During a Postgres outage a snapshot route applies the same own-id check from the identity carried in its existing session reads: the caller's id is served as no id, another's waits for Postgres and is never answered 4xx because it was away (DOMAINS invariant 37).

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

Where Jellyfin 12.4 answers a request more openly and jellyscale refuses it. Each row names the capture entry that observed Jellyfin's answer. The table is documentation, not a test held to the capture. Pictures by id are open here too, and are not in it (see The model).

| Route | Jellyfin's answer | Observed |
|---|---|---|
| `GET /Videos/{id}/stream?static=true` | 200 | capture: `GET /Videos/{id}/stream?static=true` |
| `HEAD /Videos/{id}/stream?static=true` | 200 | capture: `HEAD /Videos/{id}/stream?static=true` |
| `GET /Videos/{id}/stream.mkv?VideoCodec=h264&AudioCodec=aac` | 200 | capture: `GET /Videos/{id}/stream.mkv?VideoCodec=h264&AudioCodec=aac` |
| `GET /Videos/{id}/{id}/Attachments/3` | 200 | capture: `GET /Videos/{id}/{id}/Attachments/3` |
| `GET /Videos/{id}/{id}/Subtitles/2/Stream.vtt` | 200 | capture: `GET /Videos/{id}/{id}/Subtitles/2/Stream.vtt` |
| `GET /Videos/{id}/{id}/Subtitles/2/Stream.sup` | 200 | capture: `GET /Videos/{id}/{id}/Subtitles/2/Stream.sup` |
| `GET /Videos/{id}/{id}/Subtitles/2/10000000/Stream.vtt` | 200 | capture: `GET /Videos/{id}/{id}/Subtitles/2/10000000/Stream.vtt` |
| an administrator naming B on a route that acts for the caller, such as `GET /Items?userId=B` | 200 with B's data | capture: `GET /Items`, caller `admin`, case `query userId: B` in `user-scope-12.4.json` |

Jellyfin serves an administrator another viewer's view on every route that reads the id. jellyscale does that on the `admin-any` routes only; on every other viewer route the administrator is refused. Jellyfin also lets a non-administrator read every account (`GET /Users/{userId}` is `[Authorize]`, not administrator-only); jellyscale shows a viewer no other viewer's name (owner's decision, 2026-11-16, D-075).

## Failed sign-ins

Failed sign-ins are bounded (JS-145, D-020), because with any origin allowed (D-016) a web page in a viewer's browser can guess names and passwords and read the answers, and a server on the internet is guessed at anyway. Three bounds count wrong passwords, each over 15 minutes (any 15 minutes, not a clock window) and each refusing for 15 minutes from the failure that reached it:

| Bound | Counts | Limit |
|---|---|---|
| per name | wrong passwords for one name (letter case ignored, an account or not) from devices that have not signed in to that account before | 20 |
| per known device | wrong passwords for one account from one `DeviceId` that is known to it | 10 |
| per address | wrong passwords from one address, for any name or device | 10 |

A device is known to an account while it holds a token for it, and for 30 days after that token ended (it signed out, or the token was replaced); a device whose last token ended more than 30 days ago, one known only to another account and an empty `DeviceId` are strangers. A known device is outside the per name bound and has its own count. The per address bound counts an IPv6 address as its /64 and an IPv4-mapped one as its IPv4 address, and it applies only where a request has one client's usable address: it is on only when the operator declares `trustedProxies`, and a request from a declared proxy with no usable forwarded entry (none, every entry a declared proxy, or the nearest not an address) is not counted or refused per address, whatever the proxy's own address is (docs/running.md says which address counts behind an ingress); without the key the bounds per name and per known device hold and the start-up log says the per address bound is off. A refused attempt checks no password, counts toward no bound and does not lengthen the refusal, so a refusal is over 15 minutes after the failure that began it, and then ends by itself. No account is ever disabled by failures, and `LoginAttemptsBeforeLockout` reads -1 for every user, also while one is refused. Jellyfin 12.4 has no bound by default and disables an account at a count an administrator sets (`compat/captures/signin-12.4.md`); jellyscale does not honour that setting.

A refusal is `403` with `Retry-After`: whole seconds, rounded up, 1 to 900, until the sign-in would no longer be refused. The status, body and headers are the same for a name that exists and one that does not, and an unknown name counts toward the per name bound exactly as an account does, so a guesser learns nothing from them. A wrong password, for an account or not, answers `401 invalid username or password` and makes one bcrypt comparison either way. Every route that takes a password is bounded the same way: `POST /Users/AuthenticateByName`, in every spelling of its path, and the `CurrentPw` of a change of one's own password, whose wrong guesses count with sign-ins, as the device that holds the token (a known device) and per address. A token already issued is never touched: a film that plays, its progress reports, its socket and the account's other devices go on while the account, a device and an address are refused.

What a guesser gets: 20 checked passwords per name per 15 minutes from devices new to the account, so at most 1,920 a day (96 rounds of 20), and 10 more per round (960 a day) for each known `DeviceId` they hold, since the device's failures count toward it alone. One address alone never causes a name to be refused to others while the per address bound is on: it is refused itself after 10.

What a viewer with the right password can lose: on a device that has signed in before, nothing, unless the guesser sends that device's `DeviceId` or shares the viewer's address or /64; then the viewer is kept out until 15 minutes after that guessing stops. On a new device the viewer is kept out until 15 minutes after the guessing of the name stops (**Quick Connect is the way in while the name is refused**: the device asks for a code, a signed-in account approves it and the device redeems that approval, never a password). A viewer who mistypes 9 times and then types the right password signs in, on a known device whatever others do to the name and on a new one while others' failures on the name in the last 15 minutes are at most 10.

jellyfin-web has no "try again later": for a 403 it shows the toast `You are not authorized to access the server at this time. Please contact your server administrator for more information.` and stays on the sign-in form (recorded by `test/e2e/cors.mjs`; `compat/captures/signin-12.4.md` has the other statuses). That is what viewers read, so an administrator finds the reason in the log, which says `sign-in refused` at WARN with the bound, the address counted (or, when none was, the peer's, which behind an ingress is the ingress's own; docs/running.md says what the line holds) and the account's stored name (never the text of a name that is no account, and never a password), and in the metrics `jellyscale_signin_refusals_total{bound}` and `jellyscale_signin_failures_total`. What the apps JS-075 plays show is recorded there.

The state is in Postgres (tables `signin_*`), so the bounds hold across replicas and restarts, and an attempt stores its failure before it is answered. When Postgres cannot be read or written no password is answered as right or wrong (the caller gets the `500` of any failed write). Names that are no account are remembered within a fixed bound of 100,000 unknown names, hashed so that what a viewer typed into the name box is not stored: past it the one with the fewest failures, then the oldest, is forgotten, and what is stored stops growing, so a flood of invented names changes no account's count. The bound is held to within the sign-ins in flight at once: while names are entered the store may hold more than 100,000 by no more than that many, which on all api replicas together is the sum of each replica's `databaseMaxConns` (16 by default), and it is back to at most 100,000 with the next unknown name that fails alone, so no excess passes from one flood to the next (D-050; observed in docs/testing.md).

## Accounts and passwords

An administrator manages users in jellyfin-web's own pages, or through Jellyfin's routes. A viewer's `GET /Users` lists their own account alone.

**What a Policy does.** `IsAdministrator`, `IsDisabled` and `EnableContentDownloading` are stored and enforced; a change acts on the next request through any replica. A Policy POST changes only the fields it names. A change to any other field from what `GET` answers is 400 naming every such field, stores nothing and is logged at warn with the user and the fields; `null` where the answer is an empty list is no change, and neither is any value of the three channel fields. So jellyfin-web's New User page works with "all libraries" ticked; otherwise it makes the user, who sees every library, and shows its generic error, until per-user libraries (JS-036). There is always an enabled administrator: demoting, disabling or deleting the last one is 403, also when two such requests race, and is logged at warn with the user and the reason. A change an administrator makes to accounts applies only while they are still a signed-in, enabled administrator when it is written, so of two administrators who reset, demote, disable or delete each other at once, one is refused.

**What jellyfin-web shows when it is refused.** A Policy is saved whole. So a setting jellyscale does not enforce, such as a change on the access page, blocking unrated items on the parental control page, or hiding a user, gets the whole Policy refused until per-user libraries and their rules (JS-036): nothing in that save's Policy is stored, an enforced change beside it included, and the page shows no message or is left with its spinner. The server's answer and its warn line name the fields. A refused delete (of the configured administrator, or of the last enabled administrator, by themselves too) shows no message, and neither does a refused demotion or disabling of the last administrator, which leaves the spinner. The profile page saves the name and preferences before the Policy, so those are kept when its Policy is refused.

**What ends a token.** Disabling or deleting an account ends all its tokens in the same transaction, and re-enabling brings none back. A password change ends the account's other tokens and keeps the one of the device that changed it; an administrator changing or resetting another's ends them all. Ending a token also removes the Quick Connect codes the account approved. No sign-in, Quick Connect redemption or password change that races a disable leaves a live token. The next request with an ended token is 401 through any replica, and a stream already being sent stops within 15 seconds: each stream replica asks Postgres every 5 seconds, in one statement, which tokens of the requests it is answering are still signed in. While Postgres is away nothing is cut, and a stream replica's snapshot holds only what its last refresh (every 5 seconds) found signed in (DOMAINS invariant 37). A disabled account's sign-in is a wrong password's 401, after the same password work, and the log says the account is disabled.

**New passwords.** A new or changed password is refused with 400 and the reason when it is empty, holds a NUL byte (bcrypt would take `password`, NUL, `password` for `password`), is over 72 bytes (what bcrypt reads) or is commonly guessed, in any letter case. The list is [SecLists](https://github.com/danielmiessler/SecLists) `Passwords/Common-Credentials/10k-most-common.txt` (MIT licence, commit `913b327317496d062bcc7cace524aaad8a693be2`, 10,001 entries). It is part of the binary: no password, hash or request about one leaves the operator's servers. jellyfin-web does not show the reason: its password pages say `Invalid username or password. Please try again.`, also when the current password was right, and its New User page shows its generic error. A password that already exists is never checked.

**Accounts with no password.** An account made with no or an empty password, or whose password an administrator reset, has none. Every password sign-in for it, the empty password included, gets a wrong password's 401 after the same password work, and it cannot change its own password, until an administrator sets one. Its other sign-ins are unaffected. Nobody can reset their own password. A migration from Jellyfin brings users without passwords (JS-060): for each, set a password or, once OIDC is built, link them through the provider.

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
| `GET /Artists` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Auth/PasswordResetProviders` | Names the one password reset provider | admin | administrators (`admin-any`) |
| `GET /Auth/Providers` | Names the one sign-in provider | admin | administrators (`admin-any`) |
| `GET /Branding/Configuration` | Branding of the sign-in page; empty | anyone | none: no token is read |
| `GET /Branding/Css` | Branding of the sign-in page; empty | anyone | none: no token is read |
| `GET /Branding/Css.css` | Branding of the sign-in page; empty | anyone | none: no token is read |
| `GET /Channels` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /Devices` | Lists the signed-in devices, never their tokens | admin | administrators |
| `GET /DisplayPreferences/{preferenceId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `POST /DisplayPreferences/{preferenceId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `GET /Genres` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/Filters` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/Filters2` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/Latest` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/Suggestions` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/{itemId}` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/{itemId}/Ancestors` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/{itemId}/Collections` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/{itemId}/Download` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/{itemId}/Images/{imageType}` | Sends a picture of an item, by its random id | anyone | whoever holds the item id; a picture by name needs a token |
| `GET /Items/{itemId}/Images/{imageType}/{imageIndex}` | Sends a picture of an item, by its random id | anyone | whoever holds the item id; a picture by name needs a token |
| `GET /Items/{itemId}/LocalTrailers` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/{itemId}/PlaybackInfo` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `POST /Items/{itemId}/PlaybackInfo` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/{itemId}/Similar` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/{itemId}/SpecialFeatures` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Items/{itemId}/ThemeMedia` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `POST /jellyscale/v1/items/{itemId}/playback-info` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `GET /jellyscale/v1/track-preferences` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `PATCH /jellyscale/v1/track-preferences` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `PATCH /jellyscale/v1/users/configuration` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `GET /Library/MediaFolders` | Lists the libraries for the user pages | admin | administrators |
| `GET /LiveTv/Programs` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /LiveTv/Programs/Recommended` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /Localization/Cultures` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /Localization/ParentalRatings` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /MediaSegments/{itemId}` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Persons` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Playback/BitrateTest` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /Plugins` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `POST /QuickConnect/Authorize` | Approves a displayed code for the signed-in account only | viewer | the caller's own account; naming anyone else is refused |
| `GET /QuickConnect/Connect` | Tells a device whether its code was approved; carries no token | anyone | the secret alone, never a token |
| `GET /QuickConnect/Enabled` | Says who the server is, or that it answers, or that Quick Connect is off | anyone | none: no token is read |
| `POST /QuickConnect/Initiate` | Gives an unauthenticated device a code and its secret | anyone | its own device fields; subject to the 100-live-code cap |
| `GET /Search/Hints` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Sessions` | Reports or ends the caller's playback session, or pushes messages to it | viewer | any user id in the request must name the caller (`own`); with no id an administrator still gets the existing all-sessions list, a non-administrator their own |
| `POST /Sessions/Capabilities` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account (`own`) |
| `POST /Sessions/Capabilities/Full` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account (`own`) |
| `POST /Sessions/Logout` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account (`own`) |
| `POST /Sessions/Playing` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account (`own`) |
| `POST /Sessions/Playing/Ping` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account (`own`) |
| `POST /Sessions/Playing/Progress` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account (`own`) |
| `POST /Sessions/Playing/Stopped` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account (`own`) |
| `GET /Shows/NextUp` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Shows/{seriesId}/Episodes` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Shows/{seriesId}/Seasons` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /socket` | Reports or ends the caller's playback session, or pushes messages to it | viewer | the caller's own account (`own`) |
| `GET /Studios` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /SyncPlay/List` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /System/Configuration/encoding` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /System/Endpoint` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /System/Info` | Answers a question about the server, or a stub for a feature not built | viewer | the server; the caller's own account (`own`) |
| `GET /System/Info/Public` | Says who the server is, or that it answers, or that Quick Connect is off | anyone | none: no token is read |
| `GET /System/Ping` | Says who the server is, or that it answers, or that Quick Connect is off | anyone | none: no token is read |
| `POST /System/Ping` | Says who the server is, or that it answers, or that Quick Connect is off | anyone | none: no token is read |
| `DELETE /UserFavoriteItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `POST /UserFavoriteItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `GET /UserItems/Resume` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `POST /UserItems/{itemId}/UserData` | Reads or changes watched state, favourites or position | viewer | the named account for an administrator, otherwise the caller's own (`admin-any`) |
| `DELETE /UserPlayedItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `POST /UserPlayedItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `GET /Users` | Lists every account to an administrator, and a viewer's own to them | viewer | the named account for an administrator, otherwise the caller's own (`admin-any`) |
| `POST /Users` | Saves a user's name and Configuration | viewer | the named account for an administrator, otherwise the caller's own (`admin-any`) |
| `POST /Users/AuthenticateByName` | Signs a viewer in with a name and password and answers a token | anyone | checks the name and password itself |
| `POST /Users/AuthenticateWithQuickConnect` | Signs the asking device in with the secret of an approved code | anyone | the secret alone; the device was named at Initiate |
| `POST /Users/Configuration` | Reads or changes a Configuration | viewer | the named account for an administrator, otherwise the caller's own (`admin-any`) |
| `GET /Users/Me` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `POST /Users/New` | Makes an enabled account that is no administrator | admin | administrators (`admin-any`) |
| `POST /Users/Password` | Changes or resets a password | viewer | the named account for an administrator, otherwise the caller's own (`admin-any`) |
| `GET /Users/Public` | Lists the users to pick from on the sign-in page; always empty | anyone | none: no token is read |
| `DELETE /Users/{userId}` | Deletes an account and everything it holds | admin | administrators (`admin-any`) |
| `GET /Users/{userId}` | Reads an account | viewer | the named account for an administrator, otherwise the caller's own (`admin-any`) |
| `POST /Users/{userId}/Configuration` | Reads or changes a Configuration | viewer | the named account for an administrator, otherwise the caller's own (`admin-any`) |
| `DELETE /Users/{userId}/FavoriteItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `POST /Users/{userId}/FavoriteItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `GET /Users/{userId}/GroupingOptions` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `GET /Users/{userId}/Items` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Users/{userId}/Items/Latest` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Users/{userId}/Items/Resume` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Users/{userId}/Items/Root` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Users/{userId}/Items/{itemId}` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Users/{userId}/Items/{itemId}/Intros` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Users/{userId}/Items/{itemId}/LocalTrailers` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Users/{userId}/Items/{itemId}/SpecialFeatures` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `POST /Users/{userId}/Password` | The same, at the obsolete path jellyfin-web calls | viewer | the named account for an administrator, otherwise the caller's own (`admin-any`) |
| `DELETE /Users/{userId}/PlayedItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `POST /Users/{userId}/PlayedItems/{itemId}` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `POST /Users/{userId}/Policy` | Makes an account an administrator, disables it or stops its downloads | admin | administrators (`admin-any`) |
| `GET /UserViews` | Lists or reads items of the library | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /UserViews/GroupingOptions` | Reads or changes the caller's own data (watched, favourites, settings, resume) | viewer | the caller's own account (`own`) |
| `DELETE /Videos/ActiveEncodings` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Videos/{itemId}/hls1/main/{segmentId}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Videos/{itemId}/main.m3u8` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Videos/{itemId}/master.m3u8` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Videos/{itemId}/{file}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Videos/{itemId}/{sourceId}/Attachments/{index}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}` | Sends a film's bytes, playlist, segment, subtitle, font or file | viewer | the library as it is today (per-user libraries: JS-036); the caller's own account (`own`) |
