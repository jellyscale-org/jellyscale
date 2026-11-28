# Platform

Not a domain. Shared infrastructure that every domain uses; no domain owns it.

## Words

- **Role:** what a process does, chosen by `--roles`: `api`, `stream` or `worker`. One binary, many Deployments.
- **Store:** a POSIX or S3-compatible backend, reached only through `internal/store`.
- **Hint:** a NOTIFY wakeup. Lossy by design.

## Owns

`db` (pool and migrations), `store` (backend differences), `metrics`, configuration and roles. Their boundaries in DOMAINS.md are unchanged.

## Must not

Hold domain logic. `internal/db` holds no queries for other domains, and `internal/store` leaks no backend type or credential. Processes share state only through Postgres rows, hints and the stores (invariant 1). There is no event bus: the two logs, `events` and `catalog_changes`, serve clients.

## Rules

Platform has no new rule (68 to 78). Its rules are today's invariants 1 to 4, 7 to 15, 27 and 42 in DOMAINS.md, unchanged until they move here at JS-093b, after JS-194. Roles and ARCHITECTURE's seven rules stay as they are, and so do stores, their conformance suite, S3 read resume, migration discipline (invariant 8) and hints (invariants 9 and 10).
