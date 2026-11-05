# What Jellyfin 12.4 does with chapters

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` with jellyfin-web 12.4 driven by Chrome, evidence in `chapters-12.4.json` (same directory). Fixtures: 60 s films (640x360, 24 fps, keyframe every 48 frames) with three chapters made by ffmpeg from an ffmetadata file: `Opening` at 0 s, `Middle Part` at 20 s and one with no title at 45.5 s. In Matroska and in MP4, with and without chapter image extraction (library options `EnableChapterImageExtraction` and `ExtractChapterImagesDuringLibraryScan`), and with the streams starting 2 s late (`-itsoffset 2`), and one with AC3 audio, which Chrome's profile transcodes (video copied). The metadata providers renamed two of the films; names are in the JSON.

## The item

- `Chapters` is in `GET /Users/{u}/Items/{id}` and, in a list, only with `Fields=Chapters` (a list without it has no `Chapters` key).
- Each chapter is `{StartPositionTicks, Name, ImageDateModified}`, plus `ImagePath` and `ImageTag` when its image was extracted. Without images: no `ImageTag`, no `ImagePath`, `ImageDateModified` is `0001-01-01T00:00:00.0000000Z`.
- The untitled chapter is named `Chapter 3` (its 1-based place in the file). Titles come as the file has them.
- The start is the file's own time in ticks, for MP4 and Matroska alike, and **not moved by the streams starting late**: a chapter at 20 s of a film whose streams start at 2 s is `200000000`.
- `RunTimeTicks` of the late film is 620210000 (the container's duration), which is not changed by chapters.
- `MediaSources[]` carries no chapters; PlaybackInfo's `MediaSources` has no `Chapters` key either.

## What jellyfin-web 12.4 does

Read from `itemDetails` and `playback-video` chunks and observed:

- **Details page.** It builds the scene list from `Chapters` only `if (chapters.length && chapters[0].ImageTag)`; otherwise the section (`#scenesCollapsible`) stays hidden. Without images it shows no chapter list and requests no chapter image. With images it shows three cards and requests `/Items/{id}/Images/Chapter/{n}?maxWidth=400&tag=...&quality=90` for n = 0, 1, 2 (only with a tag).
- **Player.** For more than one chapter the OSD shows the previous and next chapter buttons and the seek bar has a marker for each chapter (`.sliderMarker`); the seek bar's hover bubble names the chapter, with its image only if it has an `ImageTag`. This is the same with and without images.
- **Next chapter** (`playbackManager.nextChapter`) seeks to the first chapter whose `StartPositionTicks` is after the current position; **previous** goes to the last chapter starting at or before the position minus 10 s. They use the ticks as they are: after next-chapter and 8 s of playback the player was at 27.9 s in every case, direct play (MKV, MP4, late start) and a transcode (video copied, streams starting 2 s late and not), a second press at 53.4 s.
- Requests: no chapter image request without images; with images only the three above. No other request is made for chapters.

## What jellyscale does, and where it differs

Same item fields, names, ticks and absent `ImageTag`/`ImagePath` (`TestProbeReadsChapters`, `TestChaptersAreInTheDetailsAndInListsThatAskForThem`, and `test/e2e`'s chapter scenarios, which also check the player's markers, buttons and seeks in direct play and in a re-encode and a copy with streams starting late). Differences:

1. Chapter images are not built, so the item never has `ImageTag`, `ImagePath` or a real `ImageDateModified`, even in a library an operator would want them in. Jellyfin's options for extracting them have no effect here.
2. QuickTime chapter tracks (`tref/chap`) are read through ffprobe's chapters; Jellyfin was not run on one.
