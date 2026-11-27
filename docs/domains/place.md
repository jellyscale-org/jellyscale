# Place

## Words

- **Place:** one history per person and title: where the viewer stopped, and what "played", "continue" and "next episode" mean.
- **Played.**
- **Continue watching:** titles in progress and each show's next unplayed episode. Taking a title off sets `dismissed_at`; it returns when watched again. Taking it off keeps the place.
- **Next episode.**

## Owns

One history per person and title, and what "played", "continue" and "next episode" mean. Owns the table `user_item_data`; only Place writes it. The key is `(profile, title)`; the version watched is recorded for the operator only. A pick for a title lives with the place (JS-157), under the same key. Playback reports (profile, title, position, when, final) and Place applies its rules; Playback writes no history rules of its own. Today `internal/playstate`, which gains continue watching and next episode.

Jellyfin import (JS-060): titles are matched by path, and history is written through the dated-write rule, so a second run changes nothing.

## Must not

Know devices, playbacks or HTTP. Show one person another's history.

## Rules

Rules at the cutover (JS-194); today's invariants apply until then.

- **73.** One history per profile and title; only Place writes it.
- **Rewritten 28 to 30 and 49, at the cutover (names only):** Place's rules, unchanged, fed by playback reports.
