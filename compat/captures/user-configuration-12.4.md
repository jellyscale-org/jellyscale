# What Jellyfin 12.4 does with a viewer's settings (JS-116)

Observed on `jellyfin/jellyfin:12.4` (server 12.4.0), empty library, by `test/differential/capture_user_configuration.py`, which writes `user-configuration-12.4.json` here. For each of 124 cases it makes three new users (U and O, not administrators; A, an administrator), sends the case's earlier steps, then its request, and records the status and, after it, the `Configuration` of each of the three in `GET /Users/Me` (their own token), in `GET /Users/{id}` by A and by U, and in the `User` of U's next `AuthenticateByName`. Synthetic names only. Each `Configuration` in the JSON is written as its difference from the new user's (`set` the fields that differ, `absent` the fields it lacks); `agree: true` says the four places held the same. The Go replay is `cmd/jellyscale/user_configuration_integration_test.go`.

## A new user who has saved nothing

15 fields. `AudioLanguagePreference` is **absent** (the 16th). `CastReceiverId` is `"F007D354"`, the same for every user and after every save.

| Field | New user |
|---|---|
| PlayDefaultAudioTrack | true |
| SubtitleLanguagePreference | `""` |
| DisplayMissingEpisodes | false |
| GroupedFolders, OrderedViews, LatestItemsExcludes, MyMediaExcludes | `[]` |
| SubtitleMode | `"Default"` |
| DisplayCollectionsView, EnableLocalPassword | false |
| HidePlayedInLatest, RememberAudioSelections, RememberSubtitleSelections, EnableNextEpisodeAutoPlay | true |
| CastReceiverId | `"F007D354"` |

## Saving

`POST /Users/{userId}/Configuration`, `POST /Users/Configuration?userId=` and `POST /Users/Configuration` (names nobody: the caller) behave alike in every case; the three routes were run through every case.

- **Success is 204 with no body.** Every field read back through the four places is what was sent, ids in the lists as 32 lower-case hex digits in the order sent (a repeated id stays repeated; dashed and upper-case ids come back compact and lower case).
- **A body replaces the whole Configuration.** A field the body lacks goes back to the new user's value, whatever it was: a body with only `SubtitleMode` after all fields were changed leaves that mode and the 14 defaults (`AudioLanguagePreference` absent again), and two bodies of one field each in turn leave only the second one's. Nothing merges, so the "two one-field bodies that name different fields both take effect" case does not exist: the later body wins whole.
- **`CastReceiverId` cannot be saved.** A body naming `"ABCD1234"` reads back `"F007D354"`.
- **Unknown fields are dropped silently** (204); the known fields beside them are saved, and the unknown field's name and value appear in no answer.
- **`SubtitleMode`** takes the five words and also, with no error: the same word in lower case (`"always"` reads back `"Always"`) and a number, by position (0 `Default`, 1 `Always`, 2 `OnlyForced`, 3 `None`, 4 `Smart`). An unlisted word (`"Sometimes"`), `""` and `null` are 400 and change nothing. What reads back is always one of the five spellings.
- **A language cleared with `""` reads back `""`; cleared with `null` or left out, `AudioLanguagePreference` is absent and `SubtitleLanguagePreference` is `""`.** `"OriginalLanguage"` is saved as it is.
- **400, nothing changed:** a boolean sent as a string (`"true"`), a list entry that is not an id, a list sent as a string, a body that is not JSON, an empty body, JSON that is not an object (`[1,2]`), JSON `null`.
- **A body over 64 KB is accepted and saved** (70 KB of padding in an unknown field, 2000 ids in a list, 70 KB of spaces after the object: 204, stored). jellyscale refuses these on purpose (400, the status of a body that is not JSON, nothing stored); a full body with 20 ids in each list is saved. JS-116 names that improvement; the capture records Jellyfin's own answer under "body over 64 KB".
- **No token, or a token no device holds: 401**, nothing changed.
- **Another user's id as a non-administrator: 403** (in the path and in the query); nothing changes for either user.
- **An administrator naming another user writes that user's fields** and leaves their own; naming themselves writes their own.
- **A unknown id is 404 for an administrator and for a non-administrator alike; a malformed id is 400 for both.** Neither changes anything. (The non-administrator is not told 403 for an id that is no user.)
- **A user naming themselves** (dashed or compact, either case, in the path or the query) saves as one naming nobody.

## `GET /Localization/Cultures`

200, `application/json; charset=utf-8`, **494 entries**, each with the five fields `Name`, `DisplayName`, `TwoLetterISOLanguageName`, `ThreeLetterISOLanguageName`, `ThreeLetterISOLanguageNames` (a list). Sorted by name: the first is Abkhazian (`abk`), then Achinese, Acoli, Adangme. German is `{"Name":"German","DisplayName":"German","TwoLetterISOLanguageName":"de","ThreeLetterISOLanguageName":"deu","ThreeLetterISOLanguageNames":["deu","ger"]}`. A language with no two-letter code has `""` there. **Without a token: 401**, empty body. The full list is `cultures.entries` in the JSON.

## What jellyfin-web does with them (Chromium against the same server)

- `#/mypreferencessubtitles` fills `#selectSubtitleLanguage` from `GET /Localization/cultures` (lower-case path): 495 options, "Any language" (value empty) and the 494 cultures by `ThreeLetterISOLanguageName`, so `deu=German`. `#selectSubtitlePlaybackMode` has `Default`, `Smart`, `Only Forced`, `Always Play`, `None` (values `Default`, `Smart`, `OnlyForced`, `Always`, `None`).
- `#/mypreferencesplayback` fills `#selectAudioLanguage` the same way, with `OriginalLanguage=Original language` between "Any language" and the cultures (496 options), and has the checkbox "Play default audio track regardless of language" (ticked for a new user).
- Each page's Save is one `POST /Users/{id}/Configuration` with the whole Configuration it was given, fields changed, and shows "Settings saved". After a full reload, the Playback page shows German and the box unticked; the Subtitles page, opened after it, still shows German and Always (the Playback save carried the Subtitles values, because the page posts back what it holds).

## What the replay holds jellyscale to

Status, and the 16 fields in the four places, as above, except where this file says jellyscale does better: a body over 64 KB; and, for the one case JS-127 changes, a non-administrator naming an id that is no user (`unknown id as a non-administrator`, in the path and in the query), which Jellyfin answers 404 and jellyscale 403 with nothing written, so that the answer does not say whether an id is a user (D-075; `compat/captures/user-scope-12.4.md`).
