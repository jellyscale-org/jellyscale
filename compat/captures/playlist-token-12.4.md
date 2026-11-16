# Where a real Jellyfin 12.4 puts the token in its playlists

Recorded for JS-128 (a playlist carries the token only when the request carried it in the URL). Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0), a 20 second synthetic H.264 + AAC Matroska film, one user. Exchanges are in `playlist-token-12.4.json` (same directory), the token replaced by `<TOKEN>`.

`POST /Items/{id}/PlaybackInfo` (the captured jellyfin-web profile, `EnableDirectPlay=false&EnableDirectStream=false`) answered a `TranscodingUrl` with `ApiKey=<token>` in it. Two cases, each asking `master.m3u8`, then `main.m3u8` at the URI the master's one line gave, then the init segment and segments at the URIs main lists:

| Case | Master asked as | Token in the master's line | Token in main's lines (MAP URI and each segment) |
|---|---|---|---|
| header | TranscodingUrl minus its `ApiKey` pair, `Authorization: MediaBrowser ... Token="<token>"` on every request | none | none |
| query | TranscodingUrl as given, no header | `ApiKey=<token>` | `ApiKey=<token>` once on every URI line |

- Jellyfin writes the same lines in both cases except for the one pair: `ApiKey=<token>` sits between `PlaySessionId` and `TranscodingMaxAudioChannels`, and with a header-only request that pair is absent, with nothing left behind (no empty `ApiKey=`).
- Both answers are 200 `application/vnd.apple.mpegurl`; main has an init (`#EXT-X-MAP`) and 4 segments for 20 s.
- The URIs of the header playlist, asked with the same header: init and segments 200 `video/mp4`. The same URIs with no header and no token: 401, empty body.
- The URIs of the query playlist asked with no header: 200 `video/mp4`, the same bytes as with the header (the query token is what authenticates).
- Jellyfin's lines follow the request: asked with the header alone, no line carries a token.

Not observed: a request with the same token in both places, a wrong token, `api_key`, and the other header spellings (the check for jellyscale holds them to the query and header rules above).
