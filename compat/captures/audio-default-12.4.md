# What Jellyfin 12.4 names as a film's default audio track (JS-121)

Observed 2026-11-16, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0, image id `fea863d0cd9e`), a disposable container on a Docker-assigned port (`-p 127.0.0.1::8096`), no metadata provider (`EnableInternetProviders` false, `MetadataFetchers` and `ImageFetchers` empty, as in `original-language-12.4.md`). Made and asked by a real Jellyfin 12.4; the answers, cut to the fields below, are in `audio-default-12.4.json` (same directory). The films are 3 second 160x90 H.264 files with AAC mono tracks and synthetic names (`Audio Def First (2020)` and so on), mounted read-only.

## What was asked

`POST /Items/{id}/PlaybackInfo?UserId=` with jellyfin-web's body: `UserId, StartTimeTicks: 0, IsPlayback: true, AutoOpenLiveStream: true, MediaSourceId, MaxStreamingBitrate: 2147483647, AlwaysBurnInSubtitleWhenTranscoding: false` and the `DeviceProfile` of `transcode-12.4.json`. jellyfin-web names no `AudioStreamIndex` when a film starts; the second capture below does. One user, whose Configuration was saved before each round with `POST /Users/{id}/Configuration` (the saved `AudioLanguagePreference` and `PlayDefaultAudioTrack` read back from `GET /Users/Me` before the films were asked), for each of 10 saved languages (the four of the task: unset, `eng`, `deu`, `OriginalLanguage`; and `ger`, `de`, `DEU`, `fra`, `und`, `xx-not-a-tag`, to see how Jellyfin compares) and both values of `PlayDefaultAudioTrack`. 20 films x 20 cells = 400 cells in `films[].cells`, each with `DefaultAudioStreamIndex` (the string `<absent>` when the key is not in the reply) and the decision (`SupportsDirectPlay`, `SupportsDirectStream`, `SupportsTranscoding`, the `AudioStreamIndex`, `VideoCodec`, `TranscodeReasons` of the `TranscodingUrl`). The same 400 asked with no `DeviceProfile` are in `films[].withoutProfile`.

| Film (`films[].name`) | Audio tracks as ffprobe lists them (`audio`; `*` is the default flag) |
|---|---|
| Audio Def First | 1 eng*, 2 deu |
| Audio Def Second | 1 eng, 2 deu* |
| Audio Def None (no flag) | 1 eng, 2 deu |
| Audio Def All (every track flagged) | 1 eng*, 2 deu* |
| Audio Ger And Deu | 1 eng*, 2 ger, 3 deu |
| Audio Untagged (MKV) | 1 no tag*, 2 eng |
| Audio Und (MP4) | 1 und*, 2 eng |
| Audio Two Same | 1 eng*, 2 deu, 3 deu |
| Audio Single | 1 eng* |
| Audio Third First (no deu, first track in a third language) | 1 fra, 2 eng* |
| Audio None (no audio) | none |
| Audio Short De | 1 eng*, 2 de |
| Audio Region De | 1 eng*, 2 de-DE |
| Audio Deu Default | 1 deu*, 2 eng |
| Audio Dup Second | 1 eng, 2 deu, 3 deu* |
| Audio Three Distinct | 1 eng, 2 deu, 3 fra* |
| Audio Dup First | 1 deu, 2 deu, 3 eng* |
| Audio Dup Middle | 1 eng, 2 deu*, 3 deu |
| Audio Four | 1 eng, 2 fra, 3 spa, 4 deu* |
| Audio Named (also a subtitle at index 4) | 1 eng*, 2 deu, 3 fra |

An MKV file has no `und` tag: ffmpeg leaves the field out, so `und` is an MP4 film.

## What was observed: the index with no AudioStreamIndex in the request

- Jellyfin lists a track's `Language` by its own spelling: the track tagged `ger` is listed `deu` (`films[].listed`, `Audio Ger And Deu`). A track tagged `de` or `de-DE` is listed as it is written.
- A saved language is read by its language, whatever the spelling: `ger`, `de`, `DEU` and `deu` all choose the same tracks. A saved `OriginalLanguage`, `und`, `xx-not-a-tag` and `fra` (where no track is French) match no track, and choose as each other do.
- A **track tagged `de` or `de-DE` is matched by no saved language**: with `deu`, `ger`, `de` or `DEU` saved and `PlayDefaultAudioTrack` false, `Audio Short De` and `Audio Region De` answer 1, the English track, where `Audio Two Same` answers its second track (cells `AudioLanguagePreference` x `PlayDefaultAudioTrack` of those films). So Jellyfin compares the spelling of a track's tag after it has changed `ger` to `deu`.

The rule that fits all 400 cells (checked by `TestTheAudioDefaultCaptureFitsTheRuleWrittenDown`, which holds this rule as code; that test is the proof, the prose is its reading). Let a track's *language* be its tag, with `ger` read as `deu`, and a saved language likewise with `ger` and `de` read as `deu` (no other spelling was seen to differ). A track *matches* when the saved language is set and is its language. The *flagged* tracks are those with the default flag.

1. No audio: no `DefaultAudioStreamIndex`.
2. `PlayDefaultAudioTrack` true and some track flagged: look only at the flagged tracks. The first that matches, else the first flagged. (`Audio Def All` with `deu` saved answers 2, with `eng` or nothing saved 1; `Audio Def First` with `deu` saved answers 1, since track 2 is not flagged.)
3. Otherwise (`PlayDefaultAudioTrack` false, or nothing flagged), with a language saved and some track matching: the first track that matches, flagged or not. (`Audio Dup Second`, `deu`, false: 2, not the flagged 3.) With nothing flagged, `PlayDefaultAudioTrack` true is this rule too (`Audio Def None`, `deu`, true: 2).
4. Otherwise let X be the first flagged track, else the first track. With no language saved the answer is X. With a language saved that matches nothing (`fra` for a film with no French track, `OriginalLanguage`, `und`, `xx-not-a-tag`), the answer is **the first track in X's language** (`Audio Dup Second`, anything unmatched, false: 2, since X is track 3, `deu`, and the first `deu` track is 2; every other film has no earlier track in X's language, so it answers X).

## What was observed: the index when AudioStreamIndex is named

`named` has 90 answers, for `Audio Named` and `Audio Three Distinct` (flagged last), five saved settings (`''` true; `deu` true; `fra` false; `deu` false; `OriginalLanguage` true) and nine requests: `AudioStreamIndex` 1, 2, 3 (audio tracks), 4 (a subtitle, in `Audio Named` only; no stream in the other film), 0 (the video), 99, -1, the string `"2"` (jellyfin-web sends strings on the first call) and none.

- **A named audio track is the answer**, whatever is saved: 1, 2, 3 and `"2"` answer themselves in every setting.
- **A named index that is no audio stream (a subtitle, the video, 99, -1) is ignored, and so is the saved language**: the answer is the file's own default, the flagged track else the first, with no preference applied. `Audio Three Distinct` with `deu` saved and `PlayDefaultAudioTrack` false answers 2 when nothing is named and 3 (the flagged track) when 4, 0, 99 or -1 is.

## What was observed: the item, which jellyfin-web's details page is built from

`details` in the JSON holds `GET /Items/{id}?userId=` for three films and three settings (`''` true; `deu` false; `deu` true). In Jellyfin 12.4 the item's `MediaSources[0].DefaultAudioStreamIndex` is the same answer as PlaybackInfo's for that viewer (`Audio Def First` with `deu` saved and false: 2; with nothing saved, or `deu` and true: 1) and the item lists every audio stream (`audioStreamIndexes` [1, 2]). jellyfin-web 12.4's details page fills its Audio menu from the item's audio streams, selects `DefaultAudioStreamIndex`, and its Play button sends the menu's value as `AudioStreamIndex` (read in `itemDetails.*.chunk.js`: `audioStreamIndex: e.querySelector(".selectAudio").value`; seen in `transcode-12.4.md` too, the first call carries `AudioStreamIndex` "1"). So there the viewer's language reaches PlaybackInfo through the item. A play that starts from a card, with no details page, sends no `AudioStreamIndex`, and PlaybackInfo's own answer decides. jellyscale's item once listed the default audio track alone (DOMAINS invariant 32); since JS-147 (D-018) it lists every audio track as Jellyfin does (audio-menu-12.4.md), and for the details page to start the film in the viewer's language the item is the viewer's too: `GET /Items/{id}` (and its `/Users/{id}/Items/{id}` alias) names, in each source, the `DefaultAudioStreamIndex` that PlaybackInfo names for that viewer, and lists every audio track. The details page's Play then sends it as `AudioStreamIndex`, the named-index rule answers it, and the decision below follows. `internal/api/audio_default_integration_test.go` (`TestTheItemNamesTheTrackPlaybackInfoNamesAndListsEveryTrackForEveryCapturedCell`, `TestPlayOnTheDetailsPageStartsTheFilmInTheViewersLanguage`) and an e2e scenario of `test/e2e/subtitles.mjs` hold it.

## What was observed: a client whose profile has no IsSecondaryAudio condition

`anyClient` and `anyClientNamed` in the JSON are 80 + 6 answers to a `DeviceProfile` that accepts every container and codec for direct play, has one HLS transcoding profile and no `CodecProfiles` at all (the shape of the profile Fluster sends, whose mpv switches the track itself; `anyClientProfile` holds it), for all 20 films, four settings (`''` true; `deu` true; `deu` false; `OriginalLanguage` false) and, with `deu` false saved, `AudioStreamIndex` 1, 2 and 3 for the two named films.

- **Jellyfin direct plays in all 86**: `SupportsDirectPlay` true and no `TranscodingUrl`, whatever index it names (the same index as with Chrome's profile). So the `SecondaryAudioNotSupported` transcode of the section below comes from the `IsSecondaryAudio` condition of Chrome's two `CodecProfiles` (`transcode-12.4.json`, `/deviceProfile/CodecProfiles/0` and `/1`), not from the track being the second. jellyscale does the same: a default that is not the file's turns direct play into a copy only for a profile with such a condition that the track fails, and a client that picks the track itself keeps its direct play with the track named in `DefaultAudioStreamIndex` and pays no worker slot.

## What was observed: the decision

- When `DefaultAudioStreamIndex` is not the first audio track of the file, Jellyfin offers no direct play: `SupportsDirectPlay` and `SupportsDirectStream` false and a `TranscodingUrl` whose `AudioStreamIndex` is the answer, `TranscodeReasons=SecondaryAudioNotSupported`, `VideoCodec=av1,h264,vp9` (the profile's list; the video copy is decided when the stream starts). When it is the first audio track, direct play, no `TranscodingUrl`. It makes this decision on the *first track*, not the flagged one: `Audio Def Second` (flagged 2), nothing saved, answers 2 and is not direct played; `Audio Def First` with `deu` saved and false answers 2, transcoded.
- The one exception seen: with nothing flagged (`Audio Def None`) and `PlayDefaultAudioTrack` true, a saved `deu`, `ger`, `de` or `DEU` answers 2 and Jellyfin still direct plays (the browser then plays track 1, not the track the reply names). That is a Jellyfin fault, not a rule for jellyscale: DOMAINS invariant 45's direct play is of the file's default track, the flagged one else the first, which is what a browser plays.
- With no `DeviceProfile`: always direct play, no `TranscodingUrl`, and the same index as with a profile in 392 of the 400 cells. In the other 8, all in `Audio Dup Second` with an unmatched language saved and `PlayDefaultAudioTrack` false, Jellyfin answers the flagged track 3 where rule 4 answers 2. jellyscale answers a request with no profile as it answers one with a profile (the task's criterion), so these 8 are a difference in the other direction.
- `Audio None` answers no `DefaultAudioStreamIndex` and direct play in every cell.

## What jellyscale does, and where it differs

jellyscale follows rules 1 to 4 and the named-index rule, comparing languages with `playback.SameLanguage`, with `OriginalLanguage` read as the language of the film's default audio track (JS-117; a film whose default audio names no language answers as if none were saved), and with a saved `und`, `mis` or a value naming no language read as no language saved. The decision follows invariant 45: a copy, not a direct play, when the answer is not the flagged track else the first (what a browser plays), so the cells above where Jellyfin direct plays track 1 while naming 2 become a copy here, and `Audio Def Second` with nothing saved, which Jellyfin transcodes, direct plays here (its default track is the answer). The differences in the index, each listed in `the named exceptions held in the replay test` under the name `audio default: <film> | <saved language> | PlayDefaultAudioTrack <value>` (replayed by `internal/api/audio_default_integration_test.go`):

1. **Tag spellings.** `Audio Short De` and `Audio Region De`, with `deu`, `ger`, `de` or `DEU` saved and `PlayDefaultAudioTrack` false: Jellyfin 1 (it matches no track tagged `de` or `de-DE`), jellyscale 2 (the track is German). Eight cells.
2. **A saved value that names no language.** `Audio Dup Second` with `und` or `xx-not-a-tag` saved and `PlayDefaultAudioTrack` false: Jellyfin 2 (rule 4), jellyscale 3 (as if nothing were saved). Two cells.
3. `OriginalLanguage` is a language here (the original of the film) where Jellyfin has none without a provider; in every one of the 400 cells the two give the same index.
