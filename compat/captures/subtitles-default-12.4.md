# What Jellyfin 12.4 starts a viewer with: the default subtitle (JS-041)

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0), a disposable container on a Docker-assigned port (`-p 127.0.0.1::8096`), no metadata provider, 2026-11-23. The first 19 films (`A1` to `C6`) were recorded earlier; `test/differential/capture_subtitles_default.py` made them again, asked them again (every one of their 285 recorded answers came back the same, so the fixtures are the same films), and added 16 films and the questions below. The answers are in `subtitles-default-12.4.json` (same directory); `test/differential/capture_subtitles_default_web.mjs` is the part that drives jellyfin-web in Chromium. Films are 6 second 160x90 Matroska files with synthetic names. Subtitle files beside a film are `Name (2020).<language>[.default][.forced].srt` (or `.sup`); tracks inside are SubRip or PGS (`test/fixtures/pgs`) with the language and the default and forced flags set by ffmpeg. `A` films are mostly files, `B` tracks inside, `C` a mix; `S` films have two audio tracks or three subtitle languages, `G` a track tagged `ger` or `deu`, `U` an untagged track, `P` a PGS track, `X` a forced and a plain track in one language, `R` is one film twice.

## What was asked

`POST /Items/{id}/PlaybackInfo?UserId=` for each film, with body `{}` (the first capture's; `films[].results[].DefaultSubtitleStreamIndex`, null when the field is absent) and with jellyfin-web's body and the `DeviceProfile` of `transcode-12.4.json` (`withProfile`; its `SubtitleProfiles` are `vtt`, `ass` and `ssa`, all `External`, so PGS rendering is off). The viewer's `SubtitleMode` (5) and `SubtitleLanguagePreference` (`''`, `eng`, `deu`) were saved with `POST /Users/{id}/Configuration` and read back from `GET /Users/Me`: 35 films x 15 = **525 cells**, each with both answers. A new user's configuration is `SubtitleMode: Default`, `SubtitleLanguagePreference: ""` and `RememberSubtitleSelections: true`.

- `smart`: Smart with each `SubtitleLanguagePreference` x each `AudioLanguagePreference` (`''`, `eng`, `jpn`) on 7 films (B3, B4, A7, S1, S2, A1, C3) with `PlayDefaultAudioTrack` off, and on S1 and S2 with it on too: 81 cells, each with the audio track that plays. A saved audio language changes the track only where `PlayDefaultAudioTrack` is off (`audio-default-12.4.md`): S1 (audio 1 eng flagged, 2 jpn) plays 2 with `jpn` saved and S2 (3 jpn flagged, 4 eng) plays 4 with `eng` saved, then.
- `named`: `SubtitleStreamIndex` named in the request (each track of the film, `-1`, `99`, and none) on A5 (a file and two tracks inside), P2 (SubRip and PGS) and B3, for each mode and for `''` and `eng`: 160 answers.
- `items`: `GET /Items/{id}?userId=` on 7 films, each mode and language: 105 answers, each beside PlaybackInfo's.
- `remember`: a play reported with `POST /Sessions/Playing`, `/Progress` and `/Stopped` naming a `SubtitleStreamIndex` (3, `-1`, 2), with `RememberSubtitleSelections` on and off, then PlaybackInfo and the item asked for the same viewer and film, the same viewer's other film and another viewer's same film.
- `german`: `GET /Localization/Cultures` for German, and what jellyfin-web's Subtitles page saves when German is picked.

## The numbering

Jellyfin numbers the subtitle files first (in file name order, case ignored: `Film.deu.srt`, `Film.eng.forced.srt`, `Film.eng.srt`), then the file's own streams after them, each at its ffprobe index plus the number of files (A5: 0 a file, video 1, audio 2, subtitles 3 and 4). jellyscale numbers the film's own streams as ffprobe does and the files after them (`internal/library/sidecars.go`, `nextIndex`), in the same file name order. The rules below are over the tracks in Jellyfin's order; a track is the same track in both by what it is: a file or inside the film, its language, its flags, and its place among the files or among the film's own tracks. So Jellyfin's 3 in A5 (the first track inside) is the first track inside in jellyscale, and its 0 the first file. A test compares a number through that map, never the raw number. A subtitle's `Language` is as the file wrote it except that `ger` is listed `deu` (as for audio), so a viewer who saved `deu` starts a track tagged `ger` (G1, G3). A track with no tag lists no `Language` and is in no language: a saved `eng` or `deu` never starts it (U1, U2), a saved `''` may.

## The rule that fits

**Every one of the 606 cells (the 525 of `films` and the 81 of `smart`) is given by the rules below; none is an exception.** The rules are read into `testenv.JellyfinSubtitleDefault` and replayed against the cells by the checks that hold jellyscale to the capture (`internal/playback`, `internal/api`), which fail on any cell whose answer the rules do not give. Let *L* be the saved `SubtitleLanguagePreference`; a track is *in L* when its language is L (`ger` read as `deu`); with L empty every track is in L. *Audio* is the language of the audio track that plays (`DefaultAudioStreamIndex`).

*The order* of tracks, first to last: files before the film's own tracks; then those flagged default; then those in L (only when L is saved); then not forced before forced; then by number. One clause: with L saved and **no track of the film in L**, forced comes before not forced among files (A4 with `eng`, X3 with `eng` and with `deu`: a forced and a plain file in a language that was not saved), where in every other cell not forced is first. Only files can show it, since in Default a plain track inside the film is not eligible.

### None

No default track, whatever else.

### Default

The first track in the order that is a file, or is flagged default, or is forced. A film with no files and no flags has none (B3). A file flagged default beats a plain file (C1), a file beats a track inside flagged default (A5, C2: with `deu` saved, A5's French file still wins over the German track flagged default), a track flagged default beats a plain one inside the film (B1). The language only decides among tracks equal in the rest (A1 with `eng`: the English file; A3 with `eng`: the plain English file over the forced one). The viewer's language does not exclude a track: with `eng` saved, A8 starts its Spanish default file, and 7 of the 19 first films (A2, A4, A5, A8, B1, C4, C5) start a track that is not English.

### Always

Among the tracks in L (all of them when L is empty): the first in the order that is not forced; if there is none, the first that is forced (C5 with `''` and `deu`: its one forced file; C3 with `deu`: the forced German file; C4 with `eng`: the forced English track inside the film, not the German file before it). With L saved and nothing in L, none (A4 with `eng`; A6, A7, A8, A9, B4, C6, U1, U2, P1 with `deu` or `eng` where the film lacks it).

### OnlyForced

The first forced track in L (any forced track when L is empty); none when no forced track is in L (A4 with `eng`, C5 with `eng`, P3 with `eng`, A3 with `deu`). A plain track is never started.

### Smart

With L empty: the first track of the whole film in the order, plain or forced (B3: the English track inside, which Default leaves out; B4: the plain track, not the forced one Default starts; C3: the forced German file; P1: the PGS track). With L saved: when *Audio* is not L, the first track in L that is not forced (S1 with `jpn` playing and `eng` saved: the English track inside; with `eng` playing: none); and where that finds none, or *Audio* is L, the first forced track in L (B4 with `eng`: the forced English track; B3 with `eng`: none; C3 with `eng`: the forced English file). So one film answers differently when the saved audio language changes the audio that plays: S1 with `eng` saved starts the English subtitle with `jpn` as `AudioLanguagePreference` and `PlayDefaultAudioTrack` off (audio 2), and none with `''` or `eng`; S2 (files deu and eng) with `eng` saved starts none while its English audio plays (`eng`, off) and the English file otherwise.

### Cells the rules do not give

None of the 606 cells. Unobserved, so not in the rules and not to be filled in by a guess: Smart where the audio track that plays names no language; a saved language other than `''`, `eng` and `deu` (a spelling such as `ger` or `de` saved as the subtitle language, which for audio is read as `deu`; jellyfin-web saves `deu`, below); a track flagged both default and forced; a film with several audio tracks in the language of the saved subtitle.

## PlaybackInfo with the DeviceProfile

`DefaultSubtitleStreamIndex` is the same with the profile and with no profile in all 525 cells. With the profile:

- A default that is a **text** track, inside the film or a file, changes no method: `SupportsDirectPlay` and `SupportsDirectStream` stay true, there is no `TranscodingUrl`, and the track's `DeliveryMethod` is `External` (241 cells).
- A default that is an **image** track the profile does not draw (PGS, inside the film or a `.sup` file beside it, with PGS rendering off) makes the reply a transcode: `SupportsDirectPlay` false, a `TranscodingUrl` with `SubtitleStreamIndex={default}`, `SubtitleMethod=Encode` and `TranscodeReasons=SubtitleCodecNotSupported`, and the track's `DeliveryMethod` is `Encode` (22 cells, P1 to P4). A film that direct played with no default direct plays still when the default is text. The cells whose default is an image track:

| Mode | `''` | `eng` | `deu` |
|---|---|---|---|
| Default | P2, P3 | P2, P3, P4 | P2, P3 |
| Always | P1, P2, P3 | P1, P3, P4 | P2, P3 |
| OnlyForced | P3 | none | P3 |
| None | none | none | none |
| Smart | P1, P2, P3 | none | P2, P3 |

- `MediaStreams[DefaultSubtitleStreamIndex]` is the track, with `Index` equal to its position, in every cell that names one (`AtPosition`).
- The audio track in `TranscodingUrl` is `DefaultAudioStreamIndex`'s.

## SubtitleStreamIndex named in the request

With the profile (jellyfin-web's body), the answer is the number named, **any** number: a track of the film (inside, a file, PGS) in every mode including None, `-1` (what the details page's Off sends: answered `-1`) and `99` (answered `99`, which is no stream: jellyfin-web then draws no subtitle). With no profile the number named is ignored and the mode's default answers (the 160 answers hold both). With nothing named, the answer is the mode's default above. jellyscale answers `-1` for a number that is no listed Subtitle stream at that position, so `99` is a difference, and a request with no profile that names a track gets that track in jellyscale: both are listed in `compat/differences.yaml` as `subtitle default: named 99` and `subtitle default: named with no profile`.

## GET /Items/{id}

The item carries `MediaSources[].DefaultSubtitleStreamIndex` for the viewer's mode and language exactly as PlaybackInfo does in all 105 answers (absent where PlaybackInfo's is absent), and lists the same subtitle tracks (`subtitleIndexes`). jellyfin-web's details page selects it in the Subtitles menu and its Play sends it as `SubtitleStreamIndex`, so the track the viewer starts with is the item's. jellyscale's item must say the same, and so must its alias `GET /Users/{id}/Items/{id}`.

## What a progress report changes (RememberSubtitleSelections)

Observed on R1 and R2 (one film twice) with mode Always and no language (default 2): with `RememberSubtitleSelections` **on**, a `POST /Sessions/Playing/Progress` that names `SubtitleStreamIndex` 3 changes that viewer's next default for that film to 3 (PlaybackInfo and the item), naming `-1` changes it to `-1`, naming 2 (what it already was) changes nothing; `Start` changes nothing and `Stopped` leaves it as `Progress` made it; no other film of the same viewer (R2) and no other viewer of the film is changed. With it **off**, no report changes anything. jellyscale does not remember a selection: its default is the mode's rule applied to the film, and a progress report changes nothing. That is not built and not in this task.

## Which language code jellyfin-web saves for German

`GET /Localization/Cultures` lists German as `Name: German`, `TwoLetterISOLanguageName: de`, `ThreeLetterISOLanguageName: deu`, `ThreeLetterISOLanguageNames: [deu, ger]`. jellyfin-web 12.4's Subtitles page (`#/mypreferencessubtitles`) lists 495 languages, one named `German` whose value is **`deu`**, and saving it stores `SubtitleLanguagePreference: deu` (`german.savedByTheSubtitlesPage`). A track tagged `ger` is listed `deu` (G1, G3), one tagged `deu` is `deu` (G2): both start when `deu` is saved.

## How jellyfin-web reads the number

`DefaultSubtitleStreamIndex` is read in two ways (web-dist, `main.jellyfin.bundle.js` and `htmlVideoPlayer-plugin`): as an `Index` (`isDefault: l.Index === DefaultSubtitleStreamIndex` when it builds the external text tracks, and the details page's `e.Index === a`), and as a position in `MediaStreams` (`MediaStreams[DefaultSubtitleStreamIndex]` for the stream's `DeliveryMethod`, which the player sets the subtitle off for when there is no stream there or it is `Encode`). So an index past the end of the list switches the subtitle off in the player although the menu shows it selected, and a build must answer only `-1` or a number that is both: a listed Subtitle stream at its own position.

## What jellyscale does

Each mode's rule above, over the tracks the reply lists, for a viewer who has saved a Configuration. A viewer who never posted one is answered `-1`, as today; a viewer who saved mode Default and language `''` gets the Default rule's answers, where Jellyfin's new user gets them without saving, a difference listed in `compat/differences.yaml` as `subtitle default: a viewer who never saved a Configuration`. Cells where jellyscale answers another track than this capture are named there as `subtitle default: <film> | <mode> | <language>` with a reason; a cell it answers as captured is not listed.

## The first 19 films

Recorded earlier and asked again with no language preference; `-1` means `DefaultSubtitleStreamIndex` was absent. The table is in Jellyfin's numbering.

| Film | Audio | Subtitles (index language flags) | Default | Always | OnlyForced | None | Smart |
|---|---|---|---|---|---|---|---|
| A1 | eng | 0 deu (file), 1 eng (file) | 0 | 0 | -1 | -1 | 0 |
| A2 | eng | 0 deu default (file), 1 eng (file) | 0 | 0 | -1 | -1 | 0 |
| A3 | eng | 0 deu (file), 1 eng forced (file), 2 eng (file) | 0 | 0 | 1 | -1 | 0 |
| A4 | eng | 0 deu forced (file), 1 deu (file) | 1 | 1 | 0 | -1 | 1 |
| A5 | eng | 0 fra (file), 3 eng, 4 deu default | 0 | 0 | -1 | -1 | 0 |
| A6 | eng | 0 eng (file), 3 eng forced | 0 | 0 | 3 | -1 | 0 |
| A7 | jpn | 0 eng (file), 1 jpn (file) | 0 | 0 | -1 | -1 | 0 |
| A8 | eng | 0 spa default (file), 1 spa forced (file) | 0 | 0 | 1 | -1 | 0 |
| A9 | eng | 0 eng default (file), 3 eng, 4 eng default | 0 | 0 | -1 | -1 | 0 |
| B1 | eng | 2 eng, 3 deu default | 3 | 3 | -1 | -1 | 3 |
| B2 | eng | 2 eng default, 3 deu | 2 | 2 | -1 | -1 | 2 |
| B3 | eng | 2 eng, 3 deu | -1 | 2 | -1 | -1 | 2 |
| B4 | eng | 2 eng forced, 3 eng | 2 | 3 | 2 | -1 | 3 |
| C1 | eng | 0 deu (file), 1 eng default (file) | 1 | 1 | -1 | -1 | 1 |
| C2 | eng | 0 eng (file), 3 eng, 4 deu default | 0 | 0 | -1 | -1 | 0 |
| C3 | eng | 0 deu forced (file), 1 eng forced (file), 4 eng | 0 | 4 | 0 | -1 | 0 |
| C4 | eng | 0 deu (file), 3 eng forced, 4 deu default | 0 | 0 | 3 | -1 | 0 |
| C5 | eng | 0 deu forced (file) | 0 | 0 | 0 | -1 | 0 |
| C6 | eng | 2 eng default, 3 eng forced | 2 | 2 | 3 | -1 | 2 |
