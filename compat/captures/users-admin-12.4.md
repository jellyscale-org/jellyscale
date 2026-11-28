# How Jellyfin 12.4 manages users (JS-033)

Observed with `jellyfin/jellyfin:12.4` (server 12.4.0), a disposable container on a Docker-assigned port with synthetic accounts, by a real Jellyfin 12.4 (API) and the browser part of the capture (the container's jellyfin-web in Chromium). Tokens and passwords are placeholders, ids are named after whom they stand for. The JSON holds the same rows in full.

## What stands out

- `GET /Users` answers a viewer the same full list as an administrator, and `isHidden`/`isDisabled` filter it for both.
- A Policy body needs only `AuthenticationProviderId` and `PasswordResetProviderId` (400 without either). Every other missing field takes
  the class's default, not the stored value: a missing `IsAdministrator` demotes (403 for the last administrator), a missing `IsDisabled` enables.
- `LoginAttemptsBeforeLockout` and `InvalidLoginAttemptCount` are the Policy's lockout fields. -1 never locks; 0 is stored as 3 for a
  viewer. At a limit of 3 the third wrong password disables the account (the counter goes from 2 to 4), an administrator's included.
  Enabling does not reset the counter, so one more wrong password disables again; a successful sign-in resets it.
- Disabling ends the user's tokens for good: they stay 401 after re-enabling, whether or not they were presented meanwhile.
- `DELETE` of the last administrator by themselves is 400, but has already revoked their token.
- A password change keeps the caller's token and ends the user's others; an administrator changing another's ends all of that user's.
  `ResetPassword` needs no `CurrentPw`, even for one's own, and ends no token. `HasPassword` stays true with an empty password.
- jellyfin-web's password pages call `POST /Users/{userId}/Password`, which is not in the OpenAPI document: in Jellyfin 12.4's source
  (`Jellyfin.Api/Controllers/UserController.cs`, `UpdateUserPasswordLegacy`) it is `[Obsolete]` and hidden from the document.
- Save with no change posts GET's own values on the profile and parental-control pages. The access page differs in two: it posts
  `BlockedMediaFolders` and `BlockedChannels` as `null` where GET answers `[]`.
- The new-user page posts only `Name` and `Password` to `/Users/New`, then a full Policy. With its boxes left alone that Policy changes
  `EnableAllFolders` and `EnableAllChannels` from true to false; ticking a library puts its id in `EnabledFolders`.
- A refused Policy Save (400 or 403) on the profile, access or parental-control page shows no message and leaves the spinner. So does a
  refused Reset Password. A refused delete, user list and Quick Connect authorize show nothing. A refused password change always says the
  user name or password is invalid. A refused Policy after New leaves the user made, with a generic error and the spinner.


## API: list

- admin: `GET /Users` → **200** `[{"Name":"capture-admin","HasPassword":true,"IsAdministrator":true,"IsDisabled":false,"IsHidden":true},{"Name":"capture-off","HasPassword":true,"IsAdministrator":false,"IsDisabled":true,"IsHidden":true},{"Name":"capture-shown","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":false},{"Name":"capture-viewer-one","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true},{"Name":"capture-viewer-two","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}]`
- admin: `GET /Users?isHidden=true` → **200** `[{"Name":"capture-admin","HasPassword":true,"IsAdministrator":true,"IsDisabled":false,"IsHidden":true},{"Name":"capture-off","HasPassword":true,"IsAdministrator":false,"IsDisabled":true,"IsHidden":true},{"Name":"capture-viewer-one","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true},{"Name":"capture-viewer-two","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}]`
- admin: `GET /Users?isHidden=false` → **200** `[{"Name":"capture-shown","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":false}]`
- admin: `GET /Users?isDisabled=true` → **200** `[{"Name":"capture-off","HasPassword":true,"IsAdministrator":false,"IsDisabled":true,"IsHidden":true}]`
- admin: `GET /Users?isDisabled=false` → **200** `[{"Name":"capture-admin","HasPassword":true,"IsAdministrator":true,"IsDisabled":false,"IsHidden":true},{"Name":"capture-shown","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":false},{"Name":"capture-viewer-one","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true},{"Name":"capture-viewer-two","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}]`
- admin: `GET /Users?isHidden=false&isDisabled=false` → **200** `[{"Name":"capture-shown","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":false}]`
- viewer: `GET /Users` → **200** `[{"Name":"capture-admin","HasPassword":true,"IsAdministrator":true,"IsDisabled":false,"IsHidden":true},{"Name":"capture-off","HasPassword":true,"IsAdministrator":false,"IsDisabled":true,"IsHidden":true},{"Name":"capture-shown","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":false},{"Name":"capture-viewer-one","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true},{"Name":"capture-viewer-two","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}]`
- viewer: `GET /Users?isHidden=true` → **200** `[{"Name":"capture-admin","HasPassword":true,"IsAdministrator":true,"IsDisabled":false,"IsHidden":true},{"Name":"capture-off","HasPassword":true,"IsAdministrator":false,"IsDisabled":true,"IsHidden":true},{"Name":"capture-viewer-one","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true},{"Name":"capture-viewer-two","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}]`
- viewer: `GET /Users?isHidden=false` → **200** `[{"Name":"capture-shown","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":false}]`
- viewer: `GET /Users?isDisabled=true` → **200** `[{"Name":"capture-off","HasPassword":true,"IsAdministrator":false,"IsDisabled":true,"IsHidden":true}]`
- viewer: `GET /Users?isDisabled=false` → **200** `[{"Name":"capture-admin","HasPassword":true,"IsAdministrator":true,"IsDisabled":false,"IsHidden":true},{"Name":"capture-shown","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":false},{"Name":"capture-viewer-one","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true},{"Name":"capture-viewer-two","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}]`
- viewer: `GET /Users?isHidden=false&isDisabled=false` → **200** `[{"Name":"capture-shown","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":false}]`
- no token: `GET /Users` → **401**
- no token: `GET /Users?isHidden=true` → **401**
- no token: `GET /Users?isHidden=false` → **401**
- no token: `GET /Users?isDisabled=true` → **401**
- no token: `GET /Users?isDisabled=false` → **401**
- no token: `GET /Users?isHidden=false&isDisabled=false` → **401**
- no token: `GET /Users/Public` → **200** `[{"Name":"capture-shown","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":false}]`

## API: new

- viewer: `POST /Users/New` `{"Name":"capture-by-viewer","Password":"<right-password>"}` → **403**
- no token: `POST /Users/New` `{"Name":"capture-anon","Password":"<right-password>"}` → **401**
- admin, plain: `POST /Users/New` `{"Name":"capture-made","Password":"<right-password>"}` → **200** `{"Name":"capture-made","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}`
- admin, same name: `POST /Users/New` `{"Name":"capture-made","Password":"<right-password>"}` → **400** `"Error processing request."`
- admin, name taken in another case: `POST /Users/New` `{"Name":"CAPTURE-MADE","Password":"<right-password>"}` → **400** `"Error processing request."`
- viewer, name taken in another case: `POST /Users/New` `{"Name":"CAPTURE-MADE","Password":"<right-password>"}` → **403**
- admin, empty name: `POST /Users/New` `{"Name":"","Password":"<right-password>"}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"Name\":[\"The Name field is required.\"]},\"traceId\":\"00-<id>-dd8723d1fa3819c8-00\"}"`
- viewer, empty name: `POST /Users/New` `{"Name":"","Password":"<right-password>"}` → **403**
- admin, blank name: `POST /Users/New` `{"Name":"   ","Password":"<right-password>"}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"Name\":[\"The Name field is required.\"]},\"traceId\":\"00-<id>-cbe40002b310fa63-00\"}"`
- admin, no Name: `POST /Users/New` `{"Password":"<right-password>"}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"$\":[\"JSON deserialization for type \\u0027Jellyfin.Api.Models.UserDtos.CreateUserByName\\u0027 was missing required properties including: \\u0027Name\\u0027.\"],\"request\""`
- admin, name with trailing space: `POST /Users/New` `{"Name":"capture-trail ","Password":"<right-password>"}` → **400** `"Error processing request."`
- admin, name with a slash: `POST /Users/New` `{"Name":"capture/slash","Password":"<right-password>"}` → **400** `"Error processing request."`
- admin, no Password field: `POST /Users/New` `{"Name":"capture-nopass"}` → **200** `{"Name":"capture-nopass","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}`
- admin, empty Password: `POST /Users/New` `{"Name":"capture-emptypass","Password":""}` → **200** `{"Name":"capture-emptypass","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}`
- viewer, no Password field: `POST /Users/New` `{"Name":"capture-viewer-nopass"}` → **403**
- admin, password 'password': `POST /Users/New` `{"Name":"capture-common","Password":"password"}` → **200** `{"Name":"capture-common","HasPassword":true,"IsAdministrator":false,"IsDisabled":false,"IsHidden":true}`
- the whole Policy of a new user `{"IsAdministrator":false,"IsHidden":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"IsDisabled":false,"BlockedTags":[],"AllowedTags":[],"EnableUserPreferenceAccess":true,"AccessSchedules":[],"BlockUnratedItems":[],"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":true,"EnableRemoteAccess":true,"EnableLiveTvManagement":false,"EnableLiveTvAccess":true,"EnableMediaPlayback":true,"EnableAudioPlaybackTranscoding":true,"EnableVideoPlaybackTranscoding":true,"EnablePlaybackRemuxing":true,"ForceRemoteSourceTranscoding":false,"Ena…`
- the fields of a new user's answer `["Configuration","EnableAutoLogin","HasConfiguredEasyPassword","HasConfiguredPassword","HasPassword","Id","Name","Policy","ServerId"]`
- user made without Password signs in with empty Pw: `POST /Users/AuthenticateByName` → **200** `"token issued"`
- user made without Password signs in with some Pw: `POST /Users/AuthenticateByName` → **401** `"Error processing request."`

## API: update

- viewer renames themselves, full user DTO: `POST /Users?userId=<capture-rename-id>` `{"(full user DTO)":true,"Name":"capture-renamed","Policy.IsAdministrator":false}` → **204**
- viewer renames themselves, full user DTO: what changed `{"Name":["capture-rename","capture-renamed"],"Policy":{},"Configuration":{}}`
- viewer renames another: `POST /Users?userId=<capture-viewer-two-id>` `{"(full user DTO)":true,"Name":"capture-stolen","Policy.IsAdministrator":false}` → **403** `"\"User update not allowed.\""`
- viewer renames another: what changed `{"Name":["capture-viewer-two","capture-viewer-two"],"Policy":{},"Configuration":{}}`
- admin renames another: `POST /Users?userId=<capture-rename-id>` `{"(full user DTO)":true,"Name":"capture-by-admin","Policy.IsAdministrator":false}` → **204**
- admin renames another: what changed `{"Name":["capture-renamed","capture-by-admin"],"Policy":{},"Configuration":{}}`
- rename to a taken name in another case: `POST /Users?userId=<capture-rename-id>` `{"(full user DTO)":true,"Name":"CAPTURE-VIEWER-ONE","Policy.IsAdministrator":false}` → **400** `"Error processing request."`
- rename to a taken name in another case: what changed `{"Name":["capture-by-admin","capture-by-admin"],"Policy":{},"Configuration":{}}`
- rename to own name in another case: `POST /Users?userId=<capture-rename-id>` `{"(full user DTO)":true,"Name":"CAPTURE-BY-ADMIN","Policy.IsAdministrator":false}` → **204**
- rename to own name in another case: what changed `{"Name":["capture-by-admin","CAPTURE-BY-ADMIN"],"Policy":{},"Configuration":{}}`
- rename to empty: `POST /Users?userId=<capture-rename-id>` `{"(full user DTO)":true,"Name":"","Policy.IsAdministrator":false}` → **400** `"Error processing request."`
- rename to empty: what changed `{"Name":["CAPTURE-BY-ADMIN","CAPTURE-BY-ADMIN"],"Policy":{},"Configuration":{}}`
- body is only Name: `POST /Users?userId=<capture-rename-id>` `{"Name":"capture-only-name"}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"Policy.PasswordResetProviderId\":[\"The PasswordResetProviderId field is required.\"],\"Policy.AuthenticationProviderId\":[\"The AuthenticationProviderId field is required"`
- body is only Name: what changed `{"Name":["CAPTURE-BY-ADMIN","CAPTURE-BY-ADMIN"],"Policy":{},"Configuration":{}}`
- own DTO that also asks IsAdministrator: `POST /Users?userId=<capture-rename-id>` `{"(full user DTO)":true,"Name":"CAPTURE-BY-ADMIN","Policy.IsAdministrator":true}` → **204**
- own DTO that also asks IsAdministrator: what changed `{"Name":["CAPTURE-BY-ADMIN","CAPTURE-BY-ADMIN"],"Policy":{},"Configuration":{}}`
- unknown id, full user DTO: `POST /Users?userId=0f0f0f0f0f0f40408f8f0f0f0f0f0f0f` `{"(full user DTO)":true,"Name":"capture-nobody","Policy.IsAdministrator":false}` → **404** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.5\",\"title\":\"Not Found\",\"status\":404,\"traceId\":\"00-<id>-d14d1642f9678998-00\"}"`
- malformed id, full user DTO: `POST /Users?userId=not-a-guid` `{"(full user DTO)":true,"Name":"capture-malformed","Policy.IsAdministrator":false}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"userId\":[\"The value \\u0027not-a-guid\\u0027 is not valid.\"]},\"traceId\":\"00-<id>-e46f52d9c999c2ab-00\"}"`
- no userId: `POST /Users` `{"(full user DTO)":true,"Name":"capture-no-query","Policy.IsAdministrator":false}` → **204**
- no userId: the caller's name afterwards `"capture-no-query"`
- GET /Users/Me with the renamed user's token `{"own":200}`

## API: delete

- only admin deletes themselves (the last admin): `DELETE /Users/<capture-admin-id>` → **400** `"Error processing request."`
- the only admin's token after that refusal `{"admin":401}`
- the administrator after signing in again `{"Name":"capture-admin","HasPassword":true,"IsAdministrator":true,"IsDisabled":false,"IsHidden":true}`
- viewer deletes an administrator: `DELETE /Users/<capture-admin-two-id>` → **403**
- one of two administrators deletes themselves: `DELETE /Users/<capture-admin-two-id>` → **204**
- their token afterwards `{"deleted":401}`
- disable the second administrator through Policy: `POST /Users/<capture-spare-admin-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":true,"IsDisabled":true,"LoginAttemptsBeforeLockout":-1}` → **403** `"\"Administrators cannot be disabled.\""`
- admin deletes a viewer: `DELETE /Users/<capture-gone-id>` → **204**
- deleted user's token: `GET /Users/Me` → **401**
- deleted user's token: `GET /Items` → **401**
- deleted user's token: `GET /System/Info` → **401**
- deleted user's token: `GET /Users/Public` → **200** `[]`
- made-up token: `GET /Users/Me` → **401**
- made-up token: `GET /Items` → **401**
- made-up token: `GET /System/Info` → **401**
- made-up token: `GET /Users/Public` → **200** `[]`
- deleted name, its old password: `POST /Users/AuthenticateByName` → **401** `"Error processing request."`
- never-existing name: `POST /Users/AuthenticateByName` → **401** `"Error processing request."`
- existing name, wrong password: `POST /Users/AuthenticateByName` → **401** `"Error processing request."`
- GET deleted id: `GET /Users/<capture-gone-id>` → **404** `"\"User not found\""`
- admin, deleted id: `DELETE /Users/<capture-gone-id>` → **404** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.5\",\"title\":\"Not Found\",\"status\":404,\"traceId\":\"00-<id>-0dc747db4b105ab2-00\"}"`
- viewer, deleted id: `DELETE /Users/<capture-gone-id>` → **403**
- GET unknown id: `GET /Users/0f0f0f0f0f0f40408f8f0f0f0f0f0f0f` → **404** `"\"User not found\""`
- admin, unknown id: `DELETE /Users/0f0f0f0f0f0f40408f8f0f0f0f0f0f0f` → **404** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.5\",\"title\":\"Not Found\",\"status\":404,\"traceId\":\"00-<id>-b1a461feb3aa1501-00\"}"`
- viewer, unknown id: `DELETE /Users/0f0f0f0f0f0f40408f8f0f0f0f0f0f0f` → **403**
- GET malformed id: `GET /Users/not-a-guid` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"userId\":[\"The value \\u0027not-a-guid\\u0027 is not valid.\"]},\"traceId\":\"00-<id>-578e0ee7ba0ca434-00\"}"`
- admin, malformed id: `DELETE /Users/not-a-guid` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"userId\":[\"The value \\u0027not-a-guid\\u0027 is not valid.\"]},\"traceId\":\"00-<id>-0253990a19022bce-00\"}"`
- viewer, malformed id: `DELETE /Users/not-a-guid` → **403**
- GET all-zero id: `GET /Users/00000000000000000000000000000000` → **400** `"Error processing request."`
- admin, all-zero id: `DELETE /Users/00000000000000000000000000000000` → **400** `"Error processing request."`
- viewer, all-zero id: `DELETE /Users/00000000000000000000000000000000` → **403**
- old token after the name is made again `{"deleted":401}`

## API: policy

- the administrator's whole Policy `{"IsAdministrator":true,"IsHidden":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"IsDisabled":false,"BlockedTags":[],"AllowedTags":[],"EnableUserPreferenceAccess":true,"AccessSchedules":[],"BlockUnratedItems":[],"EnableRemoteControlOfOtherUsers":true,"EnableSharedDeviceControl":true,"EnableRemoteAccess":true,"EnableLiveTvManagement":false,"EnableLiveTvAccess":true,"EnableMediaPlayback":true,"EnableAudioPlaybackTranscoding":true,"EnableVideoPlaybackTranscoding":true,"EnablePlaybackRemuxing":true,"ForceRemoteSourceTranscoding":false,"Enabl…`
- a viewer's whole Policy `{"IsAdministrator":false,"IsHidden":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"IsDisabled":false,"BlockedTags":[],"AllowedTags":[],"EnableUserPreferenceAccess":true,"AccessSchedules":[],"BlockUnratedItems":[],"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":true,"EnableRemoteAccess":true,"EnableLiveTvManagement":false,"EnableLiveTvAccess":true,"EnableMediaPlayback":true,"EnableAudioPlaybackTranscoding":true,"EnableVideoPlaybackTranscoding":true,"EnablePlaybackRemuxing":true,"ForceRemoteSourceTranscoding":false,"Ena…`
- GET's own Policy posted back: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- GET's own Policy posted back: Policy changes [before, after] `{}`
- empty object: `POST /Users/<capture-policy-id>/Policy` `{}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"PasswordResetProviderId\":[\"The PasswordResetProviderId field is required.\"],\"AuthenticationProviderId\":[\"The AuthenticationProviderId field is required.\"]},\"traceId\""`
- empty object: Policy changes [before, after] `{}`
- only IsDisabled=false: `POST /Users/<capture-policy-id>/Policy` `{"IsDisabled":false}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"PasswordResetProviderId\":[\"The PasswordResetProviderId field is required.\"],\"AuthenticationProviderId\":[\"The AuthenticationProviderId field is required.\"]},\"traceId\""`
- only IsDisabled=false: Policy changes [before, after] `{}`
- only the two provider ids: `POST /Users/<capture-policy-id>/Policy` `{"AuthenticationProviderId":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider","PasswordResetProviderId":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider"}` → **204**
- only the two provider ids: Policy changes [before, after] `{"EnableAllFolders":[false,true],"EnableContentDownloading":[false,true],"EnableLiveTvManagement":[false,true],"IsHidden":[false,true],"LoginAttemptsBeforeLockout":[3,-1]}`
- full body without AuthenticationProviderId: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"AuthenticationProviderId\":[\"The AuthenticationProviderId field is required.\"]},\"traceId\":\"00-<id>-b902e3c47e50e39e-00\"}"`
- full body without AuthenticationProviderId: Policy changes [before, after] `{}`
- full body without PasswordResetProviderId: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"PasswordResetProviderId\":[\"The PasswordResetProviderId field is required.\"]},\"traceId\":\"00-<id>-d85b6b3f5962cfc5-00\"}"`
- full body without PasswordResetProviderId: Policy changes [before, after] `{}`
- full body without IsAdministrator (stored true): `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- full body without IsAdministrator (stored true): Policy changes [before, after] `{"IsAdministrator":[true,false]}`
- full body without IsDisabled (stored true): `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsAdministrator":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- full body without IsDisabled (stored true): Policy changes [before, after] `{"IsDisabled":[true,false]}`
- full body without IsHidden (stored false): `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- full body without IsHidden (stored false): Policy changes [before, after] `{"IsHidden":[false,true]}`
- full body without EnableContentDownloading (stored false): `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- full body without EnableContentDownloading (stored false): Policy changes [before, after] `{"EnableContentDownloading":[false,true]}`
- full body without LoginAttemptsBeforeLockout (stored 3): `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsAdministrator":false,"IsDisabled":false}` → **204**
- full body without LoginAttemptsBeforeLockout (stored 3): Policy changes [before, after] `{"LoginAttemptsBeforeLockout":[3,-1]}`
- full body without InvalidLoginAttemptCount (stored 2): `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- full body without InvalidLoginAttemptCount (stored 2): Policy changes [before, after] `{"InvalidLoginAttemptCount":[2,0]}`
- full body without EnableAllFolders (stored false): `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- full body without EnableAllFolders (stored false): Policy changes [before, after] `{"EnableAllFolders":[false,true]}`
- full body without EnableMediaPlayback (stored false): `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":41,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- full body without EnableMediaPlayback (stored false): Policy changes [before, after] `{"EnableMediaPlayback":[false,true]}`
- full body with IsAdministrator=null: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":null,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"newPolicy\":[\"The newPolicy field is required.\"],\"$.IsAdministrator\":[\"The JSON value could not be converted to System.Boolean. Path: $.IsAdministrator \| LineNumber: "`
- full body with IsAdministrator=null: Policy changes [before, after] `{}`
- full body with IsDisabled=null: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":null,"LoginAttemptsBeforeLockout":-1}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"newPolicy\":[\"The newPolicy field is required.\"],\"$.IsDisabled\":[\"The JSON value could not be converted to System.Boolean. Path: $.IsDisabled \| LineNumber: 0 \| BytePo"`
- full body with IsDisabled=null: Policy changes [before, after] `{}`
- full body with LoginAttemptsBeforeLockout=null: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":null}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"newPolicy\":[\"The newPolicy field is required.\"],\"$.LoginAttemptsBeforeLockout\":[\"The JSON value could not be converted to System.Int32. Path: $.LoginAttemptsBeforeLo"`
- full body with LoginAttemptsBeforeLockout=null: Policy changes [before, after] `{}`
- LoginAttemptsBeforeLockout=-5: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-5}` → **204**
- LoginAttemptsBeforeLockout=-5: Policy changes [before, after] `{"LoginAttemptsBeforeLockout":[-1,-5]}`
- LoginAttemptsBeforeLockout=-1: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- LoginAttemptsBeforeLockout=-1: Policy changes [before, after] `{"LoginAttemptsBeforeLockout":[-5,-1]}`
- LoginAttemptsBeforeLockout=0: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":0}` → **204**
- LoginAttemptsBeforeLockout=0: Policy changes [before, after] `{"LoginAttemptsBeforeLockout":[-1,3]}`
- LoginAttemptsBeforeLockout=1: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":1}` → **204**
- LoginAttemptsBeforeLockout=1: Policy changes [before, after] `{"LoginAttemptsBeforeLockout":[3,1]}`
- LoginAttemptsBeforeLockout=5: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":5}` → **204**
- LoginAttemptsBeforeLockout=5: Policy changes [before, after] `{"LoginAttemptsBeforeLockout":[1,5]}`
- LoginAttemptsBeforeLockout=1000: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":1000}` → **204**
- LoginAttemptsBeforeLockout=1000: Policy changes [before, after] `{"LoginAttemptsBeforeLockout":[5,1000]}`
- unknown field: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":43,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":1000}` → **204**
- unknown field: Policy changes [before, after] `{}`
- viewer posts their own Policy: `POST /Users/<capture-viewer-one-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **403**
- viewer posts another's Policy: `POST /Users/<capture-policy-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":1000}` → **403**
- unknown id: `POST /Users/0f0f0f0f0f0f40408f8f0f0f0f0f0f0f/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":1000}` → **404** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.5\",\"title\":\"Not Found\",\"status\":404,\"traceId\":\"00-<id>-3980d1ff943f037b-00\"}"`
- malformed id: `POST /Users/not-a-guid/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":1000}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"userId\":[\"The value \\u0027not-a-guid\\u0027 is not valid.\"]},\"traceId\":\"00-<id>-633feab6015c588b-00\"}"`
- last administrator demotes themselves: `POST /Users/<capture-admin-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **403** `"\"There must be at least one user in the system with administrative access.\""`
- last administrator demotes themselves: Policy changes [before, after] `{}`
- last administrator posts a body without IsAdministrator: `POST /Users/<capture-admin-id>/Policy` `{"(full policy, fields)":41,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **403** `"\"There must be at least one user in the system with administrative access.\""`
- last administrator posts a body without IsAdministrator: Policy changes [before, after] `{}`
- last administrator posts only the provider ids: `POST /Users/<capture-admin-id>/Policy` `{"AuthenticationProviderId":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider","PasswordResetProviderId":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider"}` → **403** `"\"There must be at least one user in the system with administrative access.\""`
- last administrator posts only the provider ids: Policy changes [before, after] `{}`
- administrator disables themselves: `POST /Users/<capture-admin-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":true,"IsDisabled":true,"LoginAttemptsBeforeLockout":-1}` → **403** `"\"Administrators cannot be disabled.\""`
- administrator disables themselves: Policy changes [before, after] `{}`
- one of two administrators disables themselves: `POST /Users/<capture-admin-two-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":true,"IsDisabled":true,"LoginAttemptsBeforeLockout":-1}` → **403** `"\"Administrators cannot be disabled.\""`
- one of two administrators disables themselves: Policy changes [before, after] `{}`
- one of two administrators demotes and disables themselves at once: `POST /Users/<capture-admin-two-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":true,"LoginAttemptsBeforeLockout":-1}` → **403** `"\"Administrators cannot be disabled.\""`
- one of two administrators demotes and disables themselves at once: Policy changes [before, after] `{}`
- one of two administrators demotes themselves: `POST /Users/<capture-admin-two-id>/Policy` `{"(full policy, fields)":42,"IsAdministrator":false,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}` → **204**
- one of two administrators demotes themselves: Policy changes [before, after] `{"IsAdministrator":[true,false]}`
- the demoted token on an administrator route: `GET /Auth/Providers` → **403**

## API: lockout

- limit 3: wrong passwords `[{"wrong":1,"status":401,"InvalidLoginAttemptCount":1,"IsDisabled":false},{"wrong":2,"status":401,"InvalidLoginAttemptCount":2,"IsDisabled":false},{"wrong":3,"status":401,"InvalidLoginAttemptCount":4,"IsDisabled":true},{"wrong":4,"status":403,"InvalidLoginAttemptCount":4,"IsDisabled":true},{"wrong":5,"status":403,"InvalidLoginAttemptCount":4,"IsDisabled":true},{"wrong":6,"status":403,"InvalidLoginAttemptCount":4,"IsDisabled":true}]`
- limit 3: right password afterwards: `POST /Users/AuthenticateByName` → **403** `"Error processing request."`
- limit 3: counter after that `{"InvalidLoginAttemptCount":4,"IsDisabled":true,"LoginAttemptsBeforeLockout":3}`
- limit 3: the token held from before `{"held":401}`
- limit 3: counter after an administrator enables `{"InvalidLoginAttemptCount":4,"IsDisabled":false}`
- limit 3: after enable, one more wrong password `{"InvalidLoginAttemptCount":6,"IsDisabled":true}`
- limit 3: right password after enable: `POST /Users/AuthenticateByName` → **200** `"token issued"`
- limit 3: counter after that sign-in `{"InvalidLoginAttemptCount":0,"IsDisabled":false}`
- limit -1: wrong passwords `[{"wrong":1,"status":401,"InvalidLoginAttemptCount":1,"IsDisabled":false},{"wrong":2,"status":401,"InvalidLoginAttemptCount":2,"IsDisabled":false},{"wrong":3,"status":401,"InvalidLoginAttemptCount":3,"IsDisabled":false},{"wrong":4,"status":401,"InvalidLoginAttemptCount":4,"IsDisabled":false},{"wrong":5,"status":401,"InvalidLoginAttemptCount":5,"IsDisabled":false},{"wrong":6,"status":401,"InvalidLoginAttemptCount":6,"IsDisabled":false}]`
- limit -1: right password afterwards: `POST /Users/AuthenticateByName` → **200** `"token issued"`
- limit -1: counter after that `{"InvalidLoginAttemptCount":0,"IsDisabled":false,"LoginAttemptsBeforeLockout":-1}`
- limit -1: the token held from before `{"held":200}`
- limit 0: wrong passwords `[{"wrong":1,"status":401,"InvalidLoginAttemptCount":1,"IsDisabled":false},{"wrong":2,"status":401,"InvalidLoginAttemptCount":2,"IsDisabled":false},{"wrong":3,"status":401,"InvalidLoginAttemptCount":4,"IsDisabled":true},{"wrong":4,"status":403,"InvalidLoginAttemptCount":4,"IsDisabled":true},{"wrong":5,"status":403,"InvalidLoginAttemptCount":4,"IsDisabled":true},{"wrong":6,"status":403,"InvalidLoginAttemptCount":4,"IsDisabled":true}]`
- limit 0: right password afterwards: `POST /Users/AuthenticateByName` → **403** `"Error processing request."`
- limit 0: counter after that `{"InvalidLoginAttemptCount":4,"IsDisabled":true,"LoginAttemptsBeforeLockout":3}`
- limit 0: the token held from before `{"held":401}`
- limit 0: counter after an administrator enables `{"InvalidLoginAttemptCount":4,"IsDisabled":false}`
- limit 0: after enable, one more wrong password `{"InvalidLoginAttemptCount":6,"IsDisabled":true}`
- limit 0: right password after enable: `POST /Users/AuthenticateByName` → **200** `"token issued"`
- limit 0: counter after that sign-in `{"InvalidLoginAttemptCount":0,"IsDisabled":false}`
- a second administrator, limit 3, four wrong passwords `{"statuses":[401,401,401,403],"IsAdministrator":true,"InvalidLoginAttemptCount":4,"IsDisabled":true}`

## API: disable

- tokens before `{"first":200,"second":200}`
- disabled, first token: `GET /Users/Me` → **401**
- disabled, first token: `GET /Items` → **401**
- disabled, second token: `GET /Users/Me` → **401**
- disabled, second token: `GET /Items` → **401**
- disabled, right password: `POST /Users/AuthenticateByName` → **403** `"Error processing request."`
- disabled, wrong password: `POST /Users/AuthenticateByName` → **403** `"Error processing request."`
- enabled user, wrong password: `POST /Users/AuthenticateByName` → **401** `"Error processing request."`
- unknown name: `POST /Users/AuthenticateByName` → **401** `"Error processing request."`
- counter while disabled `0`
- old tokens after re-enable `{"first":401,"second":401}`
- re-enabled, right password: `POST /Users/AuthenticateByName` → **200** `"token issued"`
- a token not presented while disabled, after re-enable `{"never used while disabled":401}`

## API: password

- own, wrong CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"wrong","NewPw":"<new-password>"}` → **403** `"\"Invalid user or password entered.\""`
- own, wrong CurrentPw: the user's tokens afterwards `{"caller":200,"other":200}`
- own, wrong CurrentPw: HasPassword `true`
- own, empty CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"","NewPw":"<new-password>"}` → **403** `"\"Invalid user or password entered.\""`
- own, empty CurrentPw: the user's tokens afterwards `{"caller":200,"other":200}`
- own, empty CurrentPw: HasPassword `true`
- own, no CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"NewPw":"<new-password>"}` → **403** `"\"Invalid user or password entered.\""`
- own, no CurrentPw: the user's tokens afterwards `{"caller":200,"other":200}`
- own, no CurrentPw: HasPassword `true`
- own, right CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"<right-password>","NewPw":"<new-password>"}` → **204**
- own, right CurrentPw: the user's tokens afterwards `{"caller":200,"other":401}`
- own, right CurrentPw: HasPassword `true`
- admin for another, no CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"NewPw":"<right-password>"}` → **204**
- admin for another, no CurrentPw: the user's tokens afterwards `{"caller":401,"other":401}`
- admin for another, no CurrentPw: HasPassword `true`
- admin for another, wrong CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"wrong","NewPw":"<new-password>"}` → **204**
- admin for another, wrong CurrentPw: the user's tokens afterwards `{"caller":401,"other":401}`
- admin for another, wrong CurrentPw: HasPassword `true`
- viewer for another: `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"<new-password>","NewPw":"<right-password>"}` → **403** `"\"User is not allowed to update the password.\""`
- viewer for another: the user's tokens afterwards `{"caller":200,"other":200}`
- viewer for another: HasPassword `true`
- own, right CurrentPw, new password 'password': `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"<new-password>","NewPw":"password"}` → **204**
- own, right CurrentPw, new password 'password': the user's tokens afterwards `{"caller":200,"other":401}`
- own, right CurrentPw, new password 'password': HasPassword `true`
- own, right CurrentPw, one letter: `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"password","NewPw":"a"}` → **204**
- own, right CurrentPw, one letter: the user's tokens afterwards `{"caller":200,"other":401}`
- own, right CurrentPw, one letter: HasPassword `true`
- admin sets it back: `POST /Users/Password?userId=<capture-pass-id>` `{"NewPw":"<right-password>"}` → **204**
- admin sets it back: the user's tokens afterwards `{"caller":401,"other":401}`
- admin sets it back: HasPassword `true`
- own, ResetPassword without CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"ResetPassword":true}` → **204**
- own, ResetPassword without CurrentPw: the user's tokens afterwards `{"caller":200,"other":200}`
- own, ResetPassword without CurrentPw: HasPassword `true`
- after own reset without CurrentPw, the old password: `POST /Users/AuthenticateByName` → **401** `"Error processing request."`
- admin sets a password again: `POST /Users/Password?userId=<capture-pass-id>` `{"NewPw":"<right-password>"}` → **204**
- admin sets a password again: the user's tokens afterwards `{"caller":401,"other":401}`
- admin sets a password again: HasPassword `true`
- own, ResetPassword with wrong CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"ResetPassword":true,"CurrentPw":"wrong"}` → **204**
- own, ResetPassword with wrong CurrentPw: the user's tokens afterwards `{"caller":200,"other":200}`
- own, ResetPassword with wrong CurrentPw: HasPassword `true`
- admin sets a password once more: `POST /Users/Password?userId=<capture-pass-id>` `{"NewPw":"<right-password>"}` → **204**
- admin sets a password once more: the user's tokens afterwards `{"caller":401,"other":401}`
- admin sets a password once more: HasPassword `true`
- own, ResetPassword with right CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"ResetPassword":true,"CurrentPw":"<right-password>"}` → **204**
- own, ResetPassword with right CurrentPw: the user's tokens afterwards `{"caller":200,"other":200}`
- own, ResetPassword with right CurrentPw: HasPassword `true`
- after reset, empty password: `POST /Users/AuthenticateByName` → **200** `"token issued"`
- after reset, the old password: `POST /Users/AuthenticateByName` → **401** `"Error processing request."`
- no password: own, wrong CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"wrong","NewPw":"<new-password>"}` → **403** `"\"Invalid user or password entered.\""`
- no password: own, wrong CurrentPw: the user's tokens afterwards `{"caller":200,"other":200}`
- no password: own, wrong CurrentPw: HasPassword `true`
- no password: own, no CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"NewPw":"<new-password>"}` → **204**
- no password: own, no CurrentPw: the user's tokens afterwards `{"caller":200,"other":401}`
- no password: own, no CurrentPw: HasPassword `true`
- admin ResetPassword for another: `POST /Users/Password?userId=<capture-pass-id>` `{"ResetPassword":true}` → **204**
- admin ResetPassword for another: the user's tokens afterwards `{"caller":200,"other":200}`
- admin ResetPassword for another: HasPassword `true`
- no password: own, empty CurrentPw: `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"","NewPw":"<new-password>"}` → **204**
- no password: own, empty CurrentPw: the user's tokens afterwards `{"caller":200,"other":401}`
- no password: own, empty CurrentPw: HasPassword `true`
- own, right CurrentPw, empty NewPw: `POST /Users/Password?userId=<capture-pass-id>` `{"CurrentPw":"<new-password>","NewPw":""}` → **204**
- own, right CurrentPw, empty NewPw: the user's tokens afterwards `{"caller":200,"other":401}`
- own, right CurrentPw, empty NewPw: HasPassword `true`
- admin, empty body: `POST /Users/Password?userId=<capture-pass-id>` `{}` → **204**
- admin, empty body: the user's tokens afterwards `{"caller":401,"other":401}`
- admin, empty body: HasPassword `true`
- after the admin's empty body, empty password: `POST /Users/AuthenticateByName` → **200** `"token issued"`
- unknown id: `POST /Users/Password?userId=0f0f0f0f0f0f40408f8f0f0f0f0f0f0f` `{"NewPw":"<new-password>"}` → **404** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.5\",\"title\":\"Not Found\",\"status\":404,\"traceId\":\"00-<id>-6833615715bede52-00\"}"`
- unknown id: the user's tokens afterwards `{"caller":200,"other":200}`
- unknown id: HasPassword `true`
- malformed id: `POST /Users/Password?userId=not-a-guid` `{"NewPw":"<new-password>"}` → **400** `"{\"type\":\"https://tools.ietf.org/html/rfc9110#section-15.5.1\",\"title\":\"One or more validation errors occurred.\",\"status\":400,\"errors\":{\"userId\":[\"The value \\u0027not-a-guid\\u0027 is not valid.\"]},\"traceId\":\"00-<id>-cb0aaf9d21403963-00\"}"`
- malformed id: the user's tokens afterwards `{"caller":200,"other":200}`
- malformed id: HasPassword `true`
- admin for themselves, no CurrentPw: `POST /Users/Password?userId=<capture-admin-two-id>` `{"NewPw":"<new-password>"}` → **403** `"\"Invalid user or password entered.\""`
- admin for themselves, wrong CurrentPw: `POST /Users/Password?userId=<capture-admin-two-id>` `{"CurrentPw":"wrong","NewPw":"<new-password>"}` → **403** `"\"Invalid user or password entered.\""`
- no userId, right CurrentPw: `POST /Users/Password` `{"CurrentPw":"<right-password>","NewPw":"<new-password>"}` → **204**

## API: providers

- admin: `GET /Auth/Providers` → **200** `[{"Name":"Default","Id":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider"}]`
- admin: `GET /Auth/PasswordResetProviders` → **200** `[{"Name":"Default Password Reset Provider","Id":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider"}]`
- viewer: `GET /Auth/Providers` → **403**
- viewer: `GET /Auth/PasswordResetProviders` → **403**
- no token: `GET /Auth/Providers` → **401**
- no token: `GET /Auth/PasswordResetProviders` → **401**

## jellyfin-web: what each page sends

Each load builds the app again, so it holds the app's own start-up requests too. `/System/Info*`, `/System/Endpoint`, `/Playback/BitrateTest` and `/Sessions/Capabilities/Full` are left out here and kept in the JSON. A request is listed once. Each page and each refusal has its own browser context: the app caches answers, so a later load in one context can show a page without asking again.


### user list (administrator)

- load:
  - `GET /Users/<capture-admin-id>` → 200
  - `GET /Users` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users"}`
- a card's menu:
  - shows: `{"messages": ["Edit user Library access Parental control Delete"], "still_shown_at_the_end": ["Edit user Library access Parental control Delete"], "spinner_at_the_end": false, "menu": ["Edit user", "Library access", "Parental control", "Delete"], "hash": "#/dashboard/users"}`

### new user (administrator)

- load:
  - `GET /Users/<capture-admin-id>` → 200
  - `GET /Library/MediaFolders` → 200
  - `GET /Channels` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users/add"}`
- Save with a name and a password, defaults left:
  - `POST /Users/New` → 200 body `{"Name":"capture-web-made","Password":"<new-password>"}`
  - `POST /Users/<capture-web-made-id>/Policy` → 204 body `{"IsAdministrator":false,"IsHidden":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"IsDisabled":false,"BlockedTags":[],"AllowedTags":[],"EnableUserPreferenceAccess":true,"AccessSchedules":[],"BlockUnratedItems":[],"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":true,"EnableRemoteAccess":true,"EnableLiveTvManagement":false,"EnableLiveTvAccess":true,"EnableMediaPlayback":true,"EnableAudioPlaybackTranscoding":true,"EnableVideoPlaybackTranscoding":true,"EnablePlaybackRemuxing":true,"ForceRemoteSourceTranscoding":false,"EnableContentDeletion":false,"EnableContentDeletionFromFolders":[],"EnableContentDownloading":true,"EnableSyncTranscoding":true,"EnableMediaConversion":true,"EnabledDevices":[],"EnableAllDevices":true,"EnabledChannels":[],"EnableAllChannels":false,"EnabledFolders":[],"EnableAllFolders":false,"InvalidLoginAttemptCount":0,"LoginAttemptsBeforeLockout":-1,"MaxActiveSessions":0,"EnablePublicSharing":true,"BlockedMediaFolders":[],"BlockedChannels":[],"RemoteClientBitrateLimit":0,"AuthenticationProviderId":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider","PasswordResetProviderId":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider","SyncPlayAccess":"CreateAndJoinGroups"}`
  - `GET /Users/<capture-web-made-id>` → 200
  - `GET /Auth/PasswordResetProviders` → 200
  - `GET /Auth/Providers` → 200
  - `GET /System/Configuration/network` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users/<capture-web-made-id>/profile"}`
- load again:
  - `GET /Users/<capture-admin-id>` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users/add"}`
- Save with a name, no password, one library ticked instead of all:
  - `POST /Users/New` → 200 body `{"Name":"capture-web-restricted","Password":""}`
  - `POST /Users/<id>/Policy` → 204 body `{"IsAdministrator":false,"IsHidden":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"IsDisabled":false,"BlockedTags":[],"AllowedTags":[],"EnableUserPreferenceAccess":true,"AccessSchedules":[],"BlockUnratedItems":[],"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":true,"EnableRemoteAccess":true,"EnableLiveTvManagement":false,"EnableLiveTvAccess":true,"EnableMediaPlayback":true,"EnableAudioPlaybackTranscoding":true,"EnableVideoPlaybackTranscoding":true,"EnablePlaybackRemuxing":true,"ForceRemoteSourceTranscoding":false,"EnableContentDeletion":false,"EnableContentDeletionFromFolders":[],"EnableContentDownloading":true,"EnableSyncTranscoding":true,"EnableMediaConversion":true,"EnabledDevices":[],"EnableAllDevices":true,"EnabledChannels":[],"EnableAllChannels":false,"EnabledFolders":["<id>"],"EnableAllFolders":false,"InvalidLoginAttemptCount":0,"LoginAttemptsBeforeLockout":-1,"MaxActiveSessions":0,"EnablePublicSharing":true,"BlockedMediaFolders":[],"BlockedChannels":[],"RemoteClientBitrateLimit":0,"AuthenticationProviderId":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider","PasswordResetProviderId":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider","SyncPlayAccess":"CreateAndJoinGroups"}`
  - `GET /Users/<id>` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users/<id>/profile"}`

### user list: delete (administrator)

- load:
  - `GET /Users/<capture-admin-id>` → 200
  - `GET /Users` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users"}`
- Delete from a card's menu, confirmed:
  - `DELETE /Users/<id>` → 204
  - `GET /Users` → 200
  - shows: `{"messages": ["Delete capture-web-restricted Are you sure you wish to delete this user? Cancel Delete"], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users"}`

### user profile (administrator)

- load:
  - `GET /Users/<capture-admin-id>` → 200
  - `GET /Users/<capture-viewer-two-id>` → 200
  - `GET /Auth/PasswordResetProviders` → 200
  - `GET /Auth/Providers` → 200
  - `GET /System/Configuration/network` → 200
  - `GET /Library/MediaFolders?isHidden=false` → 200
  - `GET /Channels?supportsMediaDeletion=true` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users/<capture-viewer-two-id>/profile"}`
- Save with no change:
  - `POST /Users?userId=<capture-viewer-two-id>` → 204 body `{"Name":"capture-viewer-two","ServerId":"<server-id>","Id":"<capture-viewer-two-id>","HasPassword":true,"HasConfiguredPassword":true,"HasConfiguredEasyPassword":false,"EnableAutoLogin":false,"Configuration":{"PlayDefaultAudioTrack":true,"SubtitleLanguagePreference":"","DisplayMissingEpisodes":false,"GroupedFolders":[],"SubtitleMode":"Default","DisplayCollectionsView":false,"EnableLocalPassword":false,"OrderedViews":[],"LatestItemsExcludes":[],"MyMediaExcludes":[],"HidePlayedInLatest":true,"RememberAudioSelections":true,"RememberSubtitleSelections":true,"EnableNextEpisodeAutoPlay":true,"CastReceiverId":"F007D354"},"Policy":{"IsAdministrator":false,"IsHidden":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"IsDisabled":false,"BlockedTags":[],"AllowedTags":[],"EnableUserPreferenceAccess":true,"AccessSchedules":[],"BlockUnratedItems":[],"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":true,"EnableRemoteAccess":true,"EnableLiveTvManagement":false,"EnableLiveTvAccess":true,"EnableMediaPlayback":true,"EnableAudioPlaybackTranscoding":true,"EnableVideoPlaybackTranscoding":true,"EnablePlaybackRemuxing":true,"ForceRemoteSourceTranscoding":false,"EnableContentDeletion":false,"EnableContentDeletionFromFolders":[],"EnableContentDownloading":true,"EnableSyncTranscoding":true,"EnableMediaConversion":true,"EnabledDevices":[],"EnableAllDevices":true,"EnabledChannels":[],"EnableAllChannels":true,"EnabledFolders":[],"EnableAllFolders":true,"InvalidLoginAttemptCount":2,"LoginAttemptsBeforeLockout":-1,"MaxActiveSessions":0,"EnablePublicSharing":true,"BlockedMediaFolders":[],"BlockedChannels":[],"RemoteClientBitrateLimit":0,"AuthenticationProviderId":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider","PasswordResetProviderId":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider","SyncPlayAccess":"CreateAndJoinGroups"}}`
  - `GET /Users/<capture-viewer-two-id>` → 200
  - `POST /Users/<capture-viewer-two-id>/Policy` → 204 body `{"IsAdministrator":false,"IsHidden":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"IsDisabled":false,"BlockedTags":[],"AllowedTags":[],"EnableUserPreferenceAccess":true,"AccessSchedules":[],"BlockUnratedItems":[],"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":true,"EnableRemoteAccess":true,"EnableLiveTvManagement":false,"EnableLiveTvAccess":true,"EnableMediaPlayback":true,"EnableAudioPlaybackTranscoding":true,"EnableVideoPlaybackTranscoding":true,"EnablePlaybackRemuxing":true,"ForceRemoteSourceTranscoding":false,"EnableContentDeletion":false,"EnableContentDeletionFromFolders":[],"EnableContentDownloading":true,"EnableSyncTranscoding":true,"EnableMediaConversion":true,"EnabledDevices":[],"EnableAllDevices":true,"EnabledChannels":[],"EnableAllChannels":true,"EnabledFolders":[],"EnableAllFolders":true,"InvalidLoginAttemptCount":2,"LoginAttemptsBeforeLockout":-1,"MaxActiveSessions":0,"EnablePublicSharing":true,"BlockedMediaFolders":[],"BlockedChannels":[],"RemoteClientBitrateLimit":0,"AuthenticationProviderId":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider","PasswordResetProviderId":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider","SyncPlayAccess":"CreateAndJoinGroups"}`
  - `GET /Users` → 200
  - shows: `{"messages": ["Settings saved."], "still_shown_at_the_end": ["Settings saved.", "Settings saved."], "spinner_at_the_end": false, "user_against_get": {"not_posted": [], "only_posted": [], "differing": {}}, "policy_against_get": {"not_posted": [], "only_posted": [], "differing": {}}, "hash": "#/dashboard/users"}`

### access (administrator)

- load:
  - `GET /Users/<capture-admin-id>` → 200
  - `GET /Users/<capture-viewer-two-id>` → 200
  - `GET /Library/MediaFolders?IsHidden=false` → 200
  - `GET /Channels` → 200
  - `GET /Devices` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users/<capture-viewer-two-id>/access"}`
- Save with no change:
  - `GET /Users/<capture-viewer-two-id>` → 200
  - `POST /Users/<capture-viewer-two-id>/Policy` → 204 body `{"IsAdministrator":false,"IsHidden":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"IsDisabled":false,"BlockedTags":[],"AllowedTags":[],"EnableUserPreferenceAccess":true,"AccessSchedules":[],"BlockUnratedItems":[],"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":true,"EnableRemoteAccess":true,"EnableLiveTvManagement":false,"EnableLiveTvAccess":true,"EnableMediaPlayback":true,"EnableAudioPlaybackTranscoding":true,"EnableVideoPlaybackTranscoding":true,"EnablePlaybackRemuxing":true,"ForceRemoteSourceTranscoding":false,"EnableContentDeletion":false,"EnableContentDeletionFromFolders":[],"EnableContentDownloading":true,"EnableSyncTranscoding":true,"EnableMediaConversion":true,"EnabledDevices":[],"EnableAllDevices":true,"EnabledChannels":[],"EnableAllChannels":true,"EnabledFolders":[],"EnableAllFolders":true,"InvalidLoginAttemptCount":2,"LoginAttemptsBeforeLockout":-1,"MaxActiveSessions":0,"EnablePublicSharing":true,"BlockedMediaFolders":null,"BlockedChannels":null,"RemoteClientBitrateLimit":0,"AuthenticationProviderId":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider","PasswordResetProviderId":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider","SyncPlayAccess":"CreateAndJoinGroups"}`
  - shows: `{"messages": ["Settings saved."], "still_shown_at_the_end": [], "spinner_at_the_end": false, "policy_against_get": {"not_posted": [], "only_posted": [], "differing": {"BlockedChannels": [[], null], "BlockedMediaFolders": [[], null]}}, "hash": "#/dashboard/users/<capture-viewer-two-id>/access"}`

### parental control (administrator)

- load:
  - `GET /Users/<capture-admin-id>` → 200
  - `GET /Users/<capture-viewer-two-id>` → 200
  - `GET /Localization/ParentalRatings` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/dashboard/users/<capture-viewer-two-id>/parentalcontrol"}`
- Save with no change:
  - `GET /Users/<capture-viewer-two-id>` → 200
  - `POST /Users/<capture-viewer-two-id>/Policy` → 204 body `{"IsAdministrator":false,"IsHidden":true,"EnableCollectionManagement":false,"EnableSubtitleManagement":false,"EnableLyricManagement":false,"IsDisabled":false,"BlockedTags":[],"AllowedTags":[],"EnableUserPreferenceAccess":true,"AccessSchedules":[],"BlockUnratedItems":[],"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":true,"EnableRemoteAccess":true,"EnableLiveTvManagement":false,"EnableLiveTvAccess":true,"EnableMediaPlayback":true,"EnableAudioPlaybackTranscoding":true,"EnableVideoPlaybackTranscoding":true,"EnablePlaybackRemuxing":true,"ForceRemoteSourceTranscoding":false,"EnableContentDeletion":false,"EnableContentDeletionFromFolders":[],"EnableContentDownloading":true,"EnableSyncTranscoding":true,"EnableMediaConversion":true,"EnabledDevices":[],"EnableAllDevices":true,"EnabledChannels":[],"EnableAllChannels":true,"EnabledFolders":[],"EnableAllFolders":true,"InvalidLoginAttemptCount":2,"LoginAttemptsBeforeLockout":-1,"MaxActiveSessions":0,"EnablePublicSharing":true,"BlockedMediaFolders":[],"BlockedChannels":[],"RemoteClientBitrateLimit":0,"AuthenticationProviderId":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider","PasswordResetProviderId":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider","SyncPlayAccess":"CreateAndJoinGroups"}`
  - shows: `{"messages": ["Settings saved."], "still_shown_at_the_end": [], "spinner_at_the_end": false, "policy_against_get": {"not_posted": [], "only_posted": [], "differing": {}}, "hash": "#/dashboard/users/<capture-viewer-two-id>/parentalcontrol"}`

### password (another user) (administrator)

- load:
  - `GET /Users/<capture-admin-id>` → 200
  - `GET /Users/<capture-viewer-two-id>` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "current_password_field": true, "hash": "#/dashboard/users/<capture-viewer-two-id>/password"}`
- Save Password:
  - `POST /Users/<capture-viewer-two-id>/Password` → 204 body `{"CurrentPw":"","NewPw":"<new-password>"}`
  - shows: `{"messages": ["Password saved."], "still_shown_at_the_end": ["Password saved."], "spinner_at_the_end": false, "hash": "#/dashboard/users/<capture-viewer-two-id>/password"}`
- Reset Password, confirmed:
  - `POST /Users/<capture-viewer-two-id>/Password` → 204 body `{"resetPassword":true}`
  - shows: `{"messages": ["Reset Password Are you sure you wish to reset the password? Cancel OK", "Reset Password The password has been reset. Got It"], "still_shown_at_the_end": ["Reset Password The password has been reset. Got It"], "spinner_at_the_end": false, "hash": "#/dashboard/users/<capture-viewer-two-id>/password"}`

### Quick Connect (administrator)

- load:
  - `GET /Users/<capture-admin-id>` → 200
  - `GET /Users` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "user_choices": ["capture-admin", "capture-viewer-one", "capture-viewer-two", "capture-web-made"], "hash": "#/quickconnect"}`
- Authorize a real code:
  - `POST /QuickConnect/Authorize?code=<code>&userId=<capture-admin-id>` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/quickconnect"}`

### own profile and password (viewer)

- load:
  - `GET /Users/<capture-viewer-one-id>` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/userprofile?userId=<capture-viewer-one-id>"}`
- Save Password with a wrong current password (the server's real answer):
  - `POST /Users/<capture-viewer-one-id>/Password` → 403 body `{"CurrentPw":"wrong","NewPw":"<new-password>"}`
  - shows: `{"messages": ["Login Failure Invalid username or password. Please try again. Got It"], "still_shown_at_the_end": ["Login Failure Invalid username or password. Please try again. Got It"], "spinner_at_the_end": false, "hash": "#/userprofile?userId=<capture-viewer-one-id>"}`
- load again:
  - `GET /Users/<capture-viewer-one-id>` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/userprofile?userId=<capture-viewer-one-id>"}`
- Save Password with the right current password:
  - `POST /Users/<capture-viewer-one-id>/Password` → 204 body `{"CurrentPw":"<right-password>","NewPw":"<new-password>"}`
  - shows: `{"messages": ["Password saved."], "still_shown_at_the_end": ["Password saved."], "spinner_at_the_end": false, "hash": "#/userprofile?userId=<capture-viewer-one-id>"}`
- load once more:
  - `GET /Users/<capture-viewer-one-id>` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/userprofile?userId=<capture-viewer-one-id>"}`
- Reset Password, confirmed:
  - `POST /Users/<capture-viewer-one-id>/Password` → 204 body `{"resetPassword":true}`
  - shows: `{"messages": ["Reset Password Are you sure you wish to reset the password? Cancel OK", "Reset Password The password has been reset. Got It"], "still_shown_at_the_end": ["Reset Password The password has been reset. Got It"], "spinner_at_the_end": false, "hash": "#/userprofile?userId=<capture-viewer-one-id>"}`

### Quick Connect (viewer)

- load:
  - `GET /Users/<capture-viewer-one-id>` → 200
  - `GET /Users` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/quickconnect"}`
- Authorize an unknown code (the server's real answer):
  - `POST /QuickConnect/Authorize?code=<code>` → 404
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/quickconnect"}`
- load again:
  - `GET /Users/<capture-viewer-one-id>` → 200
  - `GET /Users` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/quickconnect"}`
- Authorize a real code:
  - `POST /QuickConnect/Authorize?code=<code>` → 200
  - shows: `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "hash": "#/quickconnect"}`

## jellyfin-web: answers of 400 and 403 (given by the capture in the server's place)

- user list, load, `GET /Users` → 400: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "page_text": "Users Users"}`; ends at `#/dashboard/users`; requests after the refusal: none; user requests of the load: `GET /Users/<capture-admin-id> 200`, `GET /Users 400`
- user list, load, `GET /Users` → 403: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false, "page_text": "Users Users"}`; ends at `#/dashboard/users`; requests after the refusal: none; user requests of the load: `GET /Users/<capture-admin-id> 200`, `GET /Users 403`
- user list, delete, `DELETE /Users/<capture-web-made-id>` → 400: shows `{"messages": ["Delete capture-web-made Are you sure you wish to delete this user? Cancel Delete"], "still_shown_at_the_end": [], "spinner_at_the_end": false}`; ends at `#/dashboard/users`; requests after the refusal: none
- user list, delete, `DELETE /Users/<capture-web-made-id>` → 403: shows `{"messages": ["Delete capture-web-made Are you sure you wish to delete this user? Cancel Delete"], "still_shown_at_the_end": [], "spinner_at_the_end": false}`; ends at `#/dashboard/users`; requests after the refusal: none
- new user, Save, `POST /Users/New` → 400: shows `{"messages": ["There was an error processing the request. Please try again later."], "still_shown_at_the_end": ["There was an error processing the request. Please try again later.", "There was an error processing the request. Please try again later."], "spinner_at_the_end": false}`; ends at `#/dashboard/users/add`; requests after the refusal: none
- new user, Save, `POST /Users/New` → 403: shows `{"messages": ["There was an error processing the request. Please try again later."], "still_shown_at_the_end": ["There was an error processing the request. Please try again later.", "There was an error processing the request. Please try again later."], "spinner_at_the_end": false}`; ends at `#/dashboard/users/add`; requests after the refusal: none
- new user, Save, the Policy that follows, `POST /Users/<capture-web-half-6-id>/Policy` → 400: shows `{"messages": ["There was an error processing the request. Please try again later."], "still_shown_at_the_end": ["There was an error processing the request. Please try again later.", "There was an error processing the request. Please try again later."], "spinner_at_the_end": true}`; ends at `#/dashboard/users/add`; requests after the refusal: none
- new user, Save, the Policy that follows, `POST /Users/<capture-web-half-7-id>/Policy` → 403: shows `{"messages": ["There was an error processing the request. Please try again later."], "still_shown_at_the_end": ["There was an error processing the request. Please try again later.", "There was an error processing the request. Please try again later."], "spinner_at_the_end": true}`; ends at `#/dashboard/users/add`; requests after the refusal: none
- user profile, load, `GET /Users/<capture-viewer-two-id>` → 400: shows `{"messages": ["Failed to load users edit page"], "still_shown_at_the_end": ["Failed to load users edit page"], "spinner_at_the_end": false, "page_text": "Failed to load users edit page Failed to load users edit page"}`; ends at `#/dashboard/users/<capture-viewer-two-id>/profile`; requests after the refusal: none; user requests of the load: `GET /Users/<capture-admin-id> 200`, `GET /Users/<capture-viewer-two-id> 400`
- user profile, load, `GET /Users/<capture-viewer-two-id>` → 403: shows `{"messages": ["Failed to load users edit page"], "still_shown_at_the_end": ["Failed to load users edit page"], "spinner_at_the_end": false, "page_text": "Failed to load users edit page Failed to load users edit page"}`; ends at `#/dashboard/users/<capture-viewer-two-id>/profile`; requests after the refusal: none; user requests of the load: `GET /Users/<capture-admin-id> 200`, `GET /Users/<capture-viewer-two-id> 403`
- user profile, Save, the user, `POST /Users?userId=<capture-viewer-two-id>` → 400: shows `{"messages": ["There was an error processing the request. Please try again later."], "still_shown_at_the_end": ["There was an error processing the request. Please try again later.", "There was an error processing the request. Please try again later."], "spinner_at_the_end": false}`; ends at `#/dashboard/users/<capture-viewer-two-id>/profile`; requests after the refusal: none
- user profile, Save, the user, `POST /Users?userId=<capture-viewer-two-id>` → 403: shows `{"messages": ["There was an error processing the request. Please try again later."], "still_shown_at_the_end": ["There was an error processing the request. Please try again later.", "There was an error processing the request. Please try again later."], "spinner_at_the_end": false}`; ends at `#/dashboard/users/<capture-viewer-two-id>/profile`; requests after the refusal: none
- user profile, Save, the Policy, `POST /Users/<capture-viewer-two-id>/Policy` → 400: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/dashboard/users/<capture-viewer-two-id>/profile`; requests after the refusal: `GET /Users/<capture-viewer-two-id> 200`
- user profile, Save, the Policy, `POST /Users/<capture-viewer-two-id>/Policy` → 403: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/dashboard/users/<capture-viewer-two-id>/profile`; requests after the refusal: `GET /Users/<capture-viewer-two-id> 200`
- access, Save, `POST /Users/<capture-viewer-two-id>/Policy` → 400: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/dashboard/users/<capture-viewer-two-id>/access`; requests after the refusal: none
- access, Save, `POST /Users/<capture-viewer-two-id>/Policy` → 403: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/dashboard/users/<capture-viewer-two-id>/access`; requests after the refusal: none
- parental control, Save, `POST /Users/<capture-viewer-two-id>/Policy` → 400: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/dashboard/users/<capture-viewer-two-id>/parentalcontrol`; requests after the refusal: none
- parental control, Save, `POST /Users/<capture-viewer-two-id>/Policy` → 403: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/dashboard/users/<capture-viewer-two-id>/parentalcontrol`; requests after the refusal: none
- password (another user), Save Password, `POST /Users/<capture-viewer-two-id>/Password` → 400: shows `{"messages": ["Login Failure Invalid username or password. Please try again. Got It"], "still_shown_at_the_end": ["Login Failure Invalid username or password. Please try again. Got It"], "spinner_at_the_end": false}`; ends at `#/dashboard/users/<capture-viewer-two-id>/password`; requests after the refusal: none
- password (another user), Save Password, `POST /Users/<capture-viewer-two-id>/Password` → 403: shows `{"messages": ["Login Failure Invalid username or password. Please try again. Got It"], "still_shown_at_the_end": ["Login Failure Invalid username or password. Please try again. Got It"], "spinner_at_the_end": false}`; ends at `#/dashboard/users/<capture-viewer-two-id>/password`; requests after the refusal: none
- password (another user), Reset Password, `POST /Users/<capture-viewer-two-id>/Password` → 400: shows `{"messages": ["Reset Password Are you sure you wish to reset the password? Cancel OK"], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/dashboard/users/<capture-viewer-two-id>/password`; requests after the refusal: none
- password (another user), Reset Password, `POST /Users/<capture-viewer-two-id>/Password` → 403: shows `{"messages": ["Reset Password Are you sure you wish to reset the password? Cancel OK"], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/dashboard/users/<capture-viewer-two-id>/password`; requests after the refusal: none
- Quick Connect, Authorize, `POST /QuickConnect/Authorize?code=<code>&userId=<capture-admin-id>` → 400: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false}`; ends at `#/quickconnect`; requests after the refusal: none
- Quick Connect, Authorize, `POST /QuickConnect/Authorize?code=<code>&userId=<capture-admin-id>` → 403: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false}`; ends at `#/quickconnect`; requests after the refusal: none
- own profile, Save Password, `POST /Users/<capture-viewer-one-id>/Password` → 400: shows `{"messages": ["Login Failure Invalid username or password. Please try again. Got It"], "still_shown_at_the_end": ["Login Failure Invalid username or password. Please try again. Got It"], "spinner_at_the_end": false}`; ends at `#/userprofile?userId=<capture-viewer-one-id>`; requests after the refusal: none
- own profile, Save Password, `POST /Users/<capture-viewer-one-id>/Password` → 403: shows `{"messages": ["Login Failure Invalid username or password. Please try again. Got It"], "still_shown_at_the_end": ["Login Failure Invalid username or password. Please try again. Got It"], "spinner_at_the_end": false}`; ends at `#/userprofile?userId=<capture-viewer-one-id>`; requests after the refusal: none
- own profile, Reset Password, `POST /Users/<capture-viewer-one-id>/Password` → 400: shows `{"messages": ["Reset Password Are you sure you wish to reset the password? Cancel OK"], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/userprofile?userId=<capture-viewer-one-id>`; requests after the refusal: none
- own profile, Reset Password, `POST /Users/<capture-viewer-one-id>/Password` → 403: shows `{"messages": ["Reset Password Are you sure you wish to reset the password? Cancel OK"], "still_shown_at_the_end": [], "spinner_at_the_end": true}`; ends at `#/userprofile?userId=<capture-viewer-one-id>`; requests after the refusal: none
- Quick Connect, Authorize, `POST /QuickConnect/Authorize?code=<code>` → 400: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false}`; ends at `#/quickconnect`; requests after the refusal: none
- Quick Connect, Authorize, `POST /QuickConnect/Authorize?code=<code>` → 403: shows `{"messages": [], "still_shown_at_the_end": [], "spinner_at_the_end": false}`; ends at `#/quickconnect`; requests after the refusal: none

## Obsolete routes

The server's `/api-docs/openapi.json` marks 12 operations deprecated. Requests of these pages that match one: none. Requests that match no operation in the document: `POST /Users/<capture-viewer-one-id>/Password`, `POST /Users/<capture-viewer-two-id>/Password`.
