# jellyscale's own API

Jellyfin's API has no place for some of what jellyscale offers a viewer. Those routes live under `/jellyscale/v1/`, never beside Jellyfin's, and no Jellyfin answer carries a field of them. A client finds out that it is talking to jellyscale by this namespace answering: a real Jellyfin answers 404.

The version in the path, `v1`, is the shape of these routes. Within v1 a field may be added to an answer, once the contract declares it, and a client ignores fields it does not know; no field, route or accepted spelling is removed or changed in meaning. A break is `v2`, served beside v1. The rule binds stable operations. An operation in `preview` or `declared` may rename, remove or change a field, query key or envelope member before it is marked stable, and only by a change that reaches Octave as a from:jellyscale issue naming each change before it is served (D-132). An operation is never changed this way after it is stable. The leaving operations are not renamed; they go at the cutover. The server answers only what its contract declares: the conformance check holds each typed answer to it (`additionalProperties: false`). The closed list of reasons is the one thing v1 extends: an entry may be added, never removed or renamed. Field names are exact (`hideOtherAudio`, not `hideotheraudio`), so v1 never accepts a spelling it would have to take away. Every field, query key and refusal member is camelCase, except the bodies of the three leaving operations below, which keep the names they were served with until the cutover. Paths, like all of jellyscale's, are matched without regard to case.

A client reads `GET /jellyscale/v1/contract` and `GET /jellyscale/v1/server` with no token. The contract is the OpenAPI 3.1 document at `internal/contract/openapi.yaml`, which the image carries and serves byte for byte; identity gives the build's version, `apiVersion: v1` and every capability with its state (`preview`, `declared` or `stable`). A capability the server has not built is `declared` and its route answers the `not-served` refusal, including when it shares a path with a served method. No capability is `stable` until a CI check fails on an incompatible change to it. `playback-info`, `user-configuration` and `track-preferences` leave at the cutover and can never be stable.

Every refusal on this namespace is `{"reason": "<code>", "action": "<what the viewer can do>"}`, with an optional `detail`; `too-many-tries` also carries `retryAfterSeconds` and `scope`. `reason` and `action` are open strings with known values: a client generated from an older copy decodes one it does not know. `detail` is for the app developer, not for the viewer; it says which field or value was wrong where the old text did, and its wording may change within a version, so a client must not branch on it. The server never writes `detail` to a log, because it can hold a value from the request. The codes are the closed list in the contract's `x-reasons`, each meaning written in words a viewer understands; the operator's detail goes in the server's log. The four transport reasons — an unknown route, a wrong method, a missing sign-in and a server failure — and any reason of a stable capability are added within a version, never reworded, removed or renamed. A reason used only by a preview capability may be reworded until its capability becomes stable, which the task that marks it stable locks. Each response declares the reasons it may carry, including a 200 body's. The server sends only a status and reason the operation declares: anything else is logged at error as `undeclared answer` and answered 500 `server-failure`. A viewer's own mistake is logged at Debug with the route, status and reason, so it never reaches an operator's Info log; a failure inside the server is logged at Error. A PlaybackInfo the server cannot start carries `reason` and `action` beside Jellyfin's `ErrorCode` in a 200 body. Jellyfin's routes keep their text refusals.

Every route below needs a token (docs/access.md) and acts on the caller only. A `userId` or `controllableByUserId` query value, or a PlaybackInfo body's `UserId`, must name the caller in compact or dashed form, in any letter case; another, unknown or malformed id is answered 403, and an empty value is absent. A request without a known token is answered 401 before its user id is checked.

## Sign-in (declared)

`POST /jellyscale/v1/sign-in`, `POST /jellyscale/v1/sign-out` and `GET /jellyscale/v1/me` are declared under the capability `sign-in`, with example requests and answers to build against, and each answers `404 not-served` until JS-184 serves them. The contract gives their shapes; in short:

- Sign-in takes `username`, `password`, `deviceId`, `deviceName` and `appName`, with no token. `deviceId` is a random id the app makes once per installation (at least 128 random bits as text: 22 to 128 characters of letters, digits and `.`, `_`, `~`, `-`), keeps private, never shows the viewer and sends on every sign-in from that installation, for every account on it: it is what the per known device bound of docs/access.md counts by, so a guesser cannot keep a viewer out of a device that has signed in before, and what makes a second sign-in from the same installation replace its token rather than add a device. The answer's `device.id` is the server's own id for the device, not this value. Sign-in answers `token` (given once), `account` (`id`, `name`, `administrator`), `profile` (`id`, `name`) and `device` (`id`, `name`). Its refusals: 400 `invalid-body`; 401 `credentials-not-accepted`, the same for a name that is no account; 403 `account-off`, only after the right password; 429 `too-many-tries` with `Retry-After`, `retryAfterSeconds` (1 to 900) and `scope` (`name`, `device` or `network`, the bound of docs/access.md that refused it).
- `/me` answers the caller's `account`, `profile` and `device`, and no token. Sign-out ends the calling device and answers 204 with no body. Both answer 401 `unauthorized` without a token the server knows.
- A token is sent as `Authorization: Bearer <token>`, or in any spelling Jellyfin's clients use today (docs/access.md); the contract's `securitySchemes` list them.

## Track preferences

A viewer's choice to hide audio and subtitle tracks outside their languages, the languages to keep in addition, and one switch to bring every track back. They are saved here, and JS-119 hides the tracks by them.

### GET /jellyscale/v1/track-preferences

The request has no body. The answer is 200 with the caller's own document, always these five fields:

```json
{"HideOtherAudio": false, "HideOtherSubtitles": false, "KeepAudioLanguages": [], "KeepSubtitleLanguages": [], "ShowAllTracks": false}
```

The defaults, for a viewer who has saved nothing, are both switches off, both lists empty and ShowAllTracks off. The lists are always arrays, never null.

### PATCH /jellyscale/v1/track-preferences

The request is one JSON object of at most 16 KB (16384 bytes) that names the fields to change; the others stay as stored, and `{}` changes nothing. The answer is 200 with the whole document as stored, the same as the next GET. A list that is named is replaced whole, not added to: `["deu"]` and then `["fra"]` is `["fra"]`. PUT and POST are not allowed.

```json
{"HideOtherAudio": true, "KeepAudioLanguages": ["ger", "fr"]}
```

Languages in a list are stored as lowercase three-letter codes, each once, in the order first sent, whatever the spelling sent: `["GER", "deu", "de", "fr"]` is `["deu", "fra"]`. A list holds at most 20 languages.

### Answers that are not 200

- 400 `invalid-body`, nothing stored (the log says why) (a body with several faults stores none of it): a body over 16 KB, not JSON, empty, not an object, or followed by more text; a field that is not one of the five (names are exact), is named twice, or has the wrong type (the switches are true or false, the lists are lists of strings; null is not read as absent); a list entry that names no language (`xx`, `german`, the empty string, `und`, `mis`, `mul`, `zxx`); a list of more than 20 languages.
- 409 `language-not-saved`, nothing stored: `HideOtherAudio` true while the caller's `AudioLanguagePreference` is empty, or `HideOtherSubtitles` true while `SubtitleLanguagePreference` is empty. Save that language first, with `POST /Users/Configuration`; the log says which one. `OriginalLanguage` counts as saved. Nothing else needs a language, and a language cleared afterwards leaves the stored document as it was.
- 401 `unauthorized`: no token, or one jellyscale does not know.

## Playback

### POST /jellyscale/v1/items/{itemId}/playback-info

PlaybackInfo for a client that hides tracks itself. The request is the same as `POST /Items/{itemId}/PlaybackInfo`: the same body and query, and the same answer in every field, with two differences. Every audio and subtitle track is listed as itself, never as a `Type: Data` placeholder, so the list holds every track in the file, in its order, whatever a worker or a replica can do (a track PlaybackInfo drops because it cannot be delivered there is listed here without a `DeliveryUrl`, never dropped, because a client that plays the original file maps tracks to indexes partly by order). And two fields are added, which no Jellyfin answer carries:

- `Hidden` on each audio and subtitle track of each source: true when PlaybackInfo would list the track as a placeholder for this viewer and request (the saved languages, the switches, `ShowAllTracks`, and the track the request names by `AudioStreamIndex` or `SubtitleStreamIndex`), false otherwise. A video track has no `Hidden`. A hidden text track keeps its `DeliveryUrl`.
- `OriginalLanguage` on each source: the language tag of that source's default audio track, as `GET /Items/{itemId}` answers for the item, or `""` for a film whose default audio names no language or that has no audio. It is always present.

One rule on the server decides what is hidden, so every client agrees. The marks come from the caller's token; a `UserId` naming another viewer is refused with 403, and one naming the caller is equivalent to no `UserId`.

```json
{"Index": 3, "Type": "Audio", "Language": "deu", "Hidden": true}
```

The statuses are PlaybackInfo's, each with its reason: 404 `not-found` for an unknown or malformed item id, 400 `invalid-body` for a body that is not a JSON object, 401 `unauthorized` for no token or an unknown one, and 403 `forbidden` when a user id names anyone but the caller. A 200 with no sources carries `reason` and `action`: `no-capacity` beside `ErrorCode` `RateLimitExceeded` for a transcode that cannot start, `not-found` for a media source the item does not have, and `nothing-to-play` for an item with no sources of its own, such as a series.

## A viewer's settings

Jellyfin's `POST /Users/Configuration` replaces the whole Configuration, so a client that reads it, changes a field and posts everything back undoes what another app saved in between. This route changes only the fields it names.

### PATCH /jellyscale/v1/users/configuration

The request is one JSON object of at most 65,536 bytes that names any of the 15 savable fields of `UserConfiguration` (`AudioLanguagePreference`, `PlayDefaultAudioTrack`, `SubtitleLanguagePreference`, `DisplayMissingEpisodes`, `GroupedFolders`, `SubtitleMode`, `DisplayCollectionsView`, `EnableLocalPassword`, `OrderedViews`, `LatestItemsExcludes`, `MyMediaExcludes`, `HidePlayedInLatest`, `RememberAudioSelections`, `RememberSubtitleSelections`, `EnableNextEpisodeAutoPlay`). The others stay as stored, and `{}` changes nothing. The answer is 200 with the whole 16-field Configuration, the same as the next `GET /Users/Me` carries it.

```json
{"AudioLanguagePreference": "deu", "SubtitleMode": "Always"}
```

Values are read as `POST /Users/Configuration` reads them (`SubtitleMode` takes a listed word in any case or its number; an id may be dashed or compact, and reads compact), with one difference: `null` is refused for every field, never "back to the default", so a body has one meaning. A language is cleared with `""`. A list that is named replaces the stored list whole, in the order sent, repeats kept: `[]` empties it. A viewer who has saved nothing reads the new user's value for every field not named. `CastReceiverId` cannot be saved, as on Jellyfin.

The route acts on the caller alone. The change is merged in one statement onto what is stored, so two PATCHes of different fields at once both land. Two PATCHes of the same field are decided by the later statement: the one that reaches the database last wins, whichever request arrived first. A language saved here is the one `PATCH /jellyscale/v1/track-preferences` reads.

Limit: a whole-object save from an older read, as jellyfin-web makes with `POST /Users/Configuration`, still replaces everything it carries, so it can undo a change made here after that read. Jellyfin 12.4 does the same, and the track preferences are out of its reach.

Answers that are not 200:

- 400 `invalid-body`, nothing stored (the log says why) (a body with one good and one refused field stores neither): a body of 65,537 bytes or more, not JSON, empty, not an object, or followed by more text; a field that is not one of the 15 (names are exact: `subtitleMode` is unknown; `CastReceiverId` is refused), is named twice, is `null`, or has the wrong type (the switches are true or false, the lists are lists of ids); a `SubtitleMode` outside `Default`, `Always`, `OnlyForced`, `None`, `Smart` and 0 to 4; a list entry that is not an id.
- 401 `unauthorized`: no token, or one jellyscale does not know.

A change is logged at Info with the user id and the names of the fields sent.
