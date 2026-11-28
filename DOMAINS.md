# Domains

The parts of jellyscale, what each one owns, and the rules a change must keep. Reviewers check changes against this file and the domain files under `docs/domains/`.

- These are rules for the new server. They describe what must hold, not code that exists ([docs/status.md](docs/status.md) says what is built).
- A rule is not a decision. Only the files in `tasks/decisions/` are decisions. A change that needs a rule to bend changes the rule in the same change and says why; when a viewer or operator gives something up, it cites a decision.
- Facts about the carried-over video engine are in [docs/engine/](docs/engine/README.md). They are facts about code, not product rules.

## The domains

Seven domains and one edge. A domain owns its data: only its package writes it. A task names its domains and its agents read only those files, plus Platform.

| Domain | Responsible for | Words | Must not |
|---|---|---|---|
| [Collection](docs/domains/collection.md) | What exists: finding files, naming, reading what a file holds, keeping identity | title, version, track, cut, place films are kept | Serve requests; decide access; know a viewer |
| [Finding](docs/domains/finding.md) | Every read of titles for a viewer: Films, Shows, details, search, filters, lists | home, my list, collection, filter | Read a title without a viewer; write a title; rank by other people's viewing |
| [Playback](docs/domains/playback.md) | One viewing from Play to stop: the decision, the plan, the link, steps, track switches, refusals, limits, moves | playback, plan, step, media link, reason | Run media tools; write history rules |
| [Making](docs/domains/making.md) | Everything made from a film: segments, extracted tracks, fonts, pictures; jobs, workers, capacity, the storage budget | made version, job, spec, segment, budget, machine | Know HTTP, accounts or access; refuse a viewer (it reports capacity, Playback refuses) |
| [Place](docs/domains/place.md) | One history per person and title; what "played", "continue" and "next episode" mean | place, played, continue watching, next episode | Know devices, playbacks or HTTP; show one person another's history |
| [People](docs/domains/people.md) | Who someone is, and what they may see and when | account, profile, device, invitation, circle, limits | Query titles; put a credential in a link, or where page scripts can read it |
| [Together](docs/domains/together.md) | Groups in step; moving a film between devices | group | Give a member anything Playback would refuse them alone |
| [Contract](docs/domains/contract.md) (the edge) | HTTP, the published contract, the one writer of refusals | capability, reason, action | Hold domain logic or SQL |

[Platform](docs/domains/platform.md) is not a domain: the database, the stores, metrics, configuration and roles, with the rules every domain keeps.

## How domains talk

- Inside a process, plain calls in one direction: Contract → Finding, Playback, Place, People, Together → Collection's types.
- Playback calls Making (ensure a job, read capacity) and Place (report a position).
- Between processes, only through Postgres and the stores. No process calls another, and there is no event bus.

## Words

A viewer is never shown a domain word that needs explaining: a plan is shown as "how this plays and why".

- **Title:** a film, show, season or episode a person recognises.
- **Version:** one file of a title.
- **Track:** an audio or subtitle track of one version.
- **Playback:** one viewing on one device, from Play to stop.
- **Plan:** the stored decision of how a title plays, why, and what is offered.
- **Profile:** a person, the holder of a history.
- **Place:** where a person stopped in a title.
- **Reason:** plain words for a refusal or a conversion, from one list.
