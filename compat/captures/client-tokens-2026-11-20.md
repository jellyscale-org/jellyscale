# Which Jellyfin clients can play from jellyscale

Read-only research, 2026-11-20. The rule comes from D-026 and `docs/access.md`: every media request (direct-play stream, HLS playlists and segments, subtitles, fonts and attachments, downloads) needs a token. Pictures by id are the only open media. jellyscale reads the token from any of seven spellings (`docs/access.md`, "What a client must send"):

- `Authorization: MediaBrowser Token=` or `Emby Token=`
- `X-Emby-Authorization` (either scheme)
- `X-Emby-Token`
- `X-MediaBrowser-Token`
- the query parameter `ApiKey` or `api_key`

Every `TranscodingUrl` and `DeliveryUrl` that jellyscale returns from PlaybackInfo already ends in `ApiKey=`. A client that plays the server's URL as given therefore sends a token on HLS and subtitles.

**What the verdicts mean.**
- **works**: every media request read in the client's source carries a token. No client was played on a device. `docs/status.md` calls no client working until one has been, so "works" here is a reading of the source, not a field result.
- **fails**: a named playback path builds a media URL with no token and sends no auth header with it. jellyscale answers that request 401.
- **unknown**: the source does not settle it. Each such row says what would settle it.

Each source was read at the default branch's HEAD, shallow-cloned on 2026-11-20. The full SHAs are in the permalinks below.

## Summary

| Client | Platform | Library | Direct play | HLS / transcode | Subtitles | Downloads | Verdict | Evidence |
|---|---|---|---|---|---|---|---|---|
| jellyfin-web 12.4 | Browser | @jellyfin/sdk + jellyfin-apiclient | yes (`ApiKey`) | yes (server URL) | yes (server URL) | yes (`ApiKey`, sdk `getDownloadUrl`) | **works** (also the repo's e2e gate) | [playbackmanager.js v12.4 L2885-2890](https://github.com/jellyfin/jellyfin-web/blob/v12.4/src/components/playback/playbackmanager.js#L2885-L2890) |
| Jellyfin Desktop (was Jellyfin Media Player) | Win/macOS/Linux | loads the server's `/web/` (jellyfin-web) + mpv | yes (`ApiKey` from jellyfin-web) | yes | yes (DeliveryUrl) | n/a | **works** | [mpvVideoPlayer.js L322](https://github.com/jellyfin/jellyfin-desktop/blob/5be0017648e7740223fc9d78de619b157fd4c19f/native/mpvVideoPlayer.js#L322) |
| Jellyfin Android (mobile) | Android | WebView (jellyfin-web) + ExoPlayer, jellyfin-sdk-kotlin 1.8.12 | yes (header added by a ResolvingDataSource) | yes | yes | yes (header) | **works** with the built-in players. Its *external player* option **fails** | [AppModule.kt L128-141](https://github.com/jellyfin/jellyfin-android/blob/33656f0d25d95a7a7db7d15d7a3e38650ffe7fdc/app/src/main/java/org/jellyfin/mobile/app/AppModule.kt#L128-L141) |
| Jellyfin Android TV | Android TV | jellyfin-sdk-kotlin 1.8.12 | **no** | yes (server URL) | yes (server URL) | n/a | **fails** (direct play) | [PlaybackManager.kt L41](https://github.com/jellyfin/jellyfin-androidtv/blob/5971f4425d409d262c02e2ff70a230e2233d3d80/app/src/main/java/org/jellyfin/androidtv/ui/playback/PlaybackManager.kt#L41), [JellyfinMediaStreamResolver.kt L38](https://github.com/jellyfin/jellyfin-androidtv/blob/5971f4425d409d262c02e2ff70a230e2233d3d80/playback/jellyfin/src/main/kotlin/mediastream/JellyfinMediaStreamResolver.kt#L38) |
| Findroid | Android / Android TV | jellyfin-sdk-kotlin 1.9.4 | **no** | not requested | yes (server URL) | **no** | **fails** (direct play, downloads) | [JellyfinRepositoryImpl.kt L344](https://github.com/jarnedemeulemeester/findroid/blob/0936178816c9f5048e6e9f617c44e55a4cb9123e/data/src/main/java/dev/jdtech/jellyfin/repository/JellyfinRepositoryImpl.kt#L344), [DownloaderImpl.kt L94](https://github.com/jarnedemeulemeester/findroid/blob/0936178816c9f5048e6e9f617c44e55a4cb9123e/core/src/main/java/dev/jdtech/jellyfin/utils/DownloaderImpl.kt#L94) |
| Wholphin | Android TV | jellyfin-sdk-kotlin 1.9.4 | **no** (ExoPlayer backend). MPV backend: unknown | yes (server URL) | yes (server URL) | none found | **fails** (direct play) | [PlaybackViewModel.kt L717](https://github.com/damontecres/Wholphin/blob/fea40c5bcf9a172a7c12a88886a7cef70353cd68/app/src/main/java/com/github/damontecres/wholphin/ui/playback/PlaybackViewModel.kt#L717), [PlayerFactory.kt L123](https://github.com/damontecres/Wholphin/blob/fea40c5bcf9a172a7c12a88886a7cef70353cd68/app/src/main/java/com/github/damontecres/wholphin/services/PlayerFactory.kt#L123) |
| Swiftfin | iOS / tvOS | jellyfin-sdk-swift 3.4.0 | yes (`ApiKey`, `queryAPIKey: true`) | yes (server URL) | yes (server URL) | no downloads feature found | **works** | [MediaPlayerItemResolver.swift L251](https://github.com/jellyfin/Swiftfin/blob/d8df9924e9c080ec1330b4de2f31fef9107da93d/Shared/Objects/MediaPlayerManager/MediaPlayerItem/MediaPlayerItemResolver.swift#L251) |
| Streamyfin | iOS / Android / TV | @jellyfin/sdk 1.0.2 | yes (`ApiKey`) | yes (server URL) | yes (server URL) | yes (`ApiKey` via sdk `getDownloadUrl`, or TranscodingUrl) | **works** | [getStreamUrl.ts L89](https://github.com/streamyfin/streamyfin/blob/68af07d44394510b32172a65b07dd1d57a6b7295/utils/jellyfin/media/getStreamUrl.ts#L89) |
| Moonfin | Android, Android TV, iOS, tvOS, desktop, web (Flutter) | own (server_jellyfin) | yes (`ApiKey` appended, plus header) | yes | yes (`ApiKey` appended) | yes (header) | **works** | [jellyfin_media_stream_resolver.dart L190, L306-315](https://github.com/Moonfin-Client/Moonfin-Core/blob/8b307a6bc3f4b73a47f941bed6d50626e854ae3e/packages/playback_jellyfin/lib/src/jellyfin_media_stream_resolver.dart#L306-L315) |
| Fladder | Android, iOS, desktop, web (Flutter) | own (chopper, generated) | yes (`ApiKey`) | yes (server URL) | yes (server URL, rewritten to `.srt`, query kept) | yes (`authorization` header) | **works** | [playback_model.dart L460](https://github.com/DonutWare/Fladder/blob/7522e541acad548c0ed3a51803f78eaae90267d8/lib/models/playback/playback_model.dart#L460), [sync_provider.dart L772-787](https://github.com/DonutWare/Fladder/blob/7522e541acad548c0ed3a51803f78eaae90267d8/lib/providers/sync_provider.dart#L772-L787) |
| Finamp | Android / iOS (music) | own | audio: yes (`ApiKey`) | n/a | n/a | n/a | **not applicable**: jellyscale has no music | [music_player_background_task.dart L1424](https://github.com/jmshrv/finamp/blob/74413f5e2356d655e13edde6792ee651400336e1/lib/services/music_player_background_task.dart#L1424) |
| Infuse | Apple | closed source | ? | ? | ? | ? | **unknown** | none (closed source) |
| Kodi: Jellyfin for Kodi (jellyfin-kodi) | Kodi | own Python client | yes (`ApiKey`) in add-on mode | yes (server URL) | yes (`ApiKey`) | n/a | **works** in add-on mode. Native/direct-path mode bypasses the server | [playutils.py L326-334](https://github.com/jellyfin/jellyfin-kodi/blob/4214e240be90d102e3356f482276001d918bc28a/jellyfin_kodi/helper/playutils.py#L326-L334), [L744](https://github.com/jellyfin/jellyfin-kodi/blob/4214e240be90d102e3356f482276001d918bc28a/jellyfin_kodi/helper/playutils.py#L744) |
| Kodi: JellyCon | Kodi | own Python client | **no** | yes (`ApiKey`) | direct stream: yes (server DeliveryUrl). Transcode: **no** (tokenless `Stream.srt`) | n/a | **fails** (direct stream, subtitles on transcode) | [play_utils.py L1278-1285](https://github.com/jellyfin/jellycon/blob/4be74df6f1f669c1a7c034af2232cde533f5316e/resources/lib/play_utils.py#L1278-L1285), [L904-907](https://github.com/jellyfin/jellycon/blob/4be74df6f1f669c1a7c034af2232cde533f5316e/resources/lib/play_utils.py#L904-L907) |
| Jellyfin for Roku | Roku | own BrightScript | **no** | yes (server URL) | yes (server URL) | n/a | **fails** (direct play) | [LoadVideoContentTask.bs L462-473](https://github.com/jellyfin/jellyfin-roku/blob/0dc2ab2ad8b89440be9d87b0e132a46abe9392f0/components/ItemGrid/LoadVideoContentTask.bs#L462-L473) |
| Jellyfin webOS | LG TV | loads the server's `/web/` (jellyfin-web) | yes (from jellyfin-web) | yes | yes | n/a | **works** (same as jellyfin-web) | [index.js L265-344](https://github.com/jellyfin/jellyfin-webos/blob/407a1307bf6bf19041674ea7ee1fbbc2fb33577c/frontend/js/index.js#L265-L344) |
| Jellyfin Tizen | Samsung TV | bundles a jellyfin-web build | yes (`api_key` in the 10.10 web; `ApiKey` in 12.4) | yes | yes | n/a | **works** if the bundled web is 10.10 or later (unverified per build) | [gulpfile.babel.js L10](https://github.com/jellyfin/jellyfin-tizen/blob/31408c121e34e73c9f50c4e201c05e95140675df/gulpfile.babel.js#L10), [web 10.10 playbackmanager L2807](https://github.com/jellyfin/jellyfin-web/blob/2efac6f67a947ce645d014249677898dbe458fac/src/components/playback/playbackmanager.js#L2807) |
| Fluster | Desktop | owner's own | (note only) | | | | not assessed: engineered by its own project to work with jellyscale (D-027) | `docs/status.md` Fluster row |

The **Library** column matters because the Kotlin SDK decides the token question for Kotlin apps. Since v1.5, `ApiClient.createUrl` and the generated `get*Url` builders never add credentials. The v1.5 migration notes say: "Removed includeCredentials … It is no longer possible to instruct the ApiClient to add credentials to URL's" ([docs/migration/v1.5.md](https://github.com/jellyfin/jellyfin-sdk-kotlin/blob/fd742cb676a98b3783c2a92f79286eb0229be773/docs/migration/v1.5.md)). `OkHttpFactory.createClient` adds no auth interceptor. That was verified at both master [fd742cb](https://github.com/jellyfin/jellyfin-sdk-kotlin/blob/fd742cb676a98b3783c2a92f79286eb0229be773/jellyfin-api-okhttp/src/jvmMain/kotlin/org/jellyfin/sdk/api/okhttp/OkHttpFactory.kt#L48-L54) and tag v1.8.12, the version Android TV and Jellyfin Android pin. A Kotlin app therefore sends a token on direct play only if it adds one itself. Jellyfin Android does that. Android TV, Findroid and Wholphin do not.

## Per-client notes

**jellyfin-web.**
- The v12.4 tag puts `ApiKey: apiClient.accessToken()` in the direct-play options. Master does the same through `api.accessToken` (playbackmanager.js L2908 at ebda619).
- Transcoding plays `mediaSource.TranscodingUrl`.
- Downloads use `getLibraryApi(api).getDownloadUrl` (itemContextMenu.js L420, read at master). In `@jellyfin/sdk` 1.0.2 that helper appends `ApiKey` (lib/utils/api/library-api.js L20-21). That is Streamyfin's pinned version; jellyfin-web pins an unstable sdk build, and the v12.4 tag was not read for downloads (unverified).
- Font attachments for ASS add `ApiKey` (plugin.js L1395 at master).
- This is the client the repo's e2e tier already plays.
- Activity: v12.4 was released 2026-10-26, and the master branch is active.

**Jellyfin Desktop (formerly Jellyfin Media Player).** The repo `jellyfin/jellyfin-media-player` is now `jellyfin/jellyfin-desktop`.
- It checks `/System/Info/Public`, then navigates to the server's hosted web client (`native/find-webclient.js` L9-17, `src/system/SystemComponent.cpp` L348). jellyscale serves `/web/` (`internal/api/server.go` L185).
- mpv plays `options.url`, the URL jellyfin-web built with `ApiKey` in it.
- External subtitles load from `DeliveryUrl` (mpvVideoPlayer.js L356-394), which jellyscale ends in `ApiKey`.
- Activity: v2.1.0-pre1 on 2026-11-03 (pre-release). The last stable release is v1.12.0 (2025-03-20). The repo is active.

**Jellyfin Android.**
- The UI is the server's jellyfin-web in a WebView, so its web-player path matches jellyfin-web. Whether that web player or the native player is the default was not read (unverified).
- The native ExoPlayer stack builds a tokenless `getVideoStreamUrl` (QueueManager.kt L351). It plays anyway: a `ResolvingDataSource` adds `Authorization: MediaBrowser … Token=` to every request whose authority matches the server (AppModule.kt L128-141).
- Downloads add the header (FileDownloader.kt L46).
- **Fails on one path:** the "external player" setting hands VLC or MX Player a tokenless `getVideoStreamUrl` ([ExternalPlayer.kt L121-125](https://github.com/jellyfin/jellyfin-android/blob/33656f0d25d95a7a7db7d15d7a3e38650ffe7fdc/app/src/main/java/org/jellyfin/mobile/bridge/ExternalPlayer.kt#L121-L125)). This is opt-in, not the default.
- `SessionBrowserCallback.kt` L346 appends `&ApiKey=` itself. The AppModule comment says the header exists for Android Auto; that this file is the Android Auto path is unverified.
- Activity: v2.7.3 on 2026-09-14. The repo is active.

**Jellyfin Android TV.**
- Both playback stacks call `api.videosApi.getVideoStreamUrl(…static = true…)` with no token: the legacy `PlaybackManager.kt` L41 and the new `playback/jellyfin` resolver L38.
- The ExoPlayer data source is `OkHttpDataSource.Factory(okHttpFactory.createClient(…))` with no interceptor (di/PlaybackModule.kt L45-53, and `single { OkHttpFactory() }` in AppModule.kt L82).
- The only `Authorization` headers set are on telemetry and trickplay.
- Transcode and remux use `createUrl(transcodingUrl)`, which carries the server's `ApiKey`. Subtitles use `createUrl(deliveryUrl)` (VideoManager.java L404).
- The external player (`ExternalPlayerActivity.kt` L109) is tokenless as well.
- Its minimum server is the SDK's `Jellyfin.minimumVersion` (`ServerRepository.kt` L55), which is 10.11.0 at SDK v1.8.12 (Jellyfin.kt L70 at that tag). jellyscale's 12.4.0 passes.
- Activity: v0.20.0 on 2026-11-10. The repo is active.

**Findroid** (confirmed at 0936178, newer than the 013eb52 recording).
- Direct play uses `getVideoStreamUrl(itemId, static = true, mediaSourceId)` with no token. Neither the ExoPlayer nor the mpv player adds a header: a search of the whole repo finds no `Authorization`, `ApiKey` or `DataSource.Factory` in the player code.
- The device profile has no transcoding profiles (JellyfinRepositoryImpl.kt L323), so it never asks for HLS.
- Subtitles use `baseUrl + deliveryUrl` (FindroidMediaStream.kt L31), which carries the server's `ApiKey`.
- **Downloads also fail.** `FindroidSource.path` is the same tokenless stream URL (FindroidSource.kt L25-29). It is handed to Android `DownloadManager.Request` with no `addRequestHeader` (DownloaderImpl.kt L94).
- Activity: v1.2.0 on 2026-10-25. Commits continue to 2026-11-17.

**Wholphin** (Android TV alternative, damontecres/Wholphin).
- Direct play uses `api.videoApi.getVideoStreamUrl(…static = true…)` with no token (PlaybackViewModel.kt L717).
- Its ExoPlayer video player is built in `createVideoPlayer` on `DefaultDataSource.Factory(context)`, which has no auth (PlayerFactory.kt L123, used at L151/L160). The `authOkHttpClient` with the `Authorization` interceptor (hilt/AppModule.kt L100-128) is used only by `createAudioPlayer` (PlayerFactory.kt L203-221) and the theme-song player.
- The MPV backend is the prebuilt artifact `com.github.damontecres.wholphin.mpv:wholphin-mpv` 0.2.4 (gradle/libs.versions.toml L62; app/build.gradle.kts L389-397). Only `wholphin-mpv-stub` is in this repo. Reading that artifact's source (its repo was not located) or a capture would settle whether it sends a header. The code currently steers users off MPV (AppUpgradeHandler.kt L258).
- Transcode (`createUrl(transcodingUrl)`) and subtitles (`createUrl(deliveryUrl)`, L766) carry the server's `ApiKey`.
- Activity: v1.1.1 on 2026-11-06. The repo is very active.

**Swiftfin.**
- Direct play goes through `client.url(with: request, queryAPIKey: true)`. jellyfin-sdk-swift adds the `ApiKey` query item ([JellyfinClient.swift L266-275](https://github.com/jellyfin/jellyfin-sdk-swift/blob/e69dd0358ad33148aded867661e27d71de29d0c3/Sources/JellyfinClient.swift#L266-L275)).
- Transcode and subtitles use `client.url(path:)` on the server's TranscodingUrl and DeliveryUrl, which keep their query (L297-322).
- `Package.resolved` pins sdk-swift 3.4.0 at e69dd03. `gh release list` shows 3.3.0 as the latest GitHub release, but the 3.4.0 tag is what Swiftfin resolves.
- No offline-download code was found in `Shared/` (unverified that none exists elsewhere).
- Activity: 1.7.1 on 2026-11-16. The repo is active.

**Streamyfin.**
- Direct play builds `/Videos/{id}/stream` with `ApiKey: api.accessToken` (getStreamUrl.ts L89).
- Downloads use the sdk's `getDownloadUrl`, which appends `ApiKey`, or the server's TranscodingUrl (L115-130).
- Subtitle downloads send `Authorization`. This rests on a test (providers/Downloads/additionalDownloads.test.ts L144-165), not on the implementation, which was not read.
- Activity: v0.55.2 on 2026-10-16. The repo is active.

**Moonfin** (Moonfin-Client/Moonfin-Core, one Flutter app for Jellyfin, Emby and Plex).
- The Jellyfin resolver appends `ApiKey` to every stream and subtitle URL on the server's host (`_appendAuth`, L306-315, applied at L190/L198/L200). It also sends `Authorization: MediaBrowser …` and `X-Emby-Token` headers (L9-21).
- Downloads send `X-Emby-Token` and `Authorization` (`_buildDownloadRequestHeaders`, download_service.dart L1831-1835 and L1732-1743) unless a companion-plugin ticket supplies its own. Without the plugin there is no ticket (L2681-2693).
- Unverified whether any feature *requires* the Moonfin server plugin. jellyscale serves no plugins (`/Plugins` is a stub).
- Activity: v3.1.1 on 2026-11-19. The repo is very active.

**Fladder.**
- Direct play adds `ApiKey` (playback_model.dart L457-461 and L600-604).
- Transcode uses the server's TranscodingUrl.
- Subtitles rewrite the DeliveryUrl path from `.vtt` to `.srt` and keep its query, so `ApiKey` survives (media_streams_model.dart L411-414). jellyscale renders `srt` (internal/subtitles/subtitles.go L124).
- Downloads build a tokenless `/Videos/{id}/stream?Static=true` (sync_provider.dart L746-755) but send the `authorization: MediaBrowser Token=…` header from `credentials.header` (credentials_model.dart L40-43).
- Activity: v0.12.0 on 2026-11-15. The repo is active.

**Finamp.**
- It sends `ApiKey` on audio, or the header when it can (music_player_background_task.dart L1377-1424).
- jellyscale is video-only (PRODUCT.md: music is a non-goal). `compat/endpoints.yaml` has no `/Audio` routes, so Finamp has nothing to play. It is not a candidate.
- Activity: 1.0.2 on 2026-10-29.

**Infuse.**
- Closed source, so there is no code to read. Verdict unknown.
- **To settle it:** a capture (e.g. mitmproxy) of Infuse playing one film by direct play, one forced to transcode, and one with an external subtitle against a real Jellyfin 12.4. On each `/Videos/{id}/stream*`, `master.m3u8`, segment and `/Subtitles/` request, look for `Authorization`, `X-Emby-Authorization`, `X-Emby-Token`, `X-MediaBrowser-Token`, `ApiKey` or `api_key`.

**Jellyfin for Kodi (jellyfin-kodi).**
- In add-on mode, direct stream is `…/Videos/{id}/stream?static=true&MediaSourceId=…&ApiKey=…` (playutils.py L326-334).
- Transcode rebuilds the server's TranscodingUrl, dropping only bitrate and track-index parameters, so its `ApiKey` survives (L249-303).
- External subtitles add `ApiKey` (L744).
- "Native"/direct-path mode plays `API.get_file_path(source.Path)` (L306-311) rather than an HTTP stream from the server. The exact behaviour of `get_file_path` was not read (unverified). jellyscale's `Path` is the storage key (`internal/library/scan.go` L1085), so this mode is unlikely to find the file.
- `docs/status.md` says Kodi is not a gate.
- Activity: 2.2.0 on 2026-09-17.

**JellyCon.**
- Direct stream builds `{server}/Videos/{id}/stream?static=True&PlaySessionId=…&MediaSourceId=…` with no token. Kodi gets no headers with it: there is no `|Header=` suffix and no inputstream headers anywhere in `resources/lib`.
- Subtitles take one of two paths:
  - On direct stream, external text subtitles come from the server's `DeliveryUrl` (`external_subs`, L941-966), which jellyscale ends in `ApiKey`.
  - On transcode, a text track picked in the dialog (`audio_subs_pref`, called at L471-473) is fetched from `{server}/Videos/{id}/{source}/Subtitles/{index}/Stream.srt` with no token (L904-907, L924-926).
- Only the transcode stream URL adds `ApiKey` (L1307).
- Activity: 1.0.2 on 2026-07-06. The repo is active.

**Jellyfin for Roku.**
- For a file protocol source, `addVideoContentURL` builds `Videos/{id}/stream?Static=true&Container=…&PlaySessionId=…` through `buildURL`. That adds no token (source/api/baserequest.bs L38-79).
- The video node sets no `HttpHeaders`: a search for `httpheaders`, `setHttpAgent` and `AddHeader("Authorization"` finds only `authRequest` for API calls (baserequest.bs L240).
- Transcode (L419) and subtitles (L665) use the server's URLs, which carry `ApiKey`. Trickplay adds `ApiKey` itself, but jellyscale has no Trickplay route.
- Activity: 3.2.4 on 2026-09-28. The repo is active.

**webOS.**
- It fetches `/System/Info/Public` and `/web/manifest.json`, then hands off to the server's hosted jellyfin-web (index.js L265-344).
- Playback is jellyfin-web's HTML5 player, so tokens are as in jellyfin-web.
- **Needs** `/web/manifest.json` to be served. jellyscale serves the 12.4 web-dist under `/web/`, and the local checkout's `web-dist/manifest.json` exists.
- Activity: last release 1.2.2 on 2024-04-10. Commits continue to 2026-11-11.

**Tizen.**
- It bundles jellyfin-web at build time (`JELLYFIN_WEB_DIR`, gulpfile L10). The README tells builders to use `release-10.10.z`, whose direct play sends `api_key` (playbackmanager L2807 on that branch).
- The bundled page presumably runs from the app's own origin and so relies on CORS (unverified). jellyscale allows any origin without credentials, and jellyfin-web sends the token in a header or the query, not a cookie.
- Activity: no GitHub releases. The default branch HEAD is 2025-10-24, though the repo was pushed 2026-11-18.

**Fluster.** Not assessed: Fluster is engineered by its own project to work with jellyscale, and jellyscale does not run it (D-027).

## Routes Jellyfin leaves open that jellyscale closes

`docs/access.md` ("Stricter than Jellyfin") lists them, observed on Jellyfin 12.4:
- the direct-play stream (GET and HEAD)
- `stream.mkv` remux
- font attachments
- subtitles (`Stream.vtt`, `Stream.sup`, and the windowed form)

So a client could also go tokenless on subtitles or attachments without noticing on Jellyfin. For the clients marked "works", subtitles come from the server's `DeliveryUrl` (which jellyscale ends in `ApiKey`) or carry `ApiKey` themselves, as cited above. Attachments are another matter: of the clients searched (Swiftfin, Streamyfin, Moonfin, Fladder, Android TV, Jellyfin Android, Jellyfin Desktop's native scripts), none requests `/Attachments`. jellyfin-web is the only one that does, and it adds `ApiKey`.

## What jellyscale puts in a MediaSource

jellyscale sends `Protocol: "File"` (`internal/media/media.go` L77, `internal/library/scan.go` L1082) and `Path` set to the storage key (`scan.go` L1085). That bears on three clients:
- **Roku** takes its tokenless `Videos/{id}/stream` branch when the protocol is `file`.
- **Findroid** calls `getStreamUrl` when the protocol is `FILE`.
- **Fladder** plays `mediaSource.path` only when it parses as a URL with a scheme and an authority (`isValidVideoUrl`, playback_model.dart L512-515). A storage key does not, so Fladder falls back to its `ApiKey` stream URL.

## Other things that break against a strict jellyscale (not tokens)

- **Quick Connect.** Only `GET /QuickConnect/Enabled` exists, as a stub (`compat/endpoints.yaml`). Apps must sign in with a password. Findroid calls `/QuickConnect/Enabled` at startup (the client recordings). Which other apps offer Quick Connect, and whether each handles the stub cleanly, is unverified.
- **Trickplay.** There are no `/Videos/{id}/Trickplay/…` routes. Clients that request trickplay tiles (Roku, Streamyfin, Fladder, Android TV, Jellyfin Android) get 404. That they degrade to no preview rather than an error is unverified.
- **Server version.** jellyscale reports 12.4.0 (`compat/endpoints.yaml` header). Version gates read:
  - jellyfin-sdk-kotlin master: `minimumVersion = 12.0.0` (Jellyfin.kt L70).
  - jellyfin-sdk-kotlin v1.8.12 (Android TV, Jellyfin Android): 10.11.0.
  - jellyfin-sdk-kotlin v1.9.4 (Findroid, Wholphin): 12.0.0.
  - Whether Findroid and Wholphin enforce the SDK minimum is unverified.
  - All pass.
- **Server plugins.** Moonfin's companion plugin and jellyfin-kodi's optional Kodi Sync Queue are absent. Their impact is unverified.

## What would change the picture

- **The Kotlin SDK or apps on it adding auth to the player.** jellyfin-sdk-kotlin removed `includeCredentials` in v1.5 and has not restored it. If it is restored, or an app adds an auth header to its player's HTTP data source as Jellyfin Android already does, Android TV, Findroid and Wholphin start sending a token. The per-app fix is small: Android TV's `PlaybackModule.kt` L45-53, Findroid's player and downloader, Wholphin's `PlayerFactory.kt` L123. No open issue or PR asking for it was found (a `gh search` of jellyfin-sdk-kotlin, jellyfin and jellyfin-androidtv on 2026-11-20 returned none).
- **Roku and JellyCon adding `ApiKey` to their direct-play URL builders.** Roku's is `addVideoContentURL`; JellyCon's is `get_play_url`, plus its subtitle URLs. Roku already adds `ApiKey` for trickplay, so this is a one-line change in each place.
- **Jellyfin itself requiring auth on `/Videos/{id}/stream`.** Every client would then have to send a token. No such change was found in this search (unverified either way).
- **A device run.** None of the "works" rows has been played on a device against jellyscale. A run (or capture) of Swiftfin, Streamyfin, Moonfin, Fladder, Jellyfin Desktop and Jellyfin Android would turn "works from source" into "plays".
- **Infuse and Wholphin's MPV backend** stay unknown until a capture as described above.
- **Repo records are out of date.** the client recordings pin Findroid 013eb52, Android TV e14b088, Swiftfin ffc4398 and Streamyfin c82f5bf (2026-11-12). The HEADs read here are newer, and the verdicts did not change.
