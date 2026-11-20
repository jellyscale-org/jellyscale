# Testing a client against jellyscale

A client that tests against jellyscale (Fluster, for one) can pin an image that holds a small synthetic library and one known user, and start it with one Compose file. Nothing in it is a real film. It is for tests only: the password is public, the only user is an administrator, and nothing is kept when it stops.

## The image

`ghcr.io/jellyscale-org/jellyscale` has two kinds of tag, each with a `-fixtures` twin that holds the library:

| Tag | Names | Made |
| --- | --- | --- |
| `X.Y.Z` and `X.Y.Z-fixtures` | a release, such as `0.3.0` | when the owner pushes the tag `vX.Y.Z` (no `v` in the image tag) |
| `main-<12 hex>` and `main-<12 hex>-fixtures` | the first 12 digits of a commit of `main` | for every commit that lands on `main` |

No tag moves: a tag names one image for good, and a version is never published twice. There is no `latest`, `main`, `edge` or `X.Y`. Pin an `X.Y.Z-fixtures` tag, or a `main-<12 hex>-fixtures` tag to test against a commit. The tag without `-fixtures` is the server alone, the same binary, ffmpeg and user, with no library and no user. Both images carry `org.opencontainers.image.revision` (the commit), `.source` and `.version` labels. Neither holds a web client: `GET /web/` answers 404.

## Start it

Copy [`deploy/fixtures/compose.yaml`](../deploy/fixtures/compose.yaml) next to your tests, as `compose.yaml`. It starts Postgres and the image, with no build, volume or file besides itself:

```sh
export JELLYSCALE_IMAGE=ghcr.io/jellyscale-org/jellyscale:X.Y.Z-fixtures
docker compose up -d --wait
port=$(docker compose port jellyscale 8096 | sed 's/.*://')   # Docker picks it; the server listens on 127.0.0.1 only
```

Two projects of the file can run side by side. `docker compose down -v` removes everything. With `JELLYSCALE_IMAGE` unset or empty, Compose stops and names it.

Sign in as user `test` with password `test` (`POST /Users/AuthenticateByName` with `{"Username": "test", "Pw": "test"}`). That user is an administrator and the only one; a second, non-administrator user is not there yet. The password is in the compose file, not the image: the image refuses to start without `JELLYSCALE_ADMIN_PASSWORD`.

## Wait for the scan

`up --wait` returns when `/readyz` answers 200, which is before the scan is done: the libraries are still being read, and a first request can see none or some of the films. Wait until the counts are the documented ones, with a time limit:

```sh
base=http://127.0.0.1:$port
auth='X-Emby-Authorization: MediaBrowser Client="test", Device="test", DeviceId="test", Version="1"'
token=$(curl -s -H "$auth" -H 'Content-Type: application/json' -d '{"Username":"test","Pw":"test"}' $base/Users/AuthenticateByName | jq -r .AccessToken)
count() { curl -s -H "$auth, Token=\"$token\"" "$base/Items?Recursive=true&IncludeItemTypes=$1&Limit=500" | jq .TotalRecordCount; }
for _ in $(seq 60); do   # 60 seconds, then give up
  [ "$(count Movie)" = 24 ] && [ "$(count Episode)" = 3 ] && break
  sleep 1
done
```

The library has 24 films and 3 episodes. test/image.sh waits the same 60 seconds.

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

Each track is `- <index>: <Type> <language> <codec>`, and `, default` (audio only), `, forced` or `, external` (a file beside the film) when set. `Index` is the position in the list, from 0. A language of `none` is a track with no language tag. A film that says `Plays: direct` answers with no `TranscodingUrl` for this request, and its file is served as it is, with byte ranges (`GET /Videos/{id}/stream?static=true&mediaSourceId={source}` with a `Range` header answers 206). A film that says `Plays: transcode` has AC3 audio: it answers a `TranscodingUrl` whose master playlist leads to H.264 and AAC segments made inside the container by its ffmpeg. Babel Film's text tracks answer as WebVTT: `GET /Videos/{id}/{source}/Subtitles/{index}/Stream.vtt` begins `WEBVTT`, and its one cue says which track it is (`Babel track 5` for track 5), so a client can tell that it got the track it asked for.

## The films without the image

`test/fixtures/client.sh <dir>` makes the same files under `<dir>`: it needs ffmpeg, Go (one recipe is a Go program) and a serif font, and exits non-zero, saying what failed, when a film is missing. Point libraries of your own at the folders `movies`, `shows`, `transcode` and `languages` under it. The recipes of the two language films are `test/fixtures/babel.sh` and `test/fixtures/kino.sh`, each of which runs alone.

## Libraries

- Movies (movies)
- Shows (tvshows)
- Transcode (movies)
- Languages (movies)

## Episodes

One series, Delta Show, with two seasons. Each name finds one episode.

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

Plays: transcode

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

