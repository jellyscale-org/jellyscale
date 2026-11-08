# Setting up jellyfin/jellyfin:12.4 for the differential tier

Observed 2026-11-07 on jellyfin/jellyfin:12.4 (server 12.4.0), a disposable container on a Docker-assigned port (`-p 127.0.0.1::8096`), the synthetic fixtures from `test/fixtures` mounted read-only at `/media/movies` and `/media/shows`. No real library.

Startup takes about 5 s before `/System/Info/Public` answers (`StartupWizardCompleted: false`). Wizard, all unauthenticated:

| Step | Request | Answer |
| --- | --- | --- |
| 1 | `POST /Startup/Configuration` `{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}` | 204 |
| 2 | `GET /Startup/User`, then `POST /Startup/User` `{"Name":"admin","Password":"..."}` | 200 `{"Name":"root"}`, 204 |
| 3 | `POST /Startup/RemoteAccess` `{"EnableRemoteAccess":true}` | 204 |
| 4 | `POST /Startup/Complete` | 204 |
| 5 | `POST /Users/AuthenticateByName` `{"Username":"admin","Pw":"..."}` with an `Authorization: MediaBrowser Client=..., Device=..., DeviceId=..., Version=...` header | 200, `AccessToken` and `User.Id` |

Libraries, with `Authorization: MediaBrowser Token="...", Client=...`:
`POST /Library/VirtualFolders?name=Movies&collectionType=movies&refreshLibrary=false` with `{"LibraryOptions":{"PathInfos":[{"Path":"/media/movies"}]}}` gave 204; the same for `Shows` with `tvshows` and `refreshLibrary=true` started the scan. Polling `GET /Items?Recursive=true&IncludeItemTypes=Movie,Episode` showed `TotalRecordCount` 1, then 6 (3 films, 3 episodes) within about 6 s; a library is ready when that count is final and `/ScheduledTasks` shows no running scan.

What differs on its face from jellyscale, which the normalizer must absorb:
- Ids are 32 hex digits without dashes; `ServerId` is the server's own id. `UserData.Key` carries the item id with dashes. Ids also appear inside `MediaSources[].Id`, `Path`-like URLs and image tags.
- `Path` is the path inside the container (`/media/movies/Alpha Film (2021)/...`), `DateCreated` is the scan time, `Etag` and `MediaSources[].ETag` are hashes.
- `GET /UserItems/Resume?mediaTypes=Video` after a progress report returned the film; `mediaTypes=Audio` and `mediaTypes=Book` returned `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`. That is the route and the parameters of PR #32.
- `POST /UserItems/{id}/UserData` `{"PlaybackPositionTicks":100000000}` gave 200 with `PlayedPercentage` 33.3 and (quirk) `Played` true.
