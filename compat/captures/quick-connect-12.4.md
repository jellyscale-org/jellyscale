# Quick Connect on Jellyfin 12.4 (JS-037)

`quick-connect-12.4.json`, from `python3 -I test/differential/capture_quick_connect.py compat/captures/quick-connect-12.4.json` (about 25 minutes; `capture_quick_connect_web.mjs` is its browser part): a disposable `jellyfin/jellyfin:12.4` (12.4.0) on a Docker-assigned port, synthetic accounts `admin`, `viewer`, `other`, `gone`, no library. `cmd/jellyscale/quickconnect_capture_integration_test.go` asks jellyscale the `steps`.

## Routes and answers

Quick Connect is on by default (`QuickConnectAvailable` true). `GET /QuickConnect/Enabled` answers `true`.

| Request | State | Answer |
|---|---|---|
| `POST /QuickConnect/Initiate` | device fields in `Authorization` | 200 `{Authenticated:false, Secret (64 hex), Code (6 digits), DeviceId, DeviceName, AppName, AppVersion, DateAdded}` |
| Initiate | any of DeviceId, Device, Client, Version missing or empty, or no header | 400 `Error processing request.` |
| Initiate, Connect, redemption | with an unknown, a signed-out or another account's token | as with none; the other account's token still works |
| `GET /QuickConnect/Connect?Secret=` | pending / approved / redeemed | 200, the same fields; `Authenticated` true once approved, and `DateAdded` becomes the approval's time plus 60 s. No token |
| Connect | unknown secret / no secret | 404 `"Unknown secret"` / 400 validation problem |
| `POST /QuickConnect/Authorize?code=[&userId=]` | pending, own account (userId own, dashed or capitals, empty, or absent) | 200 `true` |
| Authorize | unknown code, or the code typed with a space | 404 `Error processing request.` |
| Authorize | already approved (by anyone) or redeemed | 500 `Error processing request.` |
| Authorize | no code / malformed userId / no token | 400 / 400 / 401 |
| Authorize | viewer naming another account or an unknown id | 403 (the code stays pending) |
| Authorize | **administrator naming another account** | 200, and the device is signed in **as that account** |
| Authorize | administrator naming an unknown id | 400 |
| `POST /Users/AuthenticateWithQuickConnect {Secret}` | approved | 200, a sign-in (`User`, `SessionInfo`, `AccessToken`, `ServerId`) for the **initiating** device, whatever the redemption's own `Authorization` says (or with none) |
| redemption | pending ("not yet") or unknown secret | 404 `Error processing request.` (the same) |
| redemption | empty or no Secret | 400 |
| redemption | a second time | 200 with the **same** token |
| redemption | after the approving account was **deleted** / **disabled** | 200 with a token that answers 401 |

A redemption on a device that held a token for the same account replaces it (the old one answers 401); a token for another account on the device is kept. A password sign-in on one device does the same. `/Sessions` lists the initiating device with its name, app and version at once; it does not list the device whose header came with the redemption. The approver's token keeps working.

## Off

With `QuickConnectAvailable` false: `Enabled` answers `false`; Initiate (with device fields), Connect and Authorize (with a token) answer 401 `"Quick connect is disabled"`; redemption answers 401 `Error processing request.`; Authorize with no token answers 401 with no body; Initiate with no `Authorization` answers 400. A code approved before is refused while off and redeemable again once on; a pending one is still pending once on.

## How long codes live

`expiry` rows. Seconds are from the batch's Initiate.

- **Pending:** there at 590 s after Initiate, gone at 612 s: Connect `Unknown secret`, approval and redemption 404.
- **Approved, redemption:** 200 at 589 s after an approval made 1 s after Initiate and at 590 s after one made 540 s after Initiate; 404 at 611 s and 612 s. So redemption is possible for about **10 minutes from approval**, whenever the approval came. A code redeemed in that time is refused after it too.
- **Approved, Connect:** still answers `Authenticated: true` 612 to 649 s after approval, when redemption already answers 404; `Unknown secret` at 674 s. Connect follows the moved `DateAdded` plus 10 minutes (about 11 minutes after approval); jellyscale need not copy that gap.

## jellyfin-web 12.4 (`web`)

- Login page, Quick Connect on: a **Use Quick Connect** button. Picking it sends `POST /QuickConnect/Initiate` (device fields, no token), shows a dialog `Quick Connect / Enter code NNNNNN to login / Got It`, asks `GET /QuickConnect/Connect?Secret=` every 5 s, and once approved sends `POST /Users/AuthenticateWithQuickConnect {"Secret"}` and goes to the home page signed in.
- Quick Connect page (`#/quickconnect`, linked from the user menu), viewer: a code field and **Authorize**. It strips spaces and sends `POST /QuickConnect/Authorize?code=` with no `userId`. An unknown code shows `Unknown Quick Connect code`; success shows `You have successfully authenticated your device!`.
- The same page, administrator: a **User** list (itself selected) as well; it sends `userId` of the selected account, here its own.
- A refused Initiate (401, 403, 429, 500 or 503, with or without `Retry-After`, answered by the capture in the server's place) shows the same dialog, `Error / Quick Connect is not active on this server`, and stays on the sign-in form. No status shows a connection failure, so the choice for the 100-code cap is free as far as jellyfin-web goes; Moonfin reads 401, or the word `disabled` in the body, as Quick Connect turned off (`moonfin-quick-connect-2026-11-26.md`).
- Off: no button and no menu link. The Quick Connect page opened by its address still shows the form, and submitting it shows `Unknown Quick Connect code` (the server answered 401).

## Not observed

- jellyfin-web's administrator page was only seen to send the selected account's own id; an empty `userId` was sent by the capture itself.
- The exact instant each window closes: the rows bracket it (589 to 611 s for redemption, 649 to 674 s for Connect after approval).
- Whether two live codes can be equal, and any limit on live codes or on guessing: none was provoked (jellyscale's bounds are D-078 and D-079).
- Quick Connect being refused for a disabled account at approval time: only disabling after approval was asked.
- Moonfin against Jellyfin 12.4: Moonfin was run against jellyscale only.
