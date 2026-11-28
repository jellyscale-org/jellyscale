# Collection

What exists: finding files, naming them, reading what a file holds, and keeping identity.

## Words

- **Title:** a film, show, season or episode a person recognises.
- **Version:** one file of a title.
- **Track:** an audio or subtitle track of one version.
- **Cut:** a different edit of a film. It is another title, linked to its film.
- **Place films are kept:** a location the operator declares, on local disk or in object storage. Viewers never browse one.

## Must not

Serve requests; decide access; know a viewer.

## Rules

1. A title's identity survives a rename, a rescan and its file being absent for a while. Its history and everything made from it stay with it.
2. Several files of one film are one title. Joining two titles leaves the older identity, and the other still resolves to it.
3. A track's identity names one track of one revision of a file, or nothing. After a file is replaced, an old identity is refused with a reason; it never silently names another track.
4. A viewer's pick of a track is stored as a description (kind, language, forced, for the deaf, commentary), so it follows them to the next episode and to another version.
5. A place that can't be reached is never taken for an empty one: no title is removed because storage was missing.
6. What the collection knows about a file comes from the file and the files beside it, which always win. TMDB fills what they lack only when the operator turns it on (D-20261128-outside-metadata).
7. Collection records facts about a title. It records nothing about who may see it beyond the marks People reads.
