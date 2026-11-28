# What Jellyfin 12.4 opens with no token

Observed with `jellyfin/jellyfin:12.4` (server version 12.4.0), a disposable container on a Docker-assigned port over synthetic films made by a real Jellyfin 12.4: a film, an episode of a show, a film with a text subtitle and an attached font, a film with an image subtitle (PGS), and a film with three chapters whose pictures were extracted. Two users: an administrator and a non-administrator (`viewer`, made with `POST /Users/New`). Evidence in `access-12.4.json`.py compat/captures/access-12.4.json` from the repository root.

Every route of `compat/endpoints.yaml` is asked with three credentials, each in one spelling per row: no credential at all (none of the seven spellings; a redirect would be recorded as its 3xx and not followed), a token the server never issued, and the non-administrator's token. Each row asks for something the server has. `GET /Users/Me` with the non-administrator's token answered 200 before and 200 after the sweep: `POST /Sessions/Logout` and the sign-in were asked with tokens of their own, so no later row is a 401 of a signed-out token.

## What the media routes sent

| Route | No token | Unknown token | Non-administrator |
|---|---|---|---|
| download (`GET /Items/{id}/Download`) | 401, refused | 401, refused | 200, same-bytes |
| poster (`GET /Items/{id}/Images/Primary`) | 200, non-empty | 200, non-empty | 200, non-empty |
| chapter picture (`GET /Items/{id}/Images/Chapter/0`) | 200, non-empty | 200, non-empty | 200, non-empty |
| segment (`GET /Videos/{id}/hls1/main/0.mp4`) | 401, refused | 401, refused | 200, non-empty |
| main playlist (`GET /Videos/{id}/main.m3u8`) | 401, refused | 401, refused | 200, non-empty |
| master playlist (`GET /Videos/{id}/master.m3u8`) | 401, refused | 401, refused | 200, non-empty |
| static stream (`GET /Videos/{id}/stream`) | 200, same-bytes | 200, same-bytes | 200, same-bytes |
| static stream head (`HEAD /Videos/{id}/stream`) | 200, no-body | 200, no-body | 200, no-body |
| stream.mkv (`GET /Videos/{id}/stream.mkv`) | 200, non-empty | 200, non-empty | 200, non-empty |
| font (`GET /Videos/{id}/{id}/Attachments/3`) | 200, same-bytes | 200, same-bytes | 200, same-bytes |
| text subtitle (`GET /Videos/{id}/{id}/Subtitles/2/Stream.vtt`) | 200, non-empty | 200, non-empty | 200, non-empty |
| image subtitle (`GET /Videos/{id}/{id}/Subtitles/2/Stream.sup`) | 200, non-empty | 200, non-empty | 200, non-empty |
| text subtitle from an offset (`GET /Videos/{id}/{id}/Subtitles/2/10000000/Stream.vtt`) | 200, non-empty | 200, non-empty | 200, non-empty |

`same-bytes` is byte for byte the film file or the font; `non-empty` a body of the route's kind; `refused` a status that is not 2xx; `no-body` a HEAD answer.

## Open with no token

The rows Jellyfin answers 2xx or 3xx with no credential at all:

- `GET /Branding/Configuration`
- `GET /Branding/Css`
- `GET /Branding/Css.css`
- `GET /Items/{id}/Images/Chapter/0`
- `GET /Items/{id}/Images/Primary`
- `GET /QuickConnect/Enabled`
- `GET /System/Info/Public`
- `GET /System/Ping`
- `GET /Users/Public`
- `GET /Videos/{id}/stream.mkv?VideoCodec=h264&AudioCodec=aac`
- `GET /Videos/{id}/stream?static=true`
- `GET /Videos/{id}/{id}/Attachments/3`
- `GET /Videos/{id}/{id}/Subtitles/2/10000000/Stream.vtt`
- `GET /Videos/{id}/{id}/Subtitles/2/Stream.sup`
- `GET /Videos/{id}/{id}/Subtitles/2/Stream.vtt`
- `HEAD /Videos/{id}/stream?static=true`
- `POST /System/Ping`

## Refused to the non-administrator

The rows Jellyfin answers 401 or 403 to the non-administrator's token, each with the reason in `access-12.4.json`:

- `GET /Plugins`

## Other findings

- `GET /Plugins`: Jellyfin 12.4 lists plugins to administrators only: 403 to the non-administrator.
- `GET /socket`: 101, not a refusal: Jellyfin takes the WebSocket upgrade for the non-administrator's token (in Authorization or ApiKey) and answers 403 with none; a plain GET with the token is 404.
- `POST /Users/AuthenticateByName`: no_token is 400, not 401: with no Authorization header at all the request names no device, which Jellyfin's sign-in needs; a token (even one never issued) in the header gets the sign-in served.
- A token that was never issued got the same status as no token on every row but ['POST /Users/AuthenticateByName'].
- Not observed here: any signed-in user reading every account (`GET /Users`, `UserController.cs:91` and `:127` at tag v12.4); that is read from source.
