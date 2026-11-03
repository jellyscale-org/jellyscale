# What Jellyfin 12.4 starts a viewer with

Recorded as the baseline for starting on a default subtitle, which is not built: the server chooses no default subtitle, and nothing in jellyscale stores the viewer's subtitle mode or language. Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0). The exchanges are in `subtitles-default-12.4.json` (same directory). Nineteen 6 second Matroska films were scanned, each with an English audio track unless the table says otherwise, embedded SubRip tracks flagged as listed, and subtitle files beside them (`Film.<words>.srt`). For each film, each subtitle mode and each language preference (none, `eng`, `deu`), `POST /Items/{id}/PlaybackInfo?UserId=...` with body `{}` after `POST /Users/{id}/Configuration`. A new user's configuration is `SubtitleMode: Default` and `SubtitleLanguagePreference: ""`, which is every jellyscale user's.

## What was observed (no language preference)

Jellyfin numbers the files first; the table is in its numbering. `-1` means `DefaultSubtitleStreamIndex` was absent.

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

## The rule that fits

Order the tracks: files first, then those flagged default, then those not forced, ties by index. Then:

- `None`: none.
- `Default`: the first track that is a file, or is flagged default or forced. A film with no files and no flags starts with none (B3). A file flagged `default` beats a plain file (C1), a file beats an embedded track flagged default (A5, C2), a track flagged default beats a plain one inside the film (B1).
- `Always`: the first track that is not forced, else the first forced one (C5).
- `OnlyForced`: the first forced track.
- `Smart` agrees with `Default` for most of these films but not B3 and B4 (it gives a plain track where `Default` gives none or a forced one); it was not reduced to a rule and jellyscale does not build it.

This rule is what a slice that builds it must reproduce; no code or test of jellyscale encodes it yet.

With a language preference the answers depend on it (for `eng`, the plain English file of A3 wins over the forced one for `Default`, and `Always` gives none when no track is in the language), as the JSON shows. Nothing in jellyscale stores a preference, so that is not built either.

## How jellyfin-web reads the number

`DefaultSubtitleStreamIndex` is read in two ways (web-dist, `main.jellyfin.bundle.js` and `htmlVideoPlayer-plugin`): as an `Index` (`isDefault: l.Index === DefaultSubtitleStreamIndex` when it builds the external text tracks, and the details page's `e.Index === a`), and as a position in `MediaStreams` (`MediaStreams[DefaultSubtitleStreamIndex]` for the stream's `DeliveryMethod`, which the player sets the subtitle off for when there is no stream there or it is `Encode`). So an index past the end of the list switches the subtitle off in the player although the menu shows it selected.
