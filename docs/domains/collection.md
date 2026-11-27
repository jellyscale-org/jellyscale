# Collection

## Words

- **Title:** a film, show, season or episode a person recognises (today's `items` row and id). No id changes, so history and stored assets need no migration.
- **Version:** one file of a title (today's `media_sources` row). It has an id of its own only when a title has a second file (JS-050). Assets and specs key on source id and ETag.
- **Track:** an audio or subtitle track of one version, named by its track id (rule 72).
- **Cut:** a different edit; another title, linked to its film.
- **Place films are kept:** a library, with a media store and an asset store. A library's id survives a rename.

## Owns

What exists: finding files, naming, probing and keeping identity. Owns the tables `items` (titles), `media_sources` (versions), `libraries` (places), `scan_objects` and new `title_aliases`. Today `internal/library` and `internal/media`. Jellyfin field names leave at the edge, not in storage.

## Must not

Serve requests; decide access; know a viewer.

## Rules

Rules at the cutover (JS-194); today's invariants apply until then.

- **72.** A track id names one track of one file revision or nothing. A pick is stored as a description.
  The track id is opaque, made from the version's revision and the track's index (sidecar files keep their never-reused counter, invariant 50). An id from a replaced file is refused with a reason; it never silently names another track.
  A pick is not a track id: it must follow the viewer to the next episode and to another version, where ids differ. It is stored as a description (kind, language, forced, for the deaf, commentary, title) per profile and per title or show, and resolved to a track at Play (JS-157).
- **Rewritten 5, at the cutover (invariant 5 is unchanged until then):** A title's id survives renames, rescans and absence; a version is one file of a title; merging titles leaves an alias. Readers see titles only through `shown_items`.
  Merging two files into one title (JS-050) keeps the older id; the other becomes a `title_aliases` row, so cached ids resolve. Histories merge by Place's staleness rule (the newer `data_at` wins; played is either).
