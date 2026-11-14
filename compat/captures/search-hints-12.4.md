# What Jellyfin 12.4 does with GET /Search/Hints

Observed 2026-11-13 on `jellyfin/jellyfin:12.4` (server 12.4.0), a disposable container on a Docker-assigned port, over synthetic films and shows only; the answers are in `search-hints-12.4.json` (same directory). Fluster (#102) sends `searchTerm`, `limit`, `includeItemTypes=Movie,Series,Episode` and `userId`, and no `recursive`, `parentId` or `fields`; the capture uses that request.

Library: five films (two named `Quartz ...`), `Delta Show` (a `poster.jpg` beside it; S01E01, S01E02 and S02E01) and `Quartz Show` (no picture; one episode `S01E01 Quartz Opener`). Jellyfin with no metadata provider names an episode by its whole file name.

## What it answers

- `200` `{"SearchHints": [...], "TotalRecordCount": n}`. A hint is `ItemId`, `Id` (the same), `Name`, `Type`, `MediaType`, `RunTimeTicks`, `Artists` (`[]`), `ChannelId` (`null`), and where the item has them `ProductionYear`, `PrimaryImageTag`, `PrimaryImageAspectRatio`, `IsFolder` (a series), `Series` (an episode's series name), `IndexNumber` and `ParentIndexNumber` (an episode's number and its season's).
- A hint is one item whose name holds the term, whatever the case (`uart` and `QUA` find `Quartz ...`). A film, a series and an episode are each a hint. `includeItemTypes` narrows the hints to the types named: `Series` gave the series and none of its episodes.
- `PrimaryImageTag` is the tag `GET /Items/{id}` gives in `ImageTags.Primary` (the capture's `items`, for a film, an episode and a series with a poster). `Quartz Show` has no picture and its hint has no `PrimaryImageTag` key.
- An episode hint carries its own season and number: `S02E01 Return` is `ParentIndexNumber` 2, `IndexNumber` 1; `S01E02 Second` is 1 and 2.
- `limit` cuts the matches and the count is the cut: 4 matches with `limit=3` gave 3 hints and `TotalRecordCount` 3; `limit=1` gave 1 and 1; `limit=10` gave 4 and 4. (`startIndex` is taken after that cut: `limit=3&startIndex=3` gave no hints and 3.)
- A term that matches nothing is `200` with `SearchHints` `[]` and `TotalRecordCount` 0. `limit=0` and `limit=-1` also answer 200 with `[]` and 0.
- The order of the hints on these fixtures: episodes, then films, then the series, by name within each (`Quartz Show S01E01 Quartz Opener`, `Quartz Film`, `Quartz Quest`, `Quartz Show`). An episode's name here begins with its `SxxEyy`, so episodes come in season and episode order (`Delta Show S01E01 Pilot`, `S01E02 Second`, `S02E01 Return`); jellyscale names episodes by title, so it must order them by season and number to answer the same. On the one-letter term `a` the order is not by type (a film, three episodes, a film, a series, ...), so it is a relevance order, not a rule this capture explains.
- No `searchTerm` and an empty one both answer `400` (a validation problem: `The searchTerm field is required.`), and never the library.
- No token and a token no device holds both answer `401` with no body.
- An unknown `userId` of the right shape is served as if it were the caller's; `userId=zzz` is `400`.

The answers pass `compat/conform` as `SearchHintResult` (11 responses, no violation).
