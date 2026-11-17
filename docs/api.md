# jellyscale's own API

Jellyfin's API has no place for some of what jellyscale offers a viewer. Those routes live under `/jellyscale/v1/`, never beside Jellyfin's, and no Jellyfin answer carries a field of them. A client finds out that it is talking to jellyscale by this namespace answering: a real Jellyfin answers 404.

The version in the path, `v1`, is the shape of these routes. Within v1 a field may be added to an answer, but no field, route or accepted spelling is removed or changed in meaning; a break is `v2`, served beside v1. Field names are exact (`HideOtherAudio`, not `hideotheraudio`), so v1 never accepts a spelling it would have to take away. Paths, like all of jellyscale's, are matched without regard to case.

Every route needs a token (docs/access.md), acts on the caller only and takes no user id; a `userId` in the query is ignored. A request without a known token is answered 401.

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

- 400, with the text of what is wrong, and nothing stored (a body with several faults stores none of it): a body over 16 KB, not JSON, empty, not an object, or followed by more text; a field that is not one of the five (names are exact), is named twice, or has the wrong type (the switches are true or false, the lists are lists of strings; null is not read as absent); a list entry that names no language (`xx`, `german`, the empty string, `und`, `mis`, `mul`, `zxx`; the answer gives it as sent); a list of more than 20 languages.
- 409, nothing stored: `HideOtherAudio` true while the caller's `AudioLanguagePreference` is empty, or `HideOtherSubtitles` true while `SubtitleLanguagePreference` is empty. The answer names the language to save first, with `POST /Users/Configuration`. `OriginalLanguage` counts as saved. Nothing else needs a language, and a language cleared afterwards leaves the stored document as it was.
- 401: no token, or one jellyscale does not know.

## Playback

### POST /jellyscale/v1/items/{itemId}/playback-info

PlaybackInfo for a client that hides tracks itself. The request is the same as `POST /Items/{itemId}/PlaybackInfo`: the same body and query, and the same answer in every field, with two differences. Every audio and subtitle track is listed as itself, never as a `Type: Data` placeholder, so the list holds every track in the file, in its order, whatever a worker or a replica can do (a track PlaybackInfo drops because it cannot be delivered there is listed here without a `DeliveryUrl`, never dropped, because a client that plays the original file maps tracks to indexes partly by order). And two fields are added, which no Jellyfin answer carries:

- `Hidden` on each audio and subtitle track of each source: true when PlaybackInfo would list the track as a placeholder for this viewer and request (the saved languages, the switches, `ShowAllTracks`, and the track the request names by `AudioStreamIndex` or `SubtitleStreamIndex`), false otherwise. A video track has no `Hidden`. A hidden text track keeps its `DeliveryUrl`.
- `OriginalLanguage` on each source: the language tag of that source's default audio track, as `GET /Items/{itemId}` answers for the item, or `""` for a film whose default audio names no language or that has no audio. It is always present.

One rule on the server decides what is hidden, so every client agrees. The marks come from the caller's token; a `UserId` is ignored.

```json
{"Index": 3, "Type": "Audio", "Language": "deu", "Hidden": true}
```

The answers that are not 200 are PlaybackInfo's: 404 for an unknown or malformed item id, 400 for a body that is not a JSON object, 401 for no token or an unknown one. A transcode that cannot start is answered 200 with no sources and `ErrorCode` `RateLimitExceeded`, as PlaybackInfo does.

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

- 400, with the text of what is wrong, and nothing stored (a body with one good and one refused field stores neither): a body of 65,537 bytes or more, not JSON, empty, not an object, or followed by more text; a field that is not one of the 15 (names are exact: `subtitleMode` is unknown; `CastReceiverId` is refused), is named twice, is `null`, or has the wrong type (the switches are true or false, the lists are lists of ids); a `SubtitleMode` outside `Default`, `Always`, `OnlyForced`, `None`, `Smart` and 0 to 4; a list entry that is not an id.
- 401: no token, or one jellyscale does not know.

A change is logged at Info with the user id and the names of the fields sent.
