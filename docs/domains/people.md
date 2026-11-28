# People

## Words

- **Account:** who signs in; `users` rows (today's accounts). Every account gets a default profile.
- **Profile:** a person, the holder of a history. Every account gets a default profile whose id is the account's, in storage only and never in the contract. A profile carries its listening and reading language, subtitle mode, kept languages, the two hiding switches and "show every track". Today these are `users.configuration` and `users.track_preferences`, with their meaning unchanged; setting them through the contract arrives with JS-157. A pick for a title lives with the place (JS-157).
- **Device.**
- **Invitation.**
- **Circle:** household, friends, community, in that order of priority. A person's limits never leak into a group.
- **Limits:** hours and streams at once. Playback enforces them (rule 75).
- **Access rule:** per account, `see: all` or a list of marks; a rating ceiling; exceptions that add or remove single titles. Read with the request; nothing is cached per account.
- **Mark:** a named access mark on a title, a fact about the title and never about where its file is. A title gets a mark from its own metadata file (`access: [kids]`, which moves with the film), from configuration by naming the title, or from configuration by a selector over a closed list of title facts (kind, rating, genre, year, original language). Collection stores marks on the title, indexed; a show's marks reach its seasons and episodes. Configuration with an unknown key is refused at start.
- **Hidden:** a track marked for the calling profile. One pure function, `playback.TrackFilter`, serves Finding's details and Playback's plans (rule 62).

## Owns

Who someone is and what they may see, and when. Owns the tables `users` (accounts), new `profiles`, `devices`, `signin_*`, `device_approvals` (today's `quickconnect_codes`) and new `access_rules`. Today `internal/signin`, becoming `internal/people`; approval mechanics move here at the cutover. People owns the access rule; `catalog.For(viewer)` (Finding's package) turns it into the one condition every title read uses (rule 70).

## Must not

Query titles. Put a credential in a cookie readable by scripts, or in a link. Decide access anywhere but here: access is decided once (rule 70). Name a place where films are kept, or a directory (rule 78).

## Rules

Rules at the cutover (JS-194); today's invariants apply until then.

- **70.** No title is read for a viewer except through the access-scoped handle. Access is re-read on every request. Every media request checks, in the statement that finds its playback, that the rule still holds, so a title hidden mid-film is refused at the next segment on any replica (JS-036). Pictures need a signed-in viewer and go through the handle. Each member's playback passed the rule itself. The outage exception stays (invariant 37). A change acts at once, because the rule is read with the request.
- **78.** Access rules read only marks, rating and title id. No configuration, metadata file or rule names a place films are kept or a directory; configuration refuses unknown keys at start; moving a film changes no answer of `TestAccess` (JS-036). An unmarked title is seen only by accounts that see all.
- **Rewritten 62, at the cutover (invariant 62 is unchanged until then; rewritten, not removed):** A profile that asked has tracks outside its languages marked hidden in details and plans, by one function; nothing is removed, written or made for it. Never hidden: the original language (invariant 60), the profile's and kept languages, tracks with no language, a named track, and video. Only details and plans are marked; nothing is written, extracted or hashed for it; a hidden track plays when named. No placeholder is sent: ids replace positions, so nothing shifts. "Show every track" is the profile's switch on the server, so every app agrees. Saved settings keep applying from the cutover.
- **Rewritten 63, at the cutover (invariant 63 is unchanged until then):** A native app's credential travels in a header. Octave Web's session is a cookie that scripts cannot read; it is never accepted on media routes, and JS-215 protects it. A media link carries no account credential. The web sign-in is accepted on picture routes only, whose answers are marked `private` so no shared cache keeps them, and on no other media request (a stream, a segment, a subtitle, a font) or media link. Pictures by id need a sign-in (rewritten 61).
- **26, kept:** sign-in bounds and per-device credentials. No sign-in credential is stored in the clear (JS-184).
- **Rewritten 65 and 66, at the cutover (names only):** TV approval keeps the code-and-secret bounds of invariant 65; the mechanism is unchanged.
