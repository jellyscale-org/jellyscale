# What Jellyfin 12.4 gives the details page's Audio menu, and what jellyfin-web does with a pick (JS-147)

Observed 2026-11-20, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0, image id `fea863d0cd9e`), a disposable container on a Docker-assigned port, no metadata provider (as in `audio-default-12.4.md`); the web client is the one that image serves at `/web/`, driven by Chrome 157 (it plays H.264 and AAC). Made and asked by a real Jellyfin 12.4 and the browser part of the capture; the answers, cut to the fields below, are in `audio-menu-12.4.json` (same directory), which `compat/audio_menu_capture_test.go` holds to this text. The films are 20 second 640x360 H.264 files with AAC mono tracks, one sine tone per track (track n is 440 + 110 (n - 1) Hz: 440, 550, 660, 770), synthetic names, mounted read-only. This note adds to `audio-default-12.4.md`, whose section "the item" records that the item lists every audio stream, that the menu selects `DefaultAudioStreamIndex` and that Play sends the menu's value.

| Film (`films[].name`) | Audio tracks as ffprobe lists them (`*` is the default flag) |
|---|---|
| Menu Babel (the audio of `test/fixtures/babel.sh`) | 1 eng*, 2 deu, 3 fra, 4 no tag |
| Menu Four | 1 eng, 2 deu, 3 no tag*, 4 deu |
| Menu Mono | 1 eng* |
| Menu Silent | none |

Two viewers, each a user made for the case with the settings saved as jellyfin-web saves them: **none** (no `AudioLanguagePreference`, `PlayDefaultAudioTrack` true) and **German** (`deu`, `PlayDefaultAudioTrack` false, which is what leaves a flagged track that is not theirs).

## The item (`items` in the JSON): every audio stream, whoever asks

`GET /Items/{id}?userId=` lists every audio stream, in the file's order, in `MediaStreams` and in `MediaSources[0].MediaStreams` (`itemAudioIndexes` is the first list). `MediaSources[0].DefaultAudioStreamIndex` is the viewer's track (`audio-default-12.4.md`'s rules).

| Film | Audio streams: Index, DisplayTitle (IsDefault) | DefaultAudioStreamIndex, none viewer | German viewer |
|---|---|---|---|
| Menu Babel | 1 `English - AAC - Mono - Default` (true), 2 `German - AAC - Mono`, 3 `French - AAC - Mono`, 4 `AAC - Mono` | 1 | 2 |
| Menu Four | 1 `English - AAC - Mono`, 2 `German - AAC - Mono`, 3 `AAC - Mono - Default` (true), 4 `German - AAC - Mono` | 3 | 2 |
| Menu Mono | 1 `English - AAC - Mono - Default` | 1 | 1 |
| Menu Silent | none | absent | absent |

The two German tracks of Menu Four have the same label; a track with no tag has none in its label. jellyscale's scanner writes the same labels (`displayAudio`), so the e2e scenario reads Menu Babel's from the JSON.

## The menu (`web.menus`): jellyfin-web's `.selectAudio`

Filled from the item's audio streams, **sorted with the default track first and the rest by Index** (Menu Four's entries are 3, 1, 2, 4), each option's text its `DisplayTitle`, the viewer's `DefaultAudioStreamIndex` selected (Menu Four: 3 for the none viewer, 2 for the German one).

- Menu Babel: entries 1, 2, 3, 4 in that order; 1 selected for the none viewer, 2 for the German one. Shown (`.selectAudioContainer` not hidden) and enabled.
- Menu Mono (one audio track): **shown and disabled**, its one entry selected.
- Menu Silent (no audio track): **hidden** (`hide` on the container) and disabled, no entries.

(jellyfin-web 12.4, `itemDetails` chunk: `disabled` when the item lists fewer than two audio streams, the container hidden when it lists none.)

## What a pick does (`web.picks`)

Menu Babel, a user made for each case, Play pressed on the details page after `selectOption`.

| Viewer | Selected before | Picked | AudioStreamIndex sent | Reply's DefaultAudioStreamIndex | How it plays | Heard | Player's audio menu selects |
|---|---|---|---|---|---|---|---|
| German | 2 | 3 (neither selected nor the file's default) | `"3"` | 3 | transcode (`SecondaryAudioNotSupported`, `TranscodingUrl` AudioStreamIndex 3, VideoCodec `av1,h264,vp9`), no direct play | 659 Hz: track 3 | 3 |
| German | 2 | 1 (the file's default) | `"1"` | 1 | direct play, no `TranscodingUrl` | 439 Hz: track 1 | 1 |
| none | 1 | 3 | `"3"` | 3 | the same transcode | 659 Hz | 3 |

The request carries the menu's value as a string, as `audio-default-12.4.md` says. No error is shown; jellyfin-web logs `HLS Error: otherError internalException Fatal: false` once for each transcode (not fatal, playback goes on).

**On return**, Jellyfin remembers the pick for that viewer and film: the details page read 1.5 s after leaving the player still selected the old entry (the page held the item it had), and the same page loaded again, and a new browser signed in by the same viewer, select the picked one (3 for the first and third cases, 1 for the second; `onReturn`, `afterReload`, `afterSignIn`). It is kept by the server with the viewer's data for the film (the player's progress reports carry the `AudioStreamIndex`), so it is the viewer's, not the browser's. A second viewer is not affected. jellyscale's spec does not ask for it: the task's criteria end at the pick playing and the player's menu showing it.

## What jellyscale does, and where it differs

The item lists every audio stream and names the viewer's track as Jellyfin does (JS-147, D-018; before it listed the chosen track alone, DOMAINS invariant 32). Where Jellyfin transcodes a pick that is not the first track (`SecondaryAudioNotSupported`), jellyscale copies the video with that track (invariant 45), and plays the file's own default directly. The states where the item cannot list a track that can be delivered are the product owner's (criterion 4 of the task).
