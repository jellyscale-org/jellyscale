# What Jellyfin 12.4 does with chapter images

Observed, not read from source. Server: `jellyfin/jellyfin:12.4`, a disposable container with a 60 s Matroska film with three chapters in a library for each setting; evidence in `chapter-images-12.4.json`. What jellyfin-web does with the images is in `chapters-12.4.md`.

## The option, and when pictures are made

- Library options `EnableChapterImageExtraction` and `ExtractChapterImagesDuringLibraryScan`, **both false by default** (`GET /Library/VirtualFolders`).
- Both on: the pictures exist when the library scan ends. Only the first on: the scan makes none, and the scheduled task `RefreshChapterImages` ("Extract Chapter Images", daily at 20:00, at most four hours) makes them; run by hand, it had made all three within 15 s for this film.
- Neither: no pictures, no `ImageTag`.

## The item

- A chapter with a picture has `ImagePath` (a server path), `ImageDateModified` (when it was made) and `ImageTag` (32 hex characters, one per chapter). A chapter without one has no `ImagePath` and no `ImageTag`.
- The pictures are JPEGs at the film's own size (640x360), one file per chapter named for its start in ticks.

## The route

`GET /Items/{id}/Images/Chapter/{index}` (index from 0, the position in `Chapters`):

- `maxWidth` resizes (400 gave 400x225, 100 a smaller one); `quality` is taken. Without them the stored picture is sent whole.
- With `tag` the answer is `Cache-Control: public, max-age=31536000, immutable` and `ETag` set to the tag. Without it `Cache-Control: public` only. **The tag is not compared with anything**: a made-up one gets the picture.
- No index (`/Images/Chapter`) answers with the first picture. An index past the last chapter, a negative one, and any index for a film with no pictures are 404.

## What jellyscale does, and where it differs

1. The option is `chapterImages` in a library's config, **off by default**, one switch instead of Jellyfin's two. Pictures are made by a worker job after every scan, never inline in it, so there is no scheduled task and no setting for "during the scan". Jellyfin's two options' defaults agree.
2. `ImageTag` is 32 hex characters, a hash of the picture's asset key, which has the file's ETag in it. There is no `ImagePath` (a path on the server means nothing to a client) and `ImageDateModified` stays `0001-01-01T00:00:00.0000000Z`; jellyfin-web uses neither.
3. Pictures are stored at most 400 pixels wide (the width jellyfin-web asks for), and **the stream role does not resize**: `maxWidth` and `quality` are ignored, as they are for the other item images, because resizing needs a decoder or ffmpeg on a role that has neither. A client asking for a different width gets the 400 pixel picture.
4. Same routes and status codes, and the same `Cache-Control` (`public, max-age=31536000, immutable` with a tag, `public` without). **No `ETag` header is sent**, where Jellyfin sends the tag as one; the URL carries the tag, and the stored picture of a URL never changes, so a client revalidates nothing it needs to. No index is the first picture, out of range and negative are 404, and the tag is not compared (any tag gets the current picture, which has the file's ETag in its key).
