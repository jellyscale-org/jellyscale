# What Jellyfin 12.4 does with a user id a request names

Observed with `jellyfin/jellyfin:12.4` (server version 12.4.0), a disposable container on a Docker-assigned port over synthetic films made by `test/differential/capture_user_scope.py`: four films (the first with an English and a Spanish text subtitle) and an episode of a show. Evidence in `user-scope-12.4.json`, one line for each request; the script is rerun with `python3 -I test/differential/capture_user_scope.py compat/captures/user-scope-12.4.json` from the repository root.

Every route of `compat/endpoints.yaml` is asked as an administrator, as a non-administrator A, and with no token, with the id of a second non-administrator B, of nobody, a malformed id, the caller's own id (compact, and dashed in upper case) and the all-zero id, in each place the route can take one: the `{userId}` segment of its path, the `userId` and `controllableByUserId` query keys, and the `UserId` of the body of `POST /Items/{itemId}/PlaybackInfo`; and with two ids in two places or two values under one key. Each case that can change something (`POST`, `DELETE`, `PATCH`) is asked of three new users; the others of one shared set.

The users hold data that tells them apart. **A**: favourite film 1, film 2 played, a position in film 3, the Configuration `Always`/`eng`/`fra`. **B**: favourite film 2, film 1 played, a position twice A's in film 3, `Always`/`spa`/`deu`. **The administrator**: nothing, `None`/``/`ita`. Each has a session on a device of their own and display preferences naming them. Whose data an answer carries is read from the ids in it, the `UserData` of films 1 to 3, a `Configuration`'s subtitle language, `CustomPrefs` and the `DefaultSubtitleStreamIndex` PlaybackInfo chooses; `nobody` is an answer with none of these. What a request changed is the difference between what each user holds before and after (their favourites, played state and positions, Configuration, display preferences, whether their token still works, and their session).

In the tables a cell is the status, then whose data came back (`A`, `B`, `admin`, `nobody`), then `changed` and who. A dash is a case the route has no place for. `A` is `A`'s own token, and the case names what was sent.

## As a non-administrator A

| Route | own id in the path | no id | path: B | query userId: B | query userId: nobody | query userId: malformed | query userId: self | query userId: selfCompact | query userId: zero | query userId: empty | query controllableByUserId: B |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `DELETE /UserFavoriteItems/{itemId}` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `DELETE /UserPlayedItems/{itemId}` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `DELETE /Users/{userId}/FavoriteItems/{itemId}` | 200 nobody | - | 403 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `DELETE /Users/{userId}/PlayedItems/{itemId}` | 200 nobody | - | 403 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `DELETE /Videos/ActiveEncodings` | - | 204 | - | 204 | 204 | 204 | 204 | 204 | 204 | 204 | 204 |
| `GET /Artists` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Branding/Configuration` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Branding/Css` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Branding/Css.css` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /DisplayPreferences/{preferenceId}` | - | 200 A | - | 403 | 403 | 400 | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /Genres` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/Filters` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/Filters2` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/Latest` | - | 200 A | - | 403 | 403 | 400 | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /Items/Suggestions` | - | 200 nobody | - | 403 | 403 | 400 | 200 A | 200 A | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}` | - | 200 A | - | 403 | 403 | 400 | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /Items/{itemId}/Ancestors` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/Collections` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/Download` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/Images/{imageType}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/Images/{imageType}/{imageIndex}` | - | 404 | - | 404 | 404 | 404 | 404 | 404 | 404 | 404 | 404 |
| `GET /Items/{itemId}/LocalTrailers` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/PlaybackInfo` | - | 200 A | - | 403 | 403 | 400 | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /Items/{itemId}/Similar` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/SpecialFeatures` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/ThemeMedia` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /LiveTv/Programs` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /LiveTv/Programs/Recommended` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Localization/Cultures` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /MediaSegments/{itemId}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Persons` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Playback/BitrateTest` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Plugins` | - | 403 | - | 403 | 403 | 403 | 403 | 403 | 403 | 403 | 403 |
| `GET /QuickConnect/Enabled` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Search/Hints` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Sessions` | - | 200 A | - | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 403 |
| `GET /Shows/NextUp` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Shows/{seriesId}/Episodes` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Shows/{seriesId}/Seasons` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Studios` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /SyncPlay/List` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Configuration/encoding` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Endpoint` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Info` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Info/Public` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Ping` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /UserItems/Resume` | - | 200 A | - | 403 | 403 | 400 | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /UserViews` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /UserViews/GroupingOptions` | - | 200 nobody | - | 403 | 403 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/Me` | - | 200 A | - | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /Users/Public` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}` | 200 A | - | 200 B | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /Users/{userId}/GroupingOptions` | 200 nobody | - | 403 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items` | 200 nobody | - | 403 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items/Latest` | 200 A | - | 403 | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /Users/{userId}/Items/Resume` | 200 A | - | 403 | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /Users/{userId}/Items/Root` | 200 nobody | - | 403 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items/{itemId}` | 200 A | - | 403 | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A | 200 A |
| `GET /Users/{userId}/Items/{itemId}/Intros` | 200 nobody | - | 403 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items/{itemId}/LocalTrailers` | 200 nobody | - | 403 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items/{itemId}/SpecialFeatures` | 200 nobody | - | 403 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/hls1/main/{segmentId}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/main.m3u8` | - | 200 nobody | - | 200 B | 200 nobody | 200 nobody | 200 A | 200 A | 200 nobody | 200 nobody | 200 B |
| `GET /Videos/{itemId}/master.m3u8` | - | 200 nobody | - | 200 B | 200 nobody | 200 nobody | 200 A | 200 A | 200 nobody | 200 nobody | 200 B |
| `GET /Videos/{itemId}/{file}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/{sourceId}/Attachments/{index}` | - | 404 | - | 404 | 404 | 404 | 404 | 404 | 404 | 404 | 404 |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /socket` | - | 101 | - | 101 | 101 | 101 | 101 | 101 | 101 | 101 | 101 |
| `POST /DisplayPreferences/{preferenceId}` | - | 204 changed A | - | 403 | 403 | 400 | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A |
| `POST /Items/{itemId}/PlaybackInfo` | - | 200 A | - | 403 | 403 | 400 | 200 A | 200 A | 200 A | 200 A | 200 A |
| `POST /Sessions/Capabilities` | - | 204 | - | 204 | 204 | 204 | 204 | 204 | 204 | 204 | 204 |
| `POST /Sessions/Capabilities/Full` | - | 204 | - | 204 | 204 | 204 | 204 | 204 | 204 | 204 | 204 |
| `POST /Sessions/Logout` | - | 204 changed A | - | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A |
| `POST /Sessions/Playing` | - | 204 changed A | - | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A |
| `POST /Sessions/Playing/Ping` | - | 204 | - | 204 | 204 | 204 | 204 | 204 | 204 | 204 | 204 |
| `POST /Sessions/Playing/Progress` | - | 204 changed A | - | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A |
| `POST /Sessions/Playing/Stopped` | - | 204 changed A | - | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A |
| `POST /System/Ping` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `POST /UserFavoriteItems/{itemId}` | - | 200 nobody changed A | - | 403 | 403 | 400 | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A |
| `POST /UserItems/{itemId}/UserData` | - | 200 nobody changed A | - | 403 | 403 | 400 | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A |
| `POST /UserPlayedItems/{itemId}` | - | 200 nobody changed A | - | 403 | 403 | 400 | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A |
| `POST /Users/AuthenticateByName` | - | 200 A changed A | - | 200 A changed A | 200 A changed A | 200 A changed A | 200 A changed A | 200 A changed A | 200 A changed A | 200 A changed A | 200 A changed A |
| `POST /Users/Configuration` | - | 204 changed A | - | 403 | 404 | 400 | 204 changed A | 204 changed A | 400 | 204 changed A | 204 changed A |
| `POST /Users/{userId}/Configuration` | 204 changed A | - | 403 | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 204 changed A |
| `POST /Users/{userId}/FavoriteItems/{itemId}` | 200 nobody changed A | - | 403 | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A |
| `POST /Users/{userId}/PlayedItems/{itemId}` | 200 nobody changed A | - | 403 | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A |

## As an administrator

| Route | own id in the path | no id | path: B | query userId: B | query userId: nobody | query userId: malformed | query userId: self | query userId: selfCompact | query userId: zero | query userId: empty | query controllableByUserId: B |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `DELETE /UserFavoriteItems/{itemId}` | - | 200 nobody | - | 200 nobody | 404 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `DELETE /UserPlayedItems/{itemId}` | - | 200 nobody | - | 200 nobody | 404 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `DELETE /Users/{userId}/FavoriteItems/{itemId}` | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `DELETE /Users/{userId}/PlayedItems/{itemId}` | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `DELETE /Videos/ActiveEncodings` | - | 204 | - | 204 | 204 | 204 | 204 | 204 | 204 | 204 | 204 |
| `GET /Artists` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Branding/Configuration` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Branding/Css` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Branding/Css.css` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /DisplayPreferences/{preferenceId}` | - | 200 admin | - | 200 B | 500 | 400 | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin |
| `GET /Genres` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items` | - | 200 nobody | - | 200 nobody | 404 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/Filters` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/Filters2` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/Latest` | - | 200 admin | - | 200 B | 404 | 400 | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin |
| `GET /Items/Suggestions` | - | 200 nobody | - | 200 B | 200 nobody | 400 | 200 admin | 200 admin | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}` | - | 200 admin | - | 200 B | 404 | 400 | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin |
| `GET /Items/{itemId}/Ancestors` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/Collections` | - | 200 nobody | - | 200 nobody | 401 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/Download` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/Images/{imageType}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/Images/{imageType}/{imageIndex}` | - | 404 | - | 404 | 404 | 404 | 404 | 404 | 404 | 404 | 404 |
| `GET /Items/{itemId}/LocalTrailers` | - | 200 nobody | - | 200 nobody | 404 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/PlaybackInfo` | - | 200 admin | - | 200 B | 200 admin | 400 | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin |
| `GET /Items/{itemId}/Similar` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/SpecialFeatures` | - | 200 nobody | - | 200 nobody | 404 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/ThemeMedia` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /LiveTv/Programs` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /LiveTv/Programs/Recommended` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Localization/Cultures` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /MediaSegments/{itemId}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Persons` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Playback/BitrateTest` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Plugins` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /QuickConnect/Enabled` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Search/Hints` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Sessions` | - | 200 A/B/admin | - | 200 A/B/admin | 200 A/B/admin | 200 A/B/admin | 200 A/B/admin | 200 A/B/admin | 200 A/B/admin | 200 A/B/admin | 200 nobody |
| `GET /Shows/NextUp` | - | 200 nobody | - | 200 nobody | 404 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Shows/{seriesId}/Episodes` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Shows/{seriesId}/Seasons` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Studios` | - | 200 nobody | - | 200 nobody | 200 nobody | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /SyncPlay/List` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Configuration/encoding` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Endpoint` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Info` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Info/Public` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /System/Ping` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /UserItems/Resume` | - | 200 nobody | - | 200 B | 404 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /UserViews` | - | 200 nobody | - | 200 nobody | 404 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /UserViews/GroupingOptions` | - | 200 nobody | - | 200 nobody | 404 | 400 | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/Me` | - | 200 admin | - | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin |
| `GET /Users/Public` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}` | 200 admin | - | 200 B | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin |
| `GET /Users/{userId}/GroupingOptions` | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items` | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items/Latest` | 200 admin | - | 200 B | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin |
| `GET /Users/{userId}/Items/Resume` | 200 nobody | - | 200 B | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items/Root` | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items/{itemId}` | 200 admin | - | 200 B | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin |
| `GET /Users/{userId}/Items/{itemId}/Intros` | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items/{itemId}/LocalTrailers` | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Users/{userId}/Items/{itemId}/SpecialFeatures` | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/hls1/main/{segmentId}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/main.m3u8` | - | 200 nobody | - | 200 B | 200 nobody | 200 nobody | 200 admin | 200 admin | 200 nobody | 200 nobody | 200 B |
| `GET /Videos/{itemId}/master.m3u8` | - | 200 nobody | - | 200 B | 200 nobody | 200 nobody | 200 admin | 200 admin | 200 nobody | 200 nobody | 200 B |
| `GET /Videos/{itemId}/{file}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/{sourceId}/Attachments/{index}` | - | 404 | - | 404 | 404 | 404 | 404 | 404 | 404 | 404 | 404 |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `GET /socket` | - | 101 | - | 101 | 101 | 101 | 101 | 101 | 101 | 101 | 101 |
| `POST /DisplayPreferences/{preferenceId}` | - | 204 changed admin | - | 204 changed B | 500 | 400 | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin |
| `POST /Items/{itemId}/PlaybackInfo` | - | 200 admin | - | 200 B | 404 | 400 | 200 admin | 200 admin | 200 admin | 200 admin | 200 admin |
| `POST /Sessions/Capabilities` | - | 204 | - | 204 | 204 | 204 | 204 | 204 | 204 | 204 | 204 |
| `POST /Sessions/Capabilities/Full` | - | 204 | - | 204 | 204 | 204 | 204 | 204 | 204 | 204 | 204 |
| `POST /Sessions/Logout` | - | 204 changed admin | - | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin |
| `POST /Sessions/Playing` | - | 204 changed admin | - | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin |
| `POST /Sessions/Playing/Ping` | - | 204 | - | 204 | 204 | 204 | 204 | 204 | 204 | 204 | 204 |
| `POST /Sessions/Playing/Progress` | - | 204 changed admin | - | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin |
| `POST /Sessions/Playing/Stopped` | - | 204 changed admin | - | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin |
| `POST /System/Ping` | - | 200 nobody | - | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody |
| `POST /UserFavoriteItems/{itemId}` | - | 200 nobody changed admin | - | 200 nobody changed B | 404 | 400 | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin |
| `POST /UserItems/{itemId}/UserData` | - | 200 nobody changed admin | - | 200 nobody changed B | 404 | 400 | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin |
| `POST /UserPlayedItems/{itemId}` | - | 200 nobody changed admin | - | 200 nobody changed B | 404 | 400 | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin |
| `POST /Users/AuthenticateByName` | - | 200 admin changed admin | - | 200 admin changed admin | 200 admin changed admin | 200 admin changed admin | 200 admin changed admin | 200 admin changed admin | 200 admin changed admin | 200 admin changed admin | 200 admin changed admin |
| `POST /Users/Configuration` | - | 204 changed admin | - | 204 changed B | 404 | 400 | 204 changed admin | 204 changed admin | 400 | 204 changed admin | 204 changed admin |
| `POST /Users/{userId}/Configuration` | 204 changed admin | - | 204 changed B | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin |
| `POST /Users/{userId}/FavoriteItems/{itemId}` | 200 nobody changed admin | - | 200 nobody changed B | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin |
| `POST /Users/{userId}/PlayedItems/{itemId}` | 200 nobody changed admin | - | 200 nobody changed B | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin |

## A with two ids

| Route | query userId: two values, own then B | query userId: two values, B then own | query UserId and userId: own and B | path own, query B | path B, query own | path B, query B | path nobody, query B | body UserId: B | body UserId: nobody | body UserId: malformed | body UserId: zero | body UserId: self | query own, body B | query B, body own |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| `DELETE /UserFavoriteItems/{itemId}` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `DELETE /UserPlayedItems/{itemId}` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `DELETE /Users/{userId}/FavoriteItems/{itemId}` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `DELETE /Users/{userId}/PlayedItems/{itemId}` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `DELETE /Videos/ActiveEncodings` | 204 | 204 | 204 | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Artists` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Branding/Configuration` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Branding/Css` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Branding/Css.css` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /DisplayPreferences/{preferenceId}` | 200 A | 403 | 200 A | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Genres` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/Filters` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/Filters2` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/Latest` | 200 A | 403 | 200 A | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/Suggestions` | 200 A | 403 | 200 A | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}` | 200 A | 403 | 200 A | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Ancestors` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Collections` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Download` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Images/{imageType}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Images/{imageType}/{imageIndex}` | 404 | 404 | 404 | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/LocalTrailers` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/PlaybackInfo` | 200 A | 403 | 200 A | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Similar` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/SpecialFeatures` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/ThemeMedia` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /LiveTv/Programs` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /LiveTv/Programs/Recommended` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Localization/Cultures` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /MediaSegments/{itemId}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Persons` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Playback/BitrateTest` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Plugins` | 403 | 403 | 403 | - | - | - | - | - | - | - | - | - | - | - |
| `GET /QuickConnect/Enabled` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Search/Hints` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Sessions` | 200 A | 200 A | 200 A | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Shows/NextUp` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Shows/{seriesId}/Episodes` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Shows/{seriesId}/Seasons` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Studios` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /SyncPlay/List` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Configuration/encoding` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Endpoint` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Info` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Info/Public` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Ping` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /UserItems/Resume` | 200 A | 403 | 200 A | - | - | - | - | - | - | - | - | - | - | - |
| `GET /UserViews` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /UserViews/GroupingOptions` | 200 nobody | 403 | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Users/Me` | 200 A | 200 A | 200 A | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Users/Public` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Users/{userId}` | 200 A | 200 A | 200 A | 200 A | 200 B | 200 B | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/GroupingOptions` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/Latest` | 200 A | 200 A | 200 A | 200 A | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/Resume` | 200 A | 200 A | 200 A | 200 A | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/Root` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/{itemId}` | 200 A | 200 A | 200 A | 200 A | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/{itemId}/Intros` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/{itemId}/LocalTrailers` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/{itemId}/SpecialFeatures` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/hls1/main/{segmentId}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/main.m3u8` | 200 A/B | 200 A/B | 200 A/B | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/master.m3u8` | 200 A/B | 200 A/B | 200 A/B | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/{file}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/{sourceId}/Attachments/{index}` | 404 | 404 | 404 | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /socket` | 101 | 101 | 101 | - | - | - | - | - | - | - | - | - | - | - |
| `POST /DisplayPreferences/{preferenceId}` | 204 changed A | 403 | 204 changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Items/{itemId}/PlaybackInfo` | 200 A | 403 | 200 A | - | - | - | - | 403 | 403 | 400 | 200 A | 200 A | 200 A | 403 |
| `POST /Sessions/Capabilities` | 204 | 204 | 204 | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Capabilities/Full` | 204 | 204 | 204 | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Logout` | 204 changed A | 204 changed A | 204 changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Playing` | 204 changed A | 204 changed A | 204 changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Playing/Ping` | 204 | 204 | 204 | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Playing/Progress` | 204 changed A | 204 changed A | 204 changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Playing/Stopped` | 204 changed A | 204 changed A | 204 changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /System/Ping` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `POST /UserFavoriteItems/{itemId}` | 200 nobody changed A | 403 | 200 nobody changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /UserItems/{itemId}/UserData` | 200 nobody changed A | 403 | 200 nobody changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /UserPlayedItems/{itemId}` | 200 nobody changed A | 403 | 200 nobody changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Users/AuthenticateByName` | 200 A changed A | 200 A changed A | 200 A changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Users/Configuration` | 204 changed A | 403 | 204 changed A | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Users/{userId}/Configuration` | 204 changed A | 204 changed A | 204 changed A | 204 changed A | 403 | 403 | 404 | - | - | - | - | - | - | - |
| `POST /Users/{userId}/FavoriteItems/{itemId}` | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 403 | 403 | 403 | - | - | - | - | - | - | - |
| `POST /Users/{userId}/PlayedItems/{itemId}` | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 200 nobody changed A | 403 | 403 | 403 | - | - | - | - | - | - | - |

## The administrator with two ids

| Route | query userId: two values, own then B | query userId: two values, B then own | query UserId and userId: own and B | path own, query B | path B, query own | path B, query B | path nobody, query B | body UserId: B | body UserId: nobody | body UserId: malformed | body UserId: zero | body UserId: self | query own, body B | query B, body own |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| `DELETE /UserFavoriteItems/{itemId}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `DELETE /UserPlayedItems/{itemId}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `DELETE /Users/{userId}/FavoriteItems/{itemId}` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 404 | - | - | - | - | - | - | - |
| `DELETE /Users/{userId}/PlayedItems/{itemId}` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 404 | - | - | - | - | - | - | - |
| `DELETE /Videos/ActiveEncodings` | 204 | 204 | 204 | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Artists` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Branding/Configuration` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Branding/Css` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Branding/Css.css` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /DisplayPreferences/{preferenceId}` | 200 admin | 200 B | 200 admin | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Genres` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/Filters` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/Filters2` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/Latest` | 200 admin | 200 B | 200 admin | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/Suggestions` | 200 admin | 200 B | 200 admin | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}` | 200 admin | 200 B | 200 admin | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Ancestors` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Collections` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Download` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Images/{imageType}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Images/{imageType}/{imageIndex}` | 404 | 404 | 404 | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/LocalTrailers` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/PlaybackInfo` | 200 admin | 200 B | 200 admin | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/Similar` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/SpecialFeatures` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Items/{itemId}/ThemeMedia` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /LiveTv/Programs` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /LiveTv/Programs/Recommended` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Localization/Cultures` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /MediaSegments/{itemId}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Persons` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Playback/BitrateTest` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Plugins` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /QuickConnect/Enabled` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Search/Hints` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Sessions` | 200 A/B/admin | 200 A/B/admin | 200 A/B/admin | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Shows/NextUp` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Shows/{seriesId}/Episodes` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Shows/{seriesId}/Seasons` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Studios` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /SyncPlay/List` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Configuration/encoding` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Endpoint` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Info` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Info/Public` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /System/Ping` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /UserItems/Resume` | 200 nobody | 200 B | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /UserViews` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /UserViews/GroupingOptions` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Users/Me` | 200 admin | 200 admin | 200 admin | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Users/Public` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Users/{userId}` | 200 admin | 200 admin | 200 admin | 200 admin | 200 B | 200 B | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/GroupingOptions` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/Latest` | 200 admin | 200 admin | 200 admin | 200 admin | 200 B | 200 B | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/Resume` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 B | 200 B | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/Root` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/{itemId}` | 200 admin | 200 admin | 200 admin | 200 admin | 200 B | 200 B | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/{itemId}/Intros` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/{itemId}/LocalTrailers` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 404 | - | - | - | - | - | - | - |
| `GET /Users/{userId}/Items/{itemId}/SpecialFeatures` | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 200 nobody | 404 | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/hls1/main/{segmentId}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/main.m3u8` | 200 B/admin | 200 B/admin | 200 B/admin | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/master.m3u8` | 200 B/admin | 200 B/admin | 200 B/admin | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/{file}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/{sourceId}/Attachments/{index}` | 404 | 404 | 404 | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `GET /socket` | 101 | 101 | 101 | - | - | - | - | - | - | - | - | - | - | - |
| `POST /DisplayPreferences/{preferenceId}` | 204 changed admin | 204 changed B | 204 changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Items/{itemId}/PlaybackInfo` | 200 admin | 200 B | 200 admin | - | - | - | - | 200 B | 404 | 400 | 200 admin | 200 admin | 200 admin | 200 B |
| `POST /Sessions/Capabilities` | 204 | 204 | 204 | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Capabilities/Full` | 204 | 204 | 204 | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Logout` | 204 changed admin | 204 changed admin | 204 changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Playing` | 204 changed admin | 204 changed admin | 204 changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Playing/Ping` | 204 | 204 | 204 | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Playing/Progress` | 204 changed admin | 204 changed admin | 204 changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Sessions/Playing/Stopped` | 204 changed admin | 204 changed admin | 204 changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /System/Ping` | 200 nobody | 200 nobody | 200 nobody | - | - | - | - | - | - | - | - | - | - | - |
| `POST /UserFavoriteItems/{itemId}` | 200 nobody changed admin | 200 nobody changed B | 200 nobody changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /UserItems/{itemId}/UserData` | 200 nobody changed admin | 200 nobody changed B | 200 nobody changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /UserPlayedItems/{itemId}` | 200 nobody changed admin | 200 nobody changed B | 200 nobody changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Users/AuthenticateByName` | 200 admin changed admin | 200 admin changed admin | 200 admin changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Users/Configuration` | 204 changed admin | 204 changed B | 204 changed admin | - | - | - | - | - | - | - | - | - | - | - |
| `POST /Users/{userId}/Configuration` | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed admin | 204 changed B | 204 changed B | 404 | - | - | - | - | - | - | - |
| `POST /Users/{userId}/FavoriteItems/{itemId}` | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed B | 200 nobody changed B | 404 | - | - | - | - | - | - | - |
| `POST /Users/{userId}/PlayedItems/{itemId}` | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed admin | 200 nobody changed B | 200 nobody changed B | 404 | - | - | - | - | - | - | - |

## No token

| Route | no token, query userId: B | no token, no id |
|---|---|---|
| `DELETE /UserFavoriteItems/{itemId}` | 401 | 401 |
| `DELETE /UserPlayedItems/{itemId}` | 401 | 401 |
| `DELETE /Users/{userId}/FavoriteItems/{itemId}` | 401 | 401 |
| `DELETE /Users/{userId}/PlayedItems/{itemId}` | 401 | 401 |
| `DELETE /Videos/ActiveEncodings` | 401 | 401 |
| `GET /Artists` | 401 | 401 |
| `GET /Branding/Configuration` | 200 nobody | 200 nobody |
| `GET /Branding/Css` | 200 nobody | 200 nobody |
| `GET /Branding/Css.css` | 200 nobody | 200 nobody |
| `GET /DisplayPreferences/{preferenceId}` | 401 | 401 |
| `GET /Genres` | 401 | 401 |
| `GET /Items` | 401 | 401 |
| `GET /Items/Filters` | 401 | 401 |
| `GET /Items/Filters2` | 401 | 401 |
| `GET /Items/Latest` | 401 | 401 |
| `GET /Items/Suggestions` | 401 | 401 |
| `GET /Items/{itemId}` | 401 | 401 |
| `GET /Items/{itemId}/Ancestors` | 401 | 401 |
| `GET /Items/{itemId}/Collections` | 401 | 401 |
| `GET /Items/{itemId}/Download` | 401 | 401 |
| `GET /Items/{itemId}/Images/{imageType}` | 200 nobody | 200 nobody |
| `GET /Items/{itemId}/Images/{imageType}/{imageIndex}` | 404 | 404 |
| `GET /Items/{itemId}/LocalTrailers` | 401 | 401 |
| `GET /Items/{itemId}/PlaybackInfo` | 401 | 401 |
| `GET /Items/{itemId}/Similar` | 401 | 401 |
| `GET /Items/{itemId}/SpecialFeatures` | 401 | 401 |
| `GET /Items/{itemId}/ThemeMedia` | 401 | 401 |
| `GET /LiveTv/Programs` | 401 | 401 |
| `GET /LiveTv/Programs/Recommended` | 401 | 401 |
| `GET /Localization/Cultures` | 401 | 401 |
| `GET /MediaSegments/{itemId}` | 401 | 401 |
| `GET /Persons` | 401 | 401 |
| `GET /Playback/BitrateTest` | 401 | 401 |
| `GET /Plugins` | 401 | 401 |
| `GET /QuickConnect/Enabled` | 200 nobody | 200 nobody |
| `GET /Search/Hints` | 401 | 401 |
| `GET /Sessions` | 401 | 401 |
| `GET /Shows/NextUp` | 401 | 401 |
| `GET /Shows/{seriesId}/Episodes` | 401 | 401 |
| `GET /Shows/{seriesId}/Seasons` | 401 | 401 |
| `GET /Studios` | 401 | 401 |
| `GET /SyncPlay/List` | 401 | 401 |
| `GET /System/Configuration/encoding` | 401 | 401 |
| `GET /System/Endpoint` | 401 | 401 |
| `GET /System/Info` | 401 | 401 |
| `GET /System/Info/Public` | 200 nobody | 200 nobody |
| `GET /System/Ping` | 200 nobody | 200 nobody |
| `GET /UserItems/Resume` | 401 | 401 |
| `GET /UserViews` | 401 | 401 |
| `GET /UserViews/GroupingOptions` | 401 | 401 |
| `GET /Users/Me` | 401 | 401 |
| `GET /Users/Public` | 200 nobody | 200 nobody |
| `GET /Users/{userId}` | 401 | 401 |
| `GET /Users/{userId}/GroupingOptions` | 401 | 401 |
| `GET /Users/{userId}/Items` | 401 | 401 |
| `GET /Users/{userId}/Items/Latest` | 401 | 401 |
| `GET /Users/{userId}/Items/Resume` | 401 | 401 |
| `GET /Users/{userId}/Items/Root` | 401 | 401 |
| `GET /Users/{userId}/Items/{itemId}` | 401 | 401 |
| `GET /Users/{userId}/Items/{itemId}/Intros` | 401 | 401 |
| `GET /Users/{userId}/Items/{itemId}/LocalTrailers` | 401 | 401 |
| `GET /Users/{userId}/Items/{itemId}/SpecialFeatures` | 401 | 401 |
| `GET /Videos/{itemId}/hls1/main/{segmentId}` | 401 | 401 |
| `GET /Videos/{itemId}/main.m3u8` | 401 | 401 |
| `GET /Videos/{itemId}/master.m3u8` | 401 | 401 |
| `GET /Videos/{itemId}/{file}` | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/{sourceId}/Attachments/{index}` | 404 | 404 |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}` | 200 nobody | 200 nobody |
| `GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}` | 200 nobody | 200 nobody |
| `GET /socket` | 403 | 403 |
| `POST /DisplayPreferences/{preferenceId}` | 401 | 401 |
| `POST /Items/{itemId}/PlaybackInfo` | 401 | 401 |
| `POST /Sessions/Capabilities` | 401 | 401 |
| `POST /Sessions/Capabilities/Full` | 401 | 401 |
| `POST /Sessions/Logout` | 401 | 401 |
| `POST /Sessions/Playing` | 401 | 401 |
| `POST /Sessions/Playing/Ping` | 401 | 401 |
| `POST /Sessions/Playing/Progress` | 401 | 401 |
| `POST /Sessions/Playing/Stopped` | 401 | 401 |
| `POST /System/Ping` | 200 nobody | 200 nobody |
| `POST /UserFavoriteItems/{itemId}` | 401 | 401 |
| `POST /UserItems/{itemId}/UserData` | 401 | 401 |
| `POST /UserPlayedItems/{itemId}` | 401 | 401 |
| `POST /Users/AuthenticateByName` | 400 | 400 |
| `POST /Users/Configuration` | 401 | 401 |
| `POST /Users/{userId}/Configuration` | 401 | 401 |
| `POST /Users/{userId}/FavoriteItems/{itemId}` | 401 | 401 |
| `POST /Users/{userId}/PlayedItems/{itemId}` | 401 | 401 |

## What the rows say

- **The routes that read the id** answer a non-administrator who names B in the query with 403: 35 routes (those that call `RequestHelpers.GetUserId`), `DELETE /UserFavoriteItems/{itemId}`, `DELETE /UserPlayedItems/{itemId}`, `GET /Artists`, `GET /DisplayPreferences/{preferenceId}`, `GET /Genres`, `GET /Items`, `GET /Items/Filters`, `GET /Items/Filters2`, `GET /Items/Latest`, `GET /Items/Suggestions`, `GET /Items/{itemId}`, `GET /Items/{itemId}/Ancestors`, `GET /Items/{itemId}/Collections`, `GET /Items/{itemId}/LocalTrailers`, `GET /Items/{itemId}/PlaybackInfo`, `GET /Items/{itemId}/Similar`, `GET /Items/{itemId}/SpecialFeatures`, `GET /Items/{itemId}/ThemeMedia`, `GET /LiveTv/Programs`, `GET /LiveTv/Programs/Recommended`, `GET /Persons`, `GET /Search/Hints`, `GET /Shows/NextUp`, `GET /Shows/{seriesId}/Episodes`, `GET /Shows/{seriesId}/Seasons`, `GET /Studios`, `GET /UserItems/Resume`, `GET /UserViews`, `GET /UserViews/GroupingOptions`, `POST /DisplayPreferences/{preferenceId}`, `POST /Items/{itemId}/PlaybackInfo`, `POST /UserFavoriteItems/{itemId}`, `POST /UserItems/{itemId}/UserData`, `POST /UserPlayedItems/{itemId}`, `POST /Users/Configuration`. Every other route ignores `userId` and `controllableByUserId` and answers as it does with none (a 200 with the caller's own data, or the status it has without it); `GET /Sessions` alone reads `controllableByUserId`: 403 to a non-administrator naming B, 200 with no session to an administrator naming B.
- **The path** of the `/Users/{userId}/...` routes (14 refuse B in the path to a non-administrator, 403) is read the same way. `GET /Users/{userId}` is the exception: a non-administrator naming B in the path gets B's record (status 200), because Jellyfin lets any signed-in user read every account (`UserController.cs:127`).
- **An administrator** naming B in the query or the path is served B's view on every route that reads the id (35 routes), and acts on B on the writes among them; naming an id that is no user is 404 on `GET /Items` and wherever the answer needs the user. (`GET /Plugins` is the administrator's alone in Jellyfin: 403 to A with or without an id.)
- **The all-zero id** is read as no id: 83 routes serve the caller's own data and answer exactly as they do with none. The exceptions are the routes that parse the id as a path or `userId` they must act on: `POST /Users/Configuration` (400); in a path, `GET /Users/{zero}` and `POST /Users/{zero}/Configuration` are 400 and the other `/Users/{userId}/...` routes serve the caller. So a client that sends the zero id (none of the supported clients' captures does) is served by Jellyfin and would be refused by a guard that takes the zero id for someone else's.
- **Several values:** Jellyfin binds the first value of a repeated or differently cased key: `userId=B&userId=own` is refused (35 of the 35 routes that read the id), `userId=own&userId=B` is served as own, and `userId=own&UserId=B` likewise (the keys are one key). On `POST /Items/{itemId}/PlaybackInfo` the query beats the body: query own with B in the body is served as own, query B with own in the body is refused.
- **Path and query naming different users** on the routes that take both (`POST /Users/{userId}/Configuration`, `GET /Users/{userId}`): the path wins and the query is ignored. A non-administrator with B in the path gets 403 whatever the query says, and with its own id in the path and B in the query is served as itself (`POST /Users/{own}/Configuration?userId=B`: 204, A's row changed); an administrator with the path on B and the query on themselves writes B. Jellyfin answers neither 400 nor 403 for the disagreement.
- **Nobody's id as a non-administrator on the Configuration routes** is 404 (`POST /Users/Configuration?userId=nobody`: 404); on `POST /UserItems/{itemId}/UserData` and every GetUserId route it is 403, the status for B's id. A malformed id is 400 on every route that reads it.
- **No token** is 401 on every route that needs one, with or without `userId=B`; the open routes answer as they do without the id.
- The answers of `master.m3u8` and `main.m3u8` carry the query string they were asked with (the id in them is the id the request named, not a user's data); the table reads that as `B`.

## Where jellyscale answers otherwise

JS-127 keeps Jellyfin's rule where Jellyfin refuses, and is stricter on purpose (the lead, 2026-11-24, from the owner's access decisions of 2026-11-16 and D-075): on every route that acts for the caller, a user id that names anyone but the caller is 403 for everyone, an administrator included, in every place. These rows are what it differs from:

- **The all-zero id is refused as not the caller's** (403). Jellyfin reads it as no id and serves the caller: `GET /Items` as A, query userId: zero: 200; `POST /UserPlayedItems/{itemId}` as A, query userId: zero: 200; `GET /Users/{userId}/Items/Latest` as A, path: zero: 200; `POST /Items/{itemId}/PlaybackInfo` as A, body UserId: zero: 200.
- **Every value counts** (403 if any names another user). Jellyfin takes the first value: `GET /Items` as A, query userId: two values, own then B: 200; `GET /Items` as A, query UserId and userId: own and B: 200; `POST /Items/{itemId}/PlaybackInfo` as A, query own, body B: 200 (the query beats the body); `GET /Items` as A, query controllableByUserId: B: 200 (Jellyfin reads `controllableByUserId` on `GET /Sessions` alone: 403).
- **Different ids in the path and the query of an admin-any route:** a non-administrator gets 403 and an administrator 400, nothing read or written. Jellyfin lets the path win and ignores the query: `POST /Users/{userId}/Configuration` as A, path own, query B: 204 (A's own row changed); `POST /Users/{userId}/Configuration` as admin, path B, query own: 204 (B's row changed); `GET /Users/{userId}` as A, path own, query B: 200; `GET /Users/{userId}` as admin, path B, query own: 200.
- **A non-administrator naming an id that is no user** is 403 on the Configuration routes, as for B's id; Jellyfin answers 404 there: `POST /Users/Configuration` as A, query userId: nobody: 404; `POST /Users/{userId}/Configuration` as A, path: nobody: 404 (D-075).
- **A malformed id on a route that acts for the caller** is 403 like any other id that is not the caller's. Jellyfin answers 400 on the routes that read the id: `GET /Items` as A, query userId: malformed: 400; `GET /Users/{userId}/Items` as A, path: malformed: 400. On the admin-any routes a malformed id stays 400 whoever asks.
- **An administrator naming another user** is refused (403) on every route that acts for the caller; Jellyfin serves B's view: `GET /Items` as admin, query userId: B: 200; `POST /UserPlayedItems/{itemId}` as admin, query userId: B: 200 (B's data changed); `GET /Users/{userId}/Items/Latest` as admin, path: B: 200. Only four routes act on the user named, for an administrator: `GET /Users/{userId}`, `POST /Users/Configuration`, `POST /Users/{userId}/Configuration` and `POST /UserItems/{itemId}/UserData`.
- **A non-administrator reading B's record** (`GET /Users/{B}`) is 403; Jellyfin serves it: `GET /Users/{userId}` as A, path: B: 200 (the 'any signed-in user reading every account' row of `docs/access.md`, here observed).

## Routes that are not Jellyfin's

- `GET /jellyscale/v1/track-preferences`: not a Jellyfin route; Jellyfin answers 404.
- `PATCH /jellyscale/v1/track-preferences`: not a Jellyfin route; Jellyfin answers 404.
- `PATCH /jellyscale/v1/users/configuration`: not a Jellyfin route; Jellyfin answers 404.
- `POST /jellyscale/v1/items/{itemId}/playback-info`: not a Jellyfin route; Jellyfin answers 404.
