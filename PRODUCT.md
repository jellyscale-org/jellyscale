# Product

What jellyscale should be, and the experience it exists to give. It guides every task: a design is right when it moves the product toward what is written here. What is built so far is in [docs/status.md](docs/status.md), and the work still to do is in [tasks/prd.json](tasks/prd.json).

## What jellyscale is

A self-hosted video server that speaks the Jellyfin client API and runs as a horizontally scalable service. Someone who has outgrown a single Jellyfin process keeps their viewers' apps, and gains a server that spreads across machines, survives losing any one of them, and keeps its films on local disks or in object storage.

## Who it's for

**Operators** running one library on Kubernetes for a household or a community, with tens to hundreds of people watching at once. They are comfortable with Helm and Postgres, and want the server to be something they set up once and then mostly forget.

**Viewers**, who are not technical and never need to be. They see only a Jellyfin app on a phone, a TV, a laptop or a browser.

## The experience we're building

When a design or a task would trade any part of this away, that is a decision for the product owner, recorded as a gap with a task, never a quiet exception.

### The viewer

A viewer opens the Jellyfin app they already use, signs in, and finds their films and shows. They never need to know jellyscale exists.

- **Any film plays, and plays well.** A 4K HDR Blu-ray rip with Dolby Vision and PGS subtitles, an old DVD rip with VobSub, an AV1 web release: each plays on the device in their hand, with the audio and subtitles they chose. No codec, HDR format or subtitle track is second-class. Everything the server can do (direct play, copying, tone mapping, sharing a slow encode across the cluster, failover, seeking) it does for every film.
- **The picture is as good as the device allows.** A film the device can play is sent as it is. One it can almost play has only what the device can't handle converted, so the picture keeps its original quality. Only when nothing else works is the picture re-encoded, and an HDR film then looks right on an ordinary screen.
- **Play starts in seconds and keeps up.** A heavy film uses the whole cluster's processors rather than stalling on one machine while others sit idle.
- **Seeking and resuming just work.** Jump anywhere and the picture, sound and subtitles are right from the first frame. Stop on the TV and carry on from the phone at the same moment.
- **Failures are invisible.** A server or worker that dies mid-film costs at most a moment of buffering, never the stream, the place in the film or the subtitles.
- **Their library feels alive.** Continue watching, next up, favourites, collections, artwork, chapters, trickplay previews and intro skipping are there as they are in Jellyfin.
- **Watching together works.** A group can watch the same film in step, whichever servers each of them is connected to.
- **Signing in is easy and safe.** A password, a code entered on the TV from a phone, or the household's own single sign-on, and only the libraries they are allowed to see.

### The operator

- **Installing is boring.** A Helm chart, a Postgres, and storage, chosen per library between local disks and S3-compatible object storage, with neither second-class. Libraries and settings are declared in configuration and kept in version control, not clicked together in a dashboard.
- **Growing is adding machines.** Each part (the API, streaming, transcoding, library work) scales on its own, and more viewers or heavier films are met by more machines rather than a bigger one. The cluster can grow and shrink with demand by itself.
- **Nothing breaks the viewers' evening.** Upgrades, a lost machine and a database failover happen without interrupting anyone.
- **Nothing is hidden.** They can see who is watching what, how it is being delivered, how fast each encode runs and where, and every failure is reported with its reason, never silent.
- **Their data is theirs.** Moving from Jellyfin keeps everyone's watch history. Backups are the Postgres and the stores they already back up. Metadata comes from files beside the film, not from outside services.

## Goals, in priority order

When two goals pull against each other, the higher one wins.

1. **Playback never surprises a viewer.** Starting, seeking, resuming, and recovering from a failure just work, for every film.
2. **Every component scales independently,** vertically or horizontally, with no shared state outside Postgres and the stores.
3. **Current Jellyfin clients work unmodified,** unless supporting one would create lasting technical debt.
4. **Storage is a per-library choice** between POSIX and S3-compatible object storage, with neither a second-class citizen.
5. **Operating it stays boring.** Postgres is the only required service, and configuration is declarative.

## Better than Jellyfin

Jellyfin's behaviour shows what clients depend on, not what to copy. jellyscale keeps what clients need and does better wherever they allow it:

- One server becomes many, with no single process whose loss stops playback.
- A slow transcode is shared across the cluster instead of stalling a viewer.
- A transcode reads only as far ahead as the viewer needs, so a viewer who stops early doesn't cost the whole film's reading.
- Films can live in object storage as well as on disks.
- Operators see the real encode speed and the worker for every stream.
- Where a capability can't yet serve every film, the gap is written down and planned, not left as a quirk.

## Non-goals

- **Music, photos, audiobooks, channels and Live TV.** Better tools exist for those.
- **Plugins,** .NET or otherwise.
- **Multi-tenancy.** One library, one operator.
- **A jellyscale-branded client.** jellyfin-web is the reference app, unmodified.
- **Administration through the dashboard:** libraries, metadata, scheduled tasks and server settings are configuration.
- **Fetching metadata from outside services,** editing items, and uploading or deleting media through the API. An operator supplies metadata as files beside the film.
- **Routes Jellyfin marks obsolete,** apart from the exception below.

## Compatibility policy

- **Target:** the current Jellyfin release, currently 12.4.
- **Future-facing:** a client that depends on obsolete routes isn't well suited to jellyscale.
- **The one exception:** thin aliases for routes jellyfin-web still calls, flagged `alias` in `compat/endpoints.yaml`. Each one is removed when the web client stops calling it. Never add an alias for another client.
- **Outside scope but called anyway:** answer with a valid empty response if the client breaks otherwise; otherwise 404.
- **Clients in order:** jellyfin-web first, then the other apps built on Jellyfin's client libraries (Findroid, Android TV, Swiftfin, Streamyfin). Kodi isn't a stated target: it works if it happens to, and no work goes in solely for it.

## Quality bar

- Behaviour is observed from a real Jellyfin, not guessed. No code is ported from Jellyfin.
- A claim of working means a real client or a real store exercised it.
- Failure modes are designed, tested and reported. Nothing fails silently.
- Code is simple, readable and maintainable by people. The simplest thing that works wins.

## The road there

The experience above arrives in this order, each stage usable on its own:

1. **Watching:** browse, play, transcode, subtitles and audio for every film.
2. **Never interrupted:** any part of the cluster can fail mid-film without the viewer losing their place.
3. **People:** accounts, sign-in by code and single sign-on, per-user libraries, devices and activity.
4. **A living library:** metadata and artwork from files, next up, collections, versions, chapters, trickplay and intro skipping, hardware encoding.
5. **Running on Kubernetes:** the Helm chart, autoscaling, upgrades without downtime, backup, and migration from Jellyfin.
6. **Watching together:** SyncPlay across the cluster.
