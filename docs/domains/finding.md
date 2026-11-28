# Finding

## Words

- **Home.**
- **My list.**
- **Collection kind:** franchise in watch order, a made list, a saved rule.
- **Filter.**

## Owns

Every read of titles for a viewer: Films, Shows, details, search, filters, lists and the change log. Owns (later tables) `my_list`, `collections` and `catalog_changes`. Today `internal/items`, which becomes `internal/catalog`.

Reads only through the viewer-scoped handle (rule 70): `catalog.For(viewer)` is the only read handle Finding, Place and Playback get. Lists, counts, first-letter positions, search, Continue watching and next episode all go through it. Collection and Making use an unscoped handle that the edge cannot import.

## Must not

Read a title without a viewer; write a title; rank by other people's viewing. Never hide by what other people watch, and never show what the viewer may not see, counts included.

## Rules

Rules at the cutover (JS-194); today's invariants apply until then.

- **Rewritten 6, at the cutover (invariant 6 is unchanged until then):** Filtering and sorting use indexed columns. Answers are shaped at the edge from stored neutral facts. Today the response is shaped in memory from `items.dto`.
