# Quick Connect on Jellyfin 12.4 (JS-037)

This file is written by `test/differential/capture_quick_connect.py`: edit the script, not this file. `python3 -I test/differential/capture_quick_connect.py --markdown compat/captures/quick-connect-12.4.json` writes it again from the capture.

`quick-connect-12.4.json`, from `python3 -I test/differential/capture_quick_connect.py compat/captures/quick-connect-12.4.json` (about 25 minutes; `capture_quick_connect_web.mjs` is its browser part): a disposable `jellyfin/jellyfin:12.4` (12.4.0) on a Docker-assigned port, synthetic accounts `admin`, `viewer`, `other`, `gone`, no library. `cmd/jellyscale/quickconnect_capture_integration_test.go` asks jellyscale the `steps`.

## The observations that matter

- **The sign-in is made at approval, not at redemption** (an inference): a second redemption hands out the same token, and a redemption after the approving account was deleted or disabled still answers 200 with that account's sign-in, whose token answers 401.
- **A second approval answers 500** `Error processing request.`, from the same account or another, before and after redemption.
- **Redeeming a second time answers 200 with the same token**, and that token goes on working.
- **Initiate missing any of DeviceId, Device, Client or Version answers 400**, missing or empty, with Quick Connect on. With it off, Initiate with those fields answers 401 `"Quick connect is disabled"`, and with no `Authorization` still 400.
- **Both windows are 10 minutes:** a pending code from Initiate, an approved one's redemption from approval (`How long codes live` below).

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

- Whether the device's old token of the same account stops working at approval or at redemption: `/Users/Me` was asked with it only after the redemption.
- jellyfin-web's administrator page was only seen to send the selected account's own id; an empty `userId` was sent by the capture itself.
- The exact instant each window closes: the rows bracket it (589 to 611 s for redemption, 649 to 674 s for Connect after approval).
- Whether two live codes can be equal, and any limit on live codes or on guessing: none was provoked (jellyscale's bounds are D-078 and D-079).
- Quick Connect being refused for a disabled account at approval time: only disabling after approval was asked.
- Moonfin against Jellyfin 12.4: Moonfin was run against jellyscale only.

## Every step

In the order asked. `{secret:x}`, `{code:x}`, `{token:x}` and `{userid:x}` stand for the server's values.

### on

| Step | Request | Status | Answer |
|---|---|---|---|
| *password_signin* | user admin, device approver-admin, token admin | | |
| *password_signin* | user viewer, device approver-viewer, token viewer | | |
| *password_signin* | user other, device approver-other, token other | | |
| enabled by default | `GET /QuickConnect/Enabled` | 200 | `true` |
| initiate | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-tv1", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| connect pending | `GET /QuickConnect/Connect?Secret={secret:tv1}` | 200 | `{"Authenticated": false, "DeviceId": "dev-tv1", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| connect unknown secret | `GET /QuickConnect/Connect?Secret=0000deadbeef` | 404 | `Unknown secret` |
| connect without a secret | `GET /QuickConnect/Connect` | 400 | `{"title": "One or more validation errors occurred.", "errors": {"secret": ["The secret field is required."]}}` |
| redeem pending | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:tv1}"}` | 404 | `Error processing request.` |
| redeem unknown secret | `POST /Users/AuthenticateWithQuickConnect {"Secret":"0000deadbeef"}` | 404 | `Error processing request.` |
| redeem an empty secret | `POST /Users/AuthenticateWithQuickConnect {"Secret":""}` | 400 | `{"title": "One or more validation errors occurred.", "errors": {"Secret": ["The Secret field is required."]}}` |
| redeem without a secret | `POST /Users/AuthenticateWithQuickConnect {}` | 400 | `{"title": "One or more validation errors occurred.", "errors": {"Secret": ["The Secret field is required."]}}` |
| approve an unknown code | `POST /QuickConnect/Authorize?code=000001` | 404 | `Error processing request.` |
| approve without a code | `POST /QuickConnect/Authorize` | 400 | `{"title": "One or more validation errors occurred.", "errors": {"code": ["The code field is required."]}}` |
| approve without a token | `POST /QuickConnect/Authorize?code={code:tv1}` | 401 |  |
| viewer names another user | `POST /QuickConnect/Authorize?code={code:tv1}&userId={userid:other}` | 403 | `Error processing request.` |
| connect after a refused approval | `GET /QuickConnect/Connect?Secret={secret:tv1}` | 200 | `{"Authenticated": false, "DeviceId": "dev-tv1", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| viewer approves naming themselves | `POST /QuickConnect/Authorize?code={code:tv1}&userId={userid:viewer}` | 200 | `true` |
| connect approved | `GET /QuickConnect/Connect?Secret={secret:tv1}` | 200 | `{"Authenticated": true, "DeviceId": "dev-tv1", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve an approved code again | `POST /QuickConnect/Authorize?code={code:tv1}&userId={userid:viewer}` | 500 | `Error processing request.` |
| another account approves an approved code | `POST /QuickConnect/Authorize?code={code:tv1}` | 500 | `Error processing request.` |
| redeem approved | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:tv1}"}` | 200 | `sign-in of viewer for dev-tv1, AccessToken {token:tv1-first}` |
| redeem a second time | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:tv1}"}` | 200 | `sign-in of viewer for dev-tv1, AccessToken {token:tv1-first}` |
| first token after the second redemption | `GET /Users/Me` | 200 | `the user viewer` |
| second token | `GET /Users/Me` | 200 | `the user viewer` |
| connect after redemption | `GET /QuickConnect/Connect?Secret={secret:tv1}` | 200 | `{"Authenticated": true, "DeviceId": "dev-tv1", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve after redemption | `POST /QuickConnect/Authorize?code={code:tv1}` | 500 | `Error processing request.` |

### initiate

| Step | Request | Status | Answer |
|---|---|---|---|
| initiate without Client | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| initiate with an empty Client | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| initiate without Device | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| initiate with an empty Device | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| initiate without DeviceId | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| initiate with an empty DeviceId | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| initiate without Version | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| initiate with an empty Version | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| initiate with no Authorization | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| *password_signin* | user viewer, device to-revoke, token revoked | | |
| sign the token out | `POST /Sessions/Logout` | 204 |  |
| initiate with the unknown token | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-stale-unknown", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| connect with the unknown token | `GET /QuickConnect/Connect?Secret={secret:stale-unknown}` | 200 | `{"Authenticated": false, "DeviceId": "dev-stale-unknown", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve the code initiated with the unknown token | `POST /QuickConnect/Authorize?code={code:stale-unknown}` | 200 | `true` |
| redeem with the unknown token | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:stale-unknown}"}` | 200 | `sign-in of viewer for dev-stale-unknown, AccessToken {token:stale-unknown}` |
| initiate with the revoked token | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-stale-revoked", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| connect with the revoked token | `GET /QuickConnect/Connect?Secret={secret:stale-revoked}` | 200 | `{"Authenticated": false, "DeviceId": "dev-stale-revoked", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve the code initiated with the revoked token | `POST /QuickConnect/Authorize?code={code:stale-revoked}` | 200 | `true` |
| redeem with the revoked token | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:stale-revoked}"}` | 200 | `sign-in of viewer for dev-stale-revoked, AccessToken {token:stale-revoked}` |
| initiate with the other token | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-stale-other", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| connect with the other token | `GET /QuickConnect/Connect?Secret={secret:stale-other}` | 200 | `{"Authenticated": false, "DeviceId": "dev-stale-other", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve the code initiated with the other token | `POST /QuickConnect/Authorize?code={code:stale-other}` | 200 | `true` |
| redeem with the other token | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:stale-other}"}` | 200 | `sign-in of viewer for dev-stale-other, AccessToken {token:stale-other}` |
| the other account's token after being sent along | `GET /Users/Me` | 200 | `the user other` |

### naming

| Step | Request | Status | Answer |
|---|---|---|---|
| initiate for the administrator | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-adm-self", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| administrator approves naming themselves | `POST /QuickConnect/Authorize?code={code:adm-self}&userId={userid:admin}` | 200 | `true` |
| redeem the administrator's own approval | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:adm-self}"}` | 200 | `sign-in of admin for dev-adm-self, AccessToken {token:adm-self}` |
| initiate for naming another | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-adm-other", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| administrator names another user | `POST /QuickConnect/Authorize?code={code:adm-other}&userId={userid:other}` | 200 | `true` |
| connect after the administrator named another | `GET /QuickConnect/Connect?Secret={secret:adm-other}` | 200 | `{"Authenticated": true, "DeviceId": "dev-adm-other", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| redeem the approval naming another | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:adm-other}"}` | 200 | `sign-in of other for dev-adm-other, AccessToken {token:adm-other}` |
| initiate for no userId | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-adm-none", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| administrator approves with no userId | `POST /QuickConnect/Authorize?code={code:adm-none}` | 200 | `true` |
| redeem with no userId named | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:adm-none}"}` | 200 | `sign-in of admin for dev-adm-none, AccessToken {token:adm-none}` |
| initiate for an empty userId | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-adm-empty", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| administrator approves with an empty userId | `POST /QuickConnect/Authorize?code={code:adm-empty}&userId=` | 200 | `true` |
| redeem the administrator's empty userId | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:adm-empty}"}` | 200 | `sign-in of admin for dev-adm-empty, AccessToken {token:adm-empty}` |
| initiate for the viewer's empty userId | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-view-empty", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| viewer approves with an empty userId | `POST /QuickConnect/Authorize?code={code:view-empty}&userId=` | 200 | `true` |
| redeem the viewer's empty userId | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:view-empty}"}` | 200 | `sign-in of viewer for dev-view-empty, AccessToken {token:view-empty}` |
| initiate for unknown users | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-unknown-user", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| administrator names a user nobody is | `POST /QuickConnect/Authorize?code={code:unknown-user}&userId=11111111111111111111111111111111` | 400 | `Error processing request.` |
| initiate for the viewer naming unknown users | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-unknown-user2", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| viewer names a user nobody is | `POST /QuickConnect/Authorize?code={code:unknown-user2}&userId=11111111111111111111111111111111` | 403 | `Error processing request.` |
| viewer names a malformed userId | `POST /QuickConnect/Authorize?code={code:unknown-user2}&userId=zzz` | 400 | `{"title": "One or more validation errors occurred.", "errors": {"userId": ["The value 'zzz' is not valid."]}}` |
| initiate for a dashed userId | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-view-dashed", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| viewer names themselves dashed and in capitals | `POST /QuickConnect/Authorize?code={code:view-dashed}&userId={userid-dashed:viewer}` | 200 | `true` |
| initiate for a spaced code | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-spaced", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve a code with a space in it | `POST /QuickConnect/Authorize?code={code:spaced-out}` | 404 | `Error processing request.` |

### devices

| Step | Request | Status | Answer |
|---|---|---|---|
| *password_signin* | user viewer, device same-account, token same-old | | |
| initiate on a device signed in to the same account | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-same-account", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve the same account | `POST /QuickConnect/Authorize?code={code:same-account}` | 200 | `true` |
| redeem on a device signed in to the same account | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:same-account}"}` | 200 | `sign-in of viewer for dev-same-account, AccessToken {token:same-new}` |
| the device's old token of the same account | `GET /Users/Me` | 401 |  |
| the device's new token | `GET /Users/Me` | 200 | `the user viewer` |
| *password_signin* | user other, device two-accounts, token two-other | | |
| initiate on a device signed in to another account | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-two-accounts", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve a different account | `POST /QuickConnect/Authorize?code={code:two-accounts}` | 200 | `true` |
| redeem on a device signed in to another account | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:two-accounts}"}` | 200 | `sign-in of viewer for dev-two-accounts, AccessToken {token:two-new}` |
| the device's token of the other account | `GET /Users/Me` | 200 | `the user other` |
| the device's new token beside it | `GET /Users/Me` | 200 | `the user viewer` |
| *password_signin* | user viewer, device by-password, token pw-viewer-old | | |
| *password_signin* | user other, device by-password, token pw-other | | |
| *password_signin* | user viewer, device by-password, token pw-viewer-new | | |
| password: the first token of the account signed in again | `GET /Users/Me` | 401 |  |
| password: another account's token on the device | `GET /Users/Me` | 200 | `the user other` |
| password: the account's new token | `GET /Users/Me` | 200 | `the user viewer` |
| initiate for another header at redemption | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-asker", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve the asker | `POST /QuickConnect/Authorize?code={code:asker}` | 200 | `true` |
| redeem with another device's header | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:asker}"}` | 200 | `sign-in of viewer for dev-asker, AccessToken {token:asker}` |
| the asker's session | `GET /Sessions` | 200 | `[{"DeviceId": "dev-asker", "DeviceName": "TV-asker", "Client": "capture", "ApplicationVersion": "1.0", "UserName": "viewer"}]` |
| the other header's session | `GET /Sessions` | 200 | `[]` |
| initiate for a redemption with no Authorization | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-bare", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve the bare redemption | `POST /QuickConnect/Authorize?code={code:bare}` | 200 | `true` |
| redeem with no Authorization | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:bare}"}` | 200 | `sign-in of viewer for dev-bare, AccessToken {token:bare}` |
| the approver's own token afterwards | `GET /Users/Me` | 200 | `the user viewer` |

### gone

| Step | Request | Status | Answer |
|---|---|---|---|
| *password_signin* | user gone, device approver-gone, token gone | | |
| initiate for an account to delete | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-gone", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| the account to delete approves | `POST /QuickConnect/Authorize?code={code:gone}` | 200 | `true` |
| *delete_user* | user gone | | |
| connect after the approver is deleted | `GET /QuickConnect/Connect?Secret={secret:gone}` | 200 | `{"Authenticated": true, "DeviceId": "dev-gone", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| redeem after the approver is deleted | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:gone}"}` | 200 | `sign-in of gone for dev-gone, AccessToken {token:gone-redeemed}` |
| the token redeemed after deletion | `GET /Users/Me` | 401 |  |
| initiate for an account to disable | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-disabled", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| the account to disable approves | `POST /QuickConnect/Authorize?code={code:disabled}` | 200 | `true` |
| *set_disabled* | user other, disabled True | | |
| redeem after the approver is disabled | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:disabled}"}` | 200 | `sign-in of other for dev-disabled, AccessToken {token:disabled-redeemed}` |
| the token redeemed after the approver was disabled | `GET /Users/Me` | 401 |  |
| *set_disabled* | user other, disabled False | | |

### off

| Step | Request | Status | Answer |
|---|---|---|---|
| initiate before turning off, left pending | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-off-pending", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| initiate before turning off, approved | `POST /QuickConnect/Initiate` | 200 | `{"Authenticated": false, "DeviceId": "dev-off-approved", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| approve before turning off | `POST /QuickConnect/Authorize?code={code:off-approved}` | 200 | `true` |
| *set_enabled* | enabled False | | |
| enabled when off | `GET /QuickConnect/Enabled` | 200 | `false` |
| initiate when off | `POST /QuickConnect/Initiate` | 401 | `Quick connect is disabled` |
| initiate when off with no Authorization | `POST /QuickConnect/Initiate` | 400 | `Error processing request.` |
| connect a pending code when off | `GET /QuickConnect/Connect?Secret={secret:off-pending}` | 401 | `Quick connect is disabled` |
| connect an approved code when off | `GET /QuickConnect/Connect?Secret={secret:off-approved}` | 401 | `Quick connect is disabled` |
| connect an unknown secret when off | `GET /QuickConnect/Connect?Secret=0000deadbeef` | 401 | `Quick connect is disabled` |
| approve a pending code when off | `POST /QuickConnect/Authorize?code={code:off-pending}` | 401 | `Quick connect is disabled` |
| approve without a token when off | `POST /QuickConnect/Authorize?code={code:off-pending}` | 401 |  |
| redeem a code approved before turning off | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:off-approved}"}` | 401 | `Error processing request.` |
| redeem a pending code when off | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:off-pending}"}` | 401 | `Error processing request.` |
| redeem an unknown secret when off | `POST /Users/AuthenticateWithQuickConnect {"Secret":"0000deadbeef"}` | 401 | `Error processing request.` |
| *set_enabled* | enabled True | | |
| redeem the approved code once turned on again | `POST /Users/AuthenticateWithQuickConnect {"Secret":"{secret:off-approved}"}` | 200 | `sign-in of viewer for dev-off-approved, AccessToken {token:off-approved}` |
| connect the pending code once turned on again | `GET /QuickConnect/Connect?Secret={secret:off-pending}` | 200 | `{"Authenticated": false, "DeviceId": "dev-off-pending", "(fields)": "Authenticated Secret Code DeviceId DeviceName AppName AppVersion DateAdded"}` |
| *password_signin* | user viewer, device approver-viewer, token viewer | | |

## What jellyfin-web showed

| Quick Connect | View | Route | Button | Dialogs | Page text |
|---|---|---|---|---|---|
| on | login_page | `#/login` | `Use Quick Connect` |  | `Please sign in User Password Remember Me Sign In Use Quick Connect Forgot Password` |
| on | login_dialog | `#/login` | `Use Quick Connect` | `Quick Connect Enter code <code> to login Got It` | `Please sign in User Password Remember Me Sign In Use Quick Connect Forgot Password` |
| on | viewer_menu | `#/mypreferencesmenu` |  |  | `viewer Profile Display Home Playback Subtitles Controls` |
| on | viewer_page | `#/quickconnect` |  |  | `Quick Connect To sign in with Quick Connect, select the 'Quick Connect' button on the device you are logging in from and enter the displayed code below.   Quick Connect code Authorize` |
| on | viewer_unknown_code | `#/quickconnect` |  |  | `Quick Connect To sign in with Quick Connect, select the 'Quick Connect' button on the device you are logging in from and enter the displayed code below.   Unknown Quick Connect code Quick Connect code Authorize` |
| on | viewer_approved | `#/quickconnect` |  |  | `Quick Connect To sign in with Quick Connect, select the 'Quick Connect' button on the device you are logging in from and enter the displayed code below.    You have successfully authenticated your device!  Go Home` |
| on | tv_after | `#/home` |  |  | `Nothing here.  Ask an administrator to create a library.` |
| on | admin_menu | `#/mypreferencesmenu` |  |  | `admin Profile Display Home Playback Subtitles Controls` |
| on | admin_page | `#/quickconnect` |  |  | `Quick Connect To sign in with Quick Connect, select the 'Quick Connect' button on the device you are logging in from and enter the displayed code below.   User admin other viewer Quick Connect code Authorize` |
| on | admin_approved | `#/quickconnect` |  |  | `Quick Connect To sign in with Quick Connect, select the 'Quick Connect' button on the device you are logging in from and enter the displayed code below.    You have successfully authenticated your device!  Go Home` |
| off | login_page | `#/login` |  |  | `Please sign in User Password Remember Me Sign In Forgot Password` |
| off | viewer_menu | `#/mypreferencesmenu` |  |  | `viewer Profile Display Home Playback Subtitles Controls` |
| off | viewer_page | `#/quickconnect` |  |  | `Quick Connect To sign in with Quick Connect, select the 'Quick Connect' button on the device you are logging in from and enter the displayed code below.   Quick Connect code Authorize` |
| off | viewer_submit | `#/quickconnect` |  |  | `Quick Connect To sign in with Quick Connect, select the 'Quick Connect' button on the device you are logging in from and enter the displayed code below.   Unknown Quick Connect code Quick Connect code Authorize` |

| Initiate answered | Retry-After | Dialog | Still on the sign-in form |
|---|---|---|---|
| 401 |  | `Error Quick Connect is not active on this server Got It` | True |
| 403 |  | `Error Quick Connect is not active on this server Got It` | True |
| 403 | 60 | `Error Quick Connect is not active on this server Got It` | True |
| 429 |  | `Error Quick Connect is not active on this server Got It` | True |
| 429 | 60 | `Error Quick Connect is not active on this server Got It` | True |
| 500 |  | `Error Quick Connect is not active on this server Got It` | True |
| 503 |  | `Error Quick Connect is not active on this server Got It` | True |
| 503 | 60 | `Error Quick Connect is not active on this server Got It` | True |

| Quick Connect | Browser | Request | Query | Sends a token | Body | Status |
|---|---|---|---|---|---|---|
| on | tv | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| on | tv | `POST /QuickConnect/Initiate` |  | False |  | 200 |
| on | tv | `GET /QuickConnect/Connect` | `{"Secret": "<secret>"}` | False |  | 200 |
| on | viewer | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| on | viewer | `GET /QuickConnect/Enabled` |  | True |  | 200 |
| on | viewer | `POST /QuickConnect/Authorize` | `{"code": "000000"}` | True |  | 404 |
| on | viewer | `POST /QuickConnect/Authorize` | `{"code": "447983"}` | True |  | 200 |
| on | tv | `POST /Users/AuthenticateWithQuickConnect` |  | False | `{"Secret":"<secret>"}` | 200 |
| on | tv | `GET /QuickConnect/Enabled` |  | True |  | 200 |
| on | tv2 | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| on | tv2 | `POST /QuickConnect/Initiate` |  | False |  | 200 |
| on | admin | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| on | admin | `GET /QuickConnect/Enabled` |  | True |  | 200 |
| on | tv2 | `GET /QuickConnect/Connect` | `{"Secret": "<secret>"}` | False |  | 200 |
| on | admin | `POST /QuickConnect/Authorize` | `{"code": "974279", "userId": "942f84977a0d406d811f1e5b5e8e7960"}` | True |  | 200 |
| on | tv2 | `POST /Users/AuthenticateWithQuickConnect` |  | False | `{"Secret":"<secret>"}` | 200 |
| on | tv2 | `GET /QuickConnect/Enabled` |  | True |  | 200 |
| on | refused-401 | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| on | refused-401 | `POST /QuickConnect/Initiate` |  | False |  | 401 |
| on | refused-403 | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| on | refused-403 | `POST /QuickConnect/Initiate` |  | False |  | 403 |
| on | refused-429 | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| on | refused-429 | `POST /QuickConnect/Initiate` |  | False |  | 429 |
| on | refused-500 | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| on | refused-500 | `POST /QuickConnect/Initiate` |  | False |  | 500 |
| on | refused-503 | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| on | refused-503 | `POST /QuickConnect/Initiate` |  | False |  | 503 |
| off | off-tv | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| off | viewer | `GET /QuickConnect/Enabled` |  | False |  | 200 |
| off | viewer | `GET /QuickConnect/Enabled` |  | True |  | 200 |
| off | viewer | `POST /QuickConnect/Authorize` | `{"code": "123456"}` | True |  | 401 |

## Expiry rows

24 calls, at seconds after the batch's Initiate.

| At | Code | Call | Status | Authenticated | DateAdded moved by | Body |
|---|---|---|---|---|---|---|
| 1 s | early-a | approve | 200 |  |  | `true` |
| 1 s | early-b | approve | 200 |  |  | `true` |
| 1 s | early-c | approve | 200 |  |  | `true` |
| 1 s | early-d | approve | 200 |  |  | `true` |
| 2 s | early-a | connect | 200 | True | 61 s |  |
| 2 s | pending | connect | 200 | False | 0 s |  |
| 540 s | late-a | approve | 200 |  |  | `true` |
| 540 s | late-b | approve | 200 |  |  | `true` |
| 541 s | late-a | connect | 200 | True | 600 s |  |
| 590 s | pending | connect | 200 | False | 0 s |  |
| 590 s | early-a | redeem | 200 |  |  |  |
| 612 s | pending | connect | 404 |  |  | `"Unknown secret"` |
| 612 s | early-b | redeem | 404 |  |  | `Error processing request.` |
| 613 s | pending | approve | 404 |  |  | `Error processing request.` |
| 613 s | early-b | connect | 200 | True | 61 s |  |
| 614 s | pending | redeem | 404 |  |  | `Error processing request.` |
| 615 s | early-a | redeem | 404 |  |  | `Error processing request.` |
| 650 s | early-c | connect | 200 | True | 61 s |  |
| 675 s | early-d | connect | 404 |  |  | `"Unknown secret"` |
| 676 s | early-d | redeem | 404 |  |  | `Error processing request.` |
| 1130 s | late-a | redeem | 200 |  |  |  |
| 1152 s | late-b | redeem | 404 |  |  | `Error processing request.` |
| 1153 s | late-b | connect | 200 | True | 600 s |  |
| 1215 s | late-b | connect | 404 |  |  | `"Unknown secret"` |
