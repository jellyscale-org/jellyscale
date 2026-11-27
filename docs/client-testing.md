# Testing a client against jellyscale

A client that tests against jellyscale can pin an image that holds a small synthetic library and one known user, and start it with one Compose file. Nothing in it is a real film. It is for tests only: the password is public, the only user is an administrator, and nothing is kept when it stops.

## The image

`ghcr.io/jellyscale-org/jellyscale` has two kinds of tag, each with a `-fixtures` twin that holds the library:

| Tag | Names | Made |
| --- | --- | --- |
| `X.Y.Z` and `X.Y.Z-fixtures` | a release, such as `0.3.0` | when the owner pushes the tag `vX.Y.Z` (no `v` in the image tag) |
| `main-<12 hex>` and `main-<12 hex>-fixtures` | the first 12 digits of a commit of `main` | for every commit that lands on `main` |

No tag moves: a tag names one image for good, and a version is never published twice. There is no `latest`, `main`, `edge` or `X.Y`. Pin an `X.Y.Z-fixtures` tag, or a `main-<12 hex>-fixtures` tag to test against a commit. The tag without `-fixtures` is the server alone, the same binary, ffmpeg and user, with no library and no user. Both images carry `org.opencontainers.image.revision` (the commit), `.source` and `.version` labels. Neither holds a web client: `GET /web/` answers 404.

## Start it

Copy [`deploy/fixtures/compose.yaml`](../deploy/fixtures/compose.yaml) next to your tests, as `compose.yaml`. It starts Postgres, the image, and a SeaweedFS bucket that a one-shot `seed` service of the image fills before the server starts, with no build, volume or file besides itself:

```sh
export JELLYSCALE_IMAGE=ghcr.io/jellyscale-org/jellyscale:X.Y.Z-fixtures
docker compose up -d --wait
port=$(docker compose port jellyscale 8096 | sed 's/.*://')   # Docker picks it; the server listens on 127.0.0.1 only
```

The same command starts a `main-<12 hex>-fixtures` tag with `JELLYSCALE_IMAGE` set to it. The seed service runs on each `up` and copies only what the bucket lacks, so a second `up` copies nothing. Two projects of the file can run side by side. `docker compose down -v` removes everything. With `JELLYSCALE_IMAGE` unset or empty, Compose stops and names it.

Sign in as user `test` with password `jellyscale-fixtures` (`POST /Users/AuthenticateByName` with `{"Username": "test", "Pw": "jellyscale-fixtures"}`). That user is an administrator and the only one; a second, non-administrator user is not there yet. The password is in the compose file, not the image: the image refuses to start without `JELLYSCALE_ADMIN_PASSWORD`.

## Wait for the scan

`up --wait` returns when `/readyz` answers 200, which is before the scan is done: the libraries are still being read, and a first request can see none or some of the films. Wait until the counts are the documented ones, with a time limit:

```sh
base=http://127.0.0.1:$port
auth='X-Emby-Authorization: MediaBrowser Client="test", Device="test", DeviceId="test", Version="1"'
token=$(curl -s -H "$auth" -H 'Content-Type: application/json' -d '{"Username":"test","Pw":"jellyscale-fixtures"}' $base/Users/AuthenticateByName | jq -r .AccessToken)
count() { curl -s -H "$auth, Token=\"$token\"" "$base/Items?Recursive=true&IncludeItemTypes=$1&Limit=500" | jq .TotalRecordCount; }
for _ in $(seq 60); do   # 60 seconds, then give up
  [ "$(count Movie)" = 31 ] && [ "$(count Episode)" = 6 ] && break
  sleep 1
done
```

The library has 26 films and 3 episodes. The S3 libraries hold five copies of films and a copy of the show Delta Show with its three episodes (see Libraries), so 31 films and 6 episodes in all. test/image.sh waits the same 60 seconds.

## What is in it

Item ids are looked up by name (list the items as above and take the one whose `Name` is exactly the film's) and are not promised to stay the same between tags. Each name below finds exactly one item.

### The request

Every track list below is what `POST /Items/{id}/PlaybackInfo?UserId={user}` answers, in `MediaSources[0].MediaStreams`, for this request body (a browser that plays H.264 with AAC as it is, and transcodes anything else to fMP4 HLS with H.264 and AAC; it does not play AC3):

```json
{
  "DeviceProfile": {
    "MaxStreamingBitrate": 120000000,
    "DirectPlayProfiles": [
      {
        "Container": "mp4,m4v,mkv",
        "Type": "Video",
        "VideoCodec": "h264",
        "AudioCodec": "aac"
      }
    ],
    "TranscodingProfiles": [
      {
        "Container": "mp4",
        "Type": "Video",
        "AudioCodec": "aac",
        "VideoCodec": "h264",
        "Context": "Streaming",
        "Protocol": "hls",
        "MaxAudioChannels": "2",
        "MinSegments": "1",
        "BreakOnNonKeyFrames": false
      }
    ],
    "SubtitleProfiles": [
      {
        "Format": "vtt",
        "Method": "External"
      },
      {
        "Format": "ass",
        "Method": "External"
      },
      {
        "Format": "ssa",
        "Method": "External"
      }
    ]
  }
}
```

Each track is `- <index>: <Type> <language> <codec>`, and `, default` (audio only), `, forced` or `, external` (a file beside the film) when set. `Index` is the position in the list, from 0. A language of `none` is a track with no language tag. A film that says `Plays: direct` answers with no `TranscodingUrl` for this request, and its file is served as it is, with byte ranges (`GET /Videos/{id}/stream?static=true&mediaSourceId={source}` with a `Range` header answers 206). A film that says `Plays: transcode` answers a `TranscodingUrl` whose master playlist leads to H.264 and AAC segments made inside the container by its ffmpeg. A film that says `Plays: transcode, copying the video` (Tau Film) answers a `TranscodingUrl` that says `VideoCodec=copy`. Babel Film's text tracks answer as WebVTT: `GET /Videos/{id}/{source}/Subtitles/{index}/Stream.vtt` begins `WEBVTT`, and its one cue says which track it is (`Babel track 5` for track 5), so a client can tell that it got the track it asked for.

## The films without the image

`test/fixtures/client.sh <dir>` makes the same files under `<dir>`: it needs ffmpeg, Go (one recipe is a Go program) and a serif font, and exits non-zero, saying what failed, when a film is missing. Point libraries of your own at the folders `movies`, `shows`, `transcode` and `languages` under it. The recipes of the two language films are `test/fixtures/babel.sh` and `test/fixtures/kino.sh`, each of which runs alone. The HDR film is made by `codec_film` in `test/fixtures/codec.sh`, which the e2e films share; the HEVC film with a PGS track is in `test/fixtures/make.sh`, and it needs ffmpeg with libx265.

## Libraries

- Movies (movies)
- Shows (tvshows)
- Transcode (movies)
- Languages (movies)
- S3 (movies), in the bucket: copies of five films, each renamed with ` S3` after its title (Alpha Film S3, Tau Film S3, Pgs Hevc Film S3, Resh Film S3 and Babel Film S3). Each copy lists the tracks of its film and plays as the film does.
- S3 Shows (tvshows), in the bucket: the copy of Delta Show, Delta Show S3, with the episodes Pilot S3, Second S3 and Return S3 (see Episodes).

## Episodes

One series, Delta Show, with two seasons. Each name finds one episode. The S3 Shows library holds a copy of the series, Delta Show S3, whose episodes are the same three under the names Pilot S3, Second S3 and Return S3; each of them plays as it is.

- Pilot
- Return
- Second

## Films

### Alpha Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac

### Babel Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac, default
- 2: Audio deu aac
- 3: Audio fra aac
- 4: Audio none aac
- 5: Subtitle eng subrip
- 6: Subtitle eng subrip, forced
- 7: Subtitle eng subrip
- 8: Subtitle deu subrip
- 9: Subtitle ger subrip, forced
- 10: Subtitle fra subrip
- 11: Subtitle none subrip
- 12: Subtitle jpn PGSSUB
- 13: Subtitle deu subrip, external
- 14: Subtitle spa subrip, external

### Burn Sub Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac
- 2: Subtitle eng subrip, external
- 3: Subtitle jpn ass, external

### Cracked Fonts Film

Plays: direct

- 0: Video none h264
- 1: Audio none aac
- 2: Subtitle jpn ass

### Direct Sub Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac
- 2: Subtitle eng subrip, external
- 3: Subtitle jpn ass, external

### Dvb Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac, default
- 2: Subtitle eng DVBSUB

### Font Film

Plays: direct

- 0: Video none h264
- 1: Audio none aac
- 2: Subtitle jpn ass

### Gamma Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac

### Idx Beside Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac
- 2: Subtitle en DVDSUB, external

### Kino Film

Plays: direct

- 0: Video none h264
- 1: Audio deu aac, default
- 2: Audio eng aac
- 3: Subtitle deu subrip
- 4: Subtitle eng subrip
- 5: Subtitle fra subrip

### Orphan Beside Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac

### Pgs Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac, default
- 2: Subtitle eng PGSSUB

### Pgs Hevc Film

Plays: transcode

- 0: Video none hevc
- 1: Audio eng aac, default
- 2: Audio deu ac3
- 3: Subtitle eng PGSSUB

The client does not play HEVC, so the picture is re-encoded to H.264. A request that picks track 2 (`AudioStreamIndex=2`) and track 3 (`SubtitleStreamIndex=3`) gets a `TranscodingUrl` that names both, whose first segment holds H.264 and AAC, with track 3 burnt into the picture.

### Pgs Long Cue Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac, default
- 2: Subtitle eng PGSSUB

### Pgs Transcode Film

Plays: transcode

- 0: Video none h264
- 1: Audio eng ac3, default
- 2: Subtitle eng PGSSUB

### Phi Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac
- 2: Subtitle eng subrip
- 3: Subtitle jpn ass

### Plain Film

Plays: direct

- 0: Video none h264
- 1: Audio none aac
- 2: Subtitle jpn ass

### Resh Film

Plays: transcode

- 0: Video none hevc
- 1: Audio eng ac3

The picture is HDR10 (PQ, BT.2020, 10 bit). A client that does not show HDR gets it tone-mapped to SDR BT.709: its first segment is tagged BT.709.

### Sigma Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac
- 2: Subtitle eng subrip
- 3: Subtitle jpn ass

### Sup Beside Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac
- 2: Subtitle eng PGSSUB, external

### Sup Beside Transcode Film

Plays: transcode

- 0: Video none h264
- 1: Audio eng ac3
- 2: Subtitle eng PGSSUB, external

### Tau Film

Plays: transcode, copying the video

- 0: Video none h264
- 1: Audio eng ac3
- 2: Subtitle eng subrip
- 3: Subtitle jpn ass

### The Beta Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac, default

### Transcode Sub Film

Plays: transcode

- 0: Video none h264
- 1: Audio eng ac3
- 2: Subtitle eng subrip, external
- 3: Subtitle jpn ass, external

### Vanishing Fonts Film

Plays: direct

- 0: Video none h264
- 1: Audio none aac
- 2: Subtitle jpn ass

### Vobsub Film

Plays: direct

- 0: Video none h264
- 1: Audio eng aac, default
- 2: Subtitle eng DVDSUB

