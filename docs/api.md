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
