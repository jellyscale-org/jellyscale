# Moonfin and Quick Connect (JS-037)

## The client and the device

- Moonfin 3.1.1, Moonfin-Client/Moonfin-Core at tag `3.1.1`, commit `d71d84604fbacfb5f2deb0589b1486cb22f43244` (D-030): the release's `Moonfin_Android_v3.1.1.apk`, sha256 `d09b7176a7274ab75d9b3c9ec909738687f40fd60082270877d4111cccd0d13d` (package `org.moonfin.androidtv`, a leanback app).
- An Android TV emulator made for the run: device `tv_1080p`, system image `system-images;android-36;android-tv;x86_64` (Android 16, model `sdk_google_atv64_x86_64`), headless, with its own AVD directory (`ANDROID_AVD_HOME`), adb server port (`ANDROID_ADB_SERVER_PORT`) and console port, so no emulator already running is touched. No physical device.
- It reaches a server on the host's loopback at `http://10.0.2.2:<port>`. Its text fields open an on-screen keyboard that takes `adb shell input text` once open.

## What the source does

Read at the commit above.

- `packages/server_jellyfin/lib/src/api/jellyfin_auth_api.dart`: `POST /QuickConnect/Initiate`, `GET /QuickConnect/Connect?Secret=`, `POST /Users/AuthenticateWithQuickConnect` `{"Secret": ...}`. It never asks `/QuickConnect/Enabled` for a Jellyfin server.
- `lib/ui/screens/auth/login_screen.dart` (`_startQuickConnect`): it takes Quick Connect to be turned off, and falls back to the password form, when Initiate answers 401 or the error text contains `disabled`. Any other failure shows `QuickConnect unavailable (<status>, ...): <body>`. It polls Connect 5 seconds after each answer, and when `Authenticated` is true it redeems the secret.

## 2026-11-26, jellyscale at 8e47ede (before JS-037)

`jellyscale --roles=all`, one synthetic 2 minute H.264/AAC MP4 film, the account `admin`.

1. Add Server, Server Address: Moonfin found the server as Jellyfin 12.4.0 and showed Sign In with `Quick Connect` chosen.
2. **Quick Connect failed:** the server logged `unknown route` `POST /quickconnect/initiate` (404), and Moonfin showed `QuickConnect unavailable (404, badResponse): 404 page not found`, with no code.
3. Password, as a control: Moonfin signed in, listed the film, and played it (10 seconds in on screen; `/Sessions` showed `Moonfin for Android TV` 3.1.1 playing it). So signing in by code is the only part of the criterion that fails today.

## 2026-11-26, jellyscale at 5814ff8 (with JS-037)

The same APK (sha256 checked again, installed `versionName` 3.1.1) on a new headless Android TV emulator: device `tv_1080p`, `system-images;android-36;android-tv;x86_64`, build `google/sdk_google_atv64_x86_64/emu64xa:16/BT2A.260319.001/15058170`, emulator 37.3.10.0, its own AVD directory. It used the host's running adb server, with every command naming the new emulator's serial. The server was `jellyscale --roles=all` with a disposable Postgres, one synthetic 2 minute 720p H.264/AAC MP4 film and the throwaway account `admin` (an administrator, the only account). The app was driven with `adb shell input`.

**The code was approved from the host, not on jellyfin-web's Quick Connect page:** the host signed in as `admin` with the password and sent `POST /QuickConnect/Authorize?code=<code>` with that token. The run in jellyfin-web is below.

### Quick Connect on (the default; the key left out)

1. Add Server, Server Address, Connect: the app showed Server found, Jellyfin, Version 12.4.0. Continue opened Sign In with `Quick Connect` chosen, a six-digit code, a QR code and `Waiting for authorization...`. The QR code was not decoded in this run.
2. The host's approval answered 200 `true`. The app's next Connect poll was followed by redemption (200), and the app went to its first-run setup, with no password typed on the device. Skip setup led to the home screen with the film.
3. `GET /Sessions` with the approver's token (which still worked after redemption) showed user `admin`, client `Moonfin for Android TV`, version 3.1.1, the emulator's device name.
4. Play: moving picture and the player's bar at 0:26 of 2:00. `/Sessions` showed `Test Card`, `DirectPlay`, not paused, at 15.3 s and 25.3 s, 10 seconds apart. Back sent Stopped (204).

The Quick Connect requests and playback, in order (paths as the server logs them, lowercased; it logs no queries, headers or bodies):

| Request | Status | Sent by |
|---|---|---|
| `POST /quickconnect/initiate` | 200 | app |
| `GET /quickconnect/connect` (polls 5 s apart, before approval) | 200 | app |
| `POST /users/authenticatebyname` | 200 | host |
| `POST /quickconnect/authorize` | 200 | host |
| `GET /quickconnect/connect` | 200 | app |
| `POST /users/authenticatewithquickconnect` | 200 | app |
| `POST /items/{id}/playbackinfo` | 200 | app |
| `GET /videos/{id}/stream` | 200 | app |
| `POST /sessions/playing`, `.../progress`, `.../stopped` | 204 | app |

The app was never seen to ask `/QuickConnect/Enabled`: the one such request in each run was the host's.

### Quick Connect off (`"quickConnect": false`)

The server was restarted on the same database and the app's data cleared (`pm clear`). The host's `GET /QuickConnect/Enabled` answered 200 `false`.

1. Adding the server again and Continue: the app sent `POST /quickconnect/initiate`, got 401, and showed Sign In with `Password` chosen and the username and password fields, with no error text. The `Quick Connect` button was still shown; it was not pressed again.
2. Typing the username and password and Done on the keyboard: the app sent `POST /users/authenticatebyname` (200) and went to first-run setup. The home screen offered the film under Continue Watching.
3. Play: it resumed with a moving picture, the player's bar at 1:01 of 2:00. `/Sessions` showed `DirectPlay`, not paused, at 47.3 s, then 57.3 s about 10 seconds later. The server logged `playbackinfo` 200, `/videos/{id}/stream` 200 and 206, Playing and progress 204, and Stopped 204 after Back. `/Sessions` also still listed an idle second Moonfin session, from before the app's data was cleared.

### The app's other requests

Apart from the Initiate 401 above, every request in the two runs answered 200, 204, 206 or (`/socket`) 101, except these 404s, none of which was seen to stop a step: `/api/v2/system/info`, `/moonfin/ping`, `/moonfin/pin`, `/moonfin/games/libraries`, `/moonfin/animemarkers/items`, `/moonfin/posterbadges/items`, `/moonfin/watchparty/config`, `/plugins/achievementbadges/public-config` and `/users/{id}/images/primary` (the account has no picture). The server logged no ERROR while the app was connected.

## 2026-11-26, approved in jellyfin-web, jellyscale at 5814ff8

The same APK (sha256 checked again) and emulator image, on another new headless emulator, with a new disposable Postgres and the same kind of film. jellyscale served jellyfin-web copied from the `jellyfin/jellyfin:12.4` image (`sha256:fea863d0cd9ec445985d9c75fc6ed64e38a1d2766cf0c250a33d553f4fc1b771`). The browser was headless Chrome 155 (agent-browser, a session of its own, a 1280x800 desktop viewport) at `http://localhost:<port>/web/`. The viewer `qc-viewer`, not an administrator (`is_admin` false, and `IsAdministrator` false in the browser), was added as a database row, as `test/e2e/play.mjs` does, and signed in to jellyfin-web with its password.

### The code typed on the Quick Connect page

1. Moonfin showed a six-digit code. The browser opened `#/quickconnect` by its address (not through the menus), the code was typed in `Quick Connect code`, and Authorize was pressed. `POST /QuickConnect/Authorize` answered 200 and the page showed `You have successfully authenticated your device!`.
2. After its next poll Moonfin redeemed the code (`/quickconnect/authenticatewithquickconnect` 200) and showed first-run setup, with no password typed on the device.
3. After Skip setup the film played: `/Sessions` (asked by the host as the administrator) showed `qc-viewer`, `Moonfin for Android TV` 3.1.1, `Test Card`, `DirectPlay`, not paused, at 14.2 s and 24.2 s about 10 seconds apart. The player's bar showed 0:27 of 2:00. The server logged `playbackinfo` and `/videos/{id}/stream` 200, and Playing, progress and Stopped 204.

### The QR code's link, with a second code

The app's data was cleared and the server added again. `zbarimg` decoded a screenshot of the TV to `<server>/web/#/quickconnect?code=NNNNNN`: `<server>` is the address typed in the app, and the code is the one on the screen. No phone camera scanned it.

- With the host replaced by `localhost`, the link was loaded (from `about:blank`) in the browser still signed in as `qc-viewer`. It opened the Quick Connect page with the code filled in.
- It did not approve by itself: after 6 seconds the field and the Authorize button were still shown, and the server had no second `authorize` request. **The viewer must press Authorize.** Pressing it answered 200 with the success text, and Moonfin signed in; `/Sessions` named `qc-viewer`.
- Not tried: the link in a browser that is not signed in.

### A request jellyscale does not serve

Each time the Quick Connect page opened, jellyfin-web sent `GET /Users` three times and got 404 (`unknown route`, also seen in the browser's request list). Approval worked both times, for this non-administrator. The administrator's page is below.

## 2026-11-26, the administrator's Quick Connect page, jellyscale at c1d192d

No Moonfin or emulator: `--roles=api,stream` with a disposable Postgres, jellyfin-web from the `jellyfin/jellyfin:12.4` image, headless Chrome 155 (1280x800) signed in as the administrator `admin`, with `qc-viewer` as a second account. Each code came from the host's `POST /QuickConnect/Initiate`, and the host redeemed its secret.

- The user menu links `Quick Connect`, and it opened `#/quickconnect`.
- The page showed a **User** list, a code field and Authorize. **The list was empty**: no options, an empty value, and clicking it showed none. Opening the page from the menu, the server logged three `GET /Users` 404s. Against Jellyfin 12.4 the list had the administrator selected (`quick-connect-12.4.md`).

| Code | Authorize request | Page after | Redeeming the secret |
|---|---|---|---|
| typed, page opened from the menu | `POST /QuickConnect/Authorize?code=NNNNNN&userId=` (`userId` present and empty), 200 | `You have successfully authenticated your device!` | 200, signed in as `admin` (administrator) for the asking DeviceId, with a token |
| filled in by the link `#/quickconnect?code=NNNNNN` (`localhost`, loaded from `about:blank`) | the same form, 200, body `true` (the body was read for this request only) | the same text | the same |

The link filled in the code with the User list still empty, and had not approved after 6 seconds: Authorize had to be pressed. The browser stayed signed in as `admin`. With an empty list, no other account could be chosen on this page.

## Not verified

- A physical device; a film that transcodes (only direct play ran).
- Which engine decoded: logcat shows `media_kit` (libmpv) in the app, and the engine in use was not checked separately.
- The token's DeviceId against the one sent with Initiate: the log has no headers.
- Sound (the emulator ran with no audio).
