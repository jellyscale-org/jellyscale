# Contract (the edge)

## Words

- **Capability:** a group of operations the contract declares. One group per domain.
- **Reason:** plain words for a refusal or a conversion, from one list (JS-183). Each reason has an owning domain. `not-served` from an older replica during a roll means "not yet". The playback id and a viewer's reason are what the operator searches for.
- **Action:** what an app may do next, such as `start-again` (rule 77).

## Owns

HTTP, the OpenAPI contract, the one refusal writer and conformance. No tables.

Calls flow one way: Contract → {Finding, Playback, Place, People, Together} → Collection's types. Playback calls Making and Place. Today: `internal/api`, `internal/compat` and `compat/`. `internal/api` becomes the edge only, and domain logic moves out as each operation is built. `internal/compat`, `compat/` and `internal/quickconnect` go at the cutover; approval mechanics move to People.

## Must not

Hold domain logic or SQL.

## Rules

Rules at the cutover (JS-194); today's invariants apply until then.

- **74.** Every answer and refusal is declared in the contract, with a reason from one list (JS-183).
- **Rewritten 61, at the cutover (invariant 61 is unchanged until then):** Every operation declares its access in the contract; no content is open, pictures included. Pictures by id need a sign-in (D-20261127-pictures-signin).
- **Removed at the cutover (JS-194):** invariants 16 to 20, and the socket and jellyfin-web parts of 31, 32, 37 and 43. Tables only Jellyfin's API used are listed in docs/ for removal one release later.

Also decided: additions are compatible. An app ignores fields, reasons, actions and events it does not know. A refused Play is answered with a refusal status and its reason, never inside a successful answer (JS-217).
