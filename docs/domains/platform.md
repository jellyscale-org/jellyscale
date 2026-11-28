# Platform

Not a domain. What every domain uses and no domain owns: the database, the stores, metrics, configuration and roles. Every task reads this file.

## Words

- **Role:** what a process does: `api`, `stream` or `worker` (ARCHITECTURE.md).
- **Store:** a POSIX or S3-compatible backend, reached only through the store interface.
- **Hint:** a wakeup sent through Postgres. It may be lost.

## Must not

Hold domain logic. Leak a backend's types or credentials.

## Rules

1. No process keeps state another replica needs. What a process holds in memory is safe to lose.
2. Anything a replica does at startup is safe when every replica does it at the same instant.
3. Work that must happen once uses a lease or a lock.
4. Processes share state only through Postgres and the stores.
5. Migrations are forward-only and an applied one is never edited. A removal ships one release after the code stops using it, so a rolling upgrade never breaks a replica still on the old code.
6. Client input reaches SQL only as bound parameters. Sort keys come from an allowlist.
7. Every consumer of hints also polls, so a lost hint only adds delay. A hint is never sent inside a data transaction.
8. Local disks and object storage behave the same at the store interface. One conformance suite proves it against both kinds.
9. Store credentials come from the environment. A stored or logged location never holds a secret, and one that does is refused at startup.
10. A path built from a key can't leave its store's root.
11. Nothing fails silently: every failure is reported with its reason.
12. A design states what it costs in network, storage and database as well as CPU, at the scale PRODUCT.md names.
