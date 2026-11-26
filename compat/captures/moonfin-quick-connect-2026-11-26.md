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

## What the run after the change records

The same app and emulator: Initiate shows a six-digit code; the code is approved on jellyfin-web's Quick Connect page by a signed-in viewer; Moonfin signs in as that viewer without a password and plays the film. With `"quickConnect": false`, Moonfin's sign-in screen falls back to the password form.
