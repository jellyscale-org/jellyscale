# Product

What jellyscale should be, and the experience it exists to give: the overall product and its direction, nothing else. It says what viewers and operators get, never how it is built; how is for the design to decide. It guides every task: a design is right when it moves the product toward what is written here. What is built is in [docs/status.md](docs/status.md), and the work still to do in [tasks/prd.json](tasks/prd.json).

"jellyscale" and "Octave" are working names. The owner has chosen Zoetrope for the server and Praxi for the viewer apps, after the early moving-picture devices, as the convention for all of the project's software. They replace the working names here and everywhere else when the rename lands, once a name check clears them (JS-200).

## What jellyscale is

A self-hosted streaming service for a private collection of films and shows, shared with a household, friends and family, or a community. It runs as a horizontally scalable service: it spreads across machines, survives losing any one of them, keeps its films on local disks or in object storage, and gives its viewers an experience designed for them, at home or far away on a weak connection.

jellyscale is the back end. Octave is its front end and ships with it: the app viewers use on a TV, on a phone and in a browser. jellyscale's API exists for Octave, and the best experience for viewers decides what both do. Octave is built by its own project; jellyscale's work is the server and everything Octave's features need from it.

jellyscale began as a server for Jellyfin's apps. It is now new software of its own, and Jellyfin is behind it (D-107). An operator moving from Jellyfin brings their films, their accounts and everyone's watch history, and their viewers move to Octave.

## Who it's for

**Operators** running one private collection on Kubernetes for a household, friends and family, or a community, with tens to hundreds of people watching at once, many of them from outside the home over the operator's own upload. They are comfortable with Kubernetes and Postgres, and want the server to be something they set up once and then mostly forget.

**Viewers**, who are not technical and never need to be. They see only Octave, on a TV, a phone, a laptop or a browser.

## The experience we're building

When a design or a task would trade any part of this away, that is a decision for the product owner, recorded as a gap with a task, never a quiet exception.

### The viewer

A viewer opens Octave, signs in, and finds their films and shows. They never need to know jellyscale exists.

- **Any film plays, and plays well.** A 4K HDR Blu-ray rip with Dolby Vision and image subtitles, an old DVD rip, an AV1 web release: each plays on the device in their hand, with the audio and subtitles they chose. No codec, HDR format or subtitle track is second-class: whatever the server does to make one film play well, it does for every film.
- **The picture follows the connection.** At home the film plays as it is. On a weak or crowded connection the picture steps down to a lighter version and back up when it can, while the film keeps playing, with no setting to find first. A viewer can override it at any time, as on YouTube: pick a quality and it stays there until they choose automatic again. The versions are the original, always the best, and a few lighter steps below it. 720p is the usual floor; 480p is offered only for a film whose own picture is SD, or when nothing higher can keep it playing. A step is never larger than the film itself.
- **The picture is as good as the device and the connection allow.** A film the device can play is sent as it is. One it can almost play has only what the device can't handle converted, so the picture keeps its original quality. Only when nothing else works, or the connection can't carry the original, is the picture re-encoded, and an HDR film then looks right on an ordinary screen.
- **Their languages come first.** A viewer chooses the language they listen in and the language they read, once, and every film on every device starts that way where it can. Changing the audio or subtitle language mid-film is quick and doesn't restart the picture, and choosing a subtitle never costs picture quality. A track they pick for a film is the one it starts with when they come back. A viewer who wants shorter menus can ask for the rest to be hidden: audio and subtitles in other languages leave the track menus, except the film's original language and any languages they chose to keep. Nothing is hidden until they ask, a film is never left without sound, one switch brings every track back, and nothing is removed from the collection.
- **Play starts in seconds and keeps up.** The next episode and a film the viewer is about to resume are ready before they press Play, and a heavy film never stalls on one machine while others sit idle.
- **Seeking and resuming just work.** Jump anywhere and the picture, sound and subtitles are right from the first frame, with a preview of where they are jumping to. Stop on the TV and carry on from the phone at the same moment.
- **Failures are invisible.** A server or worker that dies mid-film costs at most a moment of buffering, never the stream, the place in the film or the subtitles.
- **What can't be done is said plainly.** When a film can't play now, or part of it can't be made, the viewer is told what happened and what they can do (wait, try again, pick another track or version), in words they understand. Never a frozen picture, never a generic error.
- **They can see what they get.** The viewer can see whether a film plays as it is or what is converted, and why, and which quality they are watching.
- **Their place is always one press away.** Each person has their own watch history, one per title, whichever device or version they watched. Continue watching (with each show's next episode) is the same on every device, and a viewer can take a title off it without losing their place. Next-episode autoplay is theirs to turn off.
- **Finding something to watch is easy.** Films and Shows are the ways in, each holding everything the viewer may see, however the operator keeps the files. A short home that stays where they left it, and the whole collection one press away, sortable and filtered by what matters to them: unwatched, language, audio description and subtitles for the deaf or hard of hearing, 4K and HDR. Search finds a title from its first letters, in its original or another title, and says plainly when the collection doesn't have it. Then, in order: their own list, collections that know their kind (a franchise in its watch order, a list someone made, a saved rule), more like a title with the reason shown, artwork, chapters, trickplay previews and intro skipping. Several files of one film are one title; different cuts are different titles.
- **What they watch stays theirs.** Nothing shows one viewer what another watched. Anything drawn from what the whole server watches is off unless the operator turns it on, and never names or reveals a person.
- **Watching together works.** A group can watch the same film in step, whichever servers each of them is connected to, and a viewer can move what they are watching to another of their devices. No one sees in a group what they couldn't see alone.
- **Joining and signing in are easy and safe.** A friend joins from an invitation that gives them exactly what the operator chose they may see, and the limits that go with it. A viewer signs in with a password or the household's own single sign-on; passkeys and other strong sign-in come from that provider, not from jellyscale. On a TV, the viewer approves the TV from their phone, checking that both show the same code, signing in on the phone if they need to: no TV asks for a password typed with a remote. A device stays signed in until the viewer or the operator signs it out, and a viewer can see and sign out each of their devices.
- **They see only what they may.** Each viewer sees only what the operator allows them, everywhere: home, lists, search, filters and their counts, collections, next episodes, recommendations, groups and every picture and media request. A child is protected by what they can see, never by how titles are ranked. How the collection is organised never grants or hides anything by itself.
- **Their app keeps working.** An Octave app the viewer hasn't updated keeps working after the server is upgraded.

### The operator

- **Installing is boring.** Plain manifests applied with `kubectl` or a Helm chart, whichever the operator prefers, plus a Postgres and storage, chosen for each place films are kept between local disks and S3-compatible object storage, with neither second-class. The server ships with Octave Web, so a fresh install opens in a browser with nothing copied in. Where the films are, settings, access and limits are declared in configuration and kept in version control, not clicked together in a dashboard. A stable address the viewers reach at home and away is all the network it asks for.
- **New films just appear.** A film or episode added to storage shows up for viewers soon after, and one removed or renamed keeps everyone's history, with nothing for the operator to do.
- **Growing is adding machines.** Each part (the API, streaming, conversion, the work on the collection) scales on its own, and more viewers or heavier films are met by more machines rather than a bigger one. The cluster can grow and shrink with demand by itself, and uses the hardware the operator gives it.
- **The upload is shared fairly.** The operator says how much of their upload remote viewers may use. When it is full, streams step down to fit rather than anyone being refused, and the household comes before friends, and friends before a community.
- **Storage stays bounded.** The collection is stored once, as it is. Everything the server makes from it (lighter versions, converted audio, subtitles) is made only when a viewer needs it, kept only while it is used, and held within a budget the operator sets. An operator who would rather spend storage than make viewers wait can choose to have versions made ahead, for the titles and qualities they pick, within a share of the budget they set; it is off until they turn it on, and it never slows anything a viewer or the operator is waiting for. The operator can see what the made versions take and why. Local disks and S3-compatible object storage are equals for the films and for everything made from them.
- **Nothing breaks the viewers' evening.** Upgrades, a lost machine and a database failover happen without interrupting anyone, and no upgrade makes viewers update their apps that day.
- **Nothing is hidden.** They can see who is watching what, how it is being delivered and at which quality, how fast each conversion runs and where, and what the viewer's player saw. Every failure is reported with its reason, never silent. They can follow any one playback from the decision to the last piece delivered, including each step down, and see exactly where it stalled and why, and when the cluster needed more machines they can see what demanded them.
- **Access is enforced everywhere.** Nothing, artwork included, is reached without a signed-in person whose role allows it, on every request. No link carries a viewer's sign-in: a media link opens one playback only, and ends when the operator or the viewer ends it.
- **Their data is theirs.** Moving from Jellyfin keeps every account and everyone's watch history. Backups are the Postgres and the stores they already back up. Metadata, collections and their orders come from files beside the films or from configuration, not from outside services. Nothing about a film or a viewer (its audio, subtitles, watch history, playback measurements or logs) is sent outside the operator's servers. A viewer hiding languages changes only their own menus: no file is touched and nothing is made or stored differently.

## Goals, in priority order

When two goals pull against each other, the higher one wins.

1. **Playback never surprises a viewer.** Starting, seeking, resuming, adapting to the connection and recovering from a failure just work, for every film, and what can't work is explained.
2. **Every component scales independently,** vertically or horizontally, with no shared state outside Postgres and the stores.
3. **Octave's features have the server they need,** through one API designed for the best viewer experience, and an Octave release keeps working across server upgrades.
4. **Storage is chosen for each place films are kept,** between POSIX and S3-compatible object storage, with neither a second-class citizen, and what the server makes stays within the operator's budget.
5. **Operating it stays boring.** Postgres is the only required service, and configuration is declarative.

## What sets it apart

- One server becomes many, with no single process whose loss stops playback.
- The server owns each playback: it can step a stream down, explain a failure, or move it to another device while it plays.
- The stream adapts to the viewer's connection, and the operator's upload is shared rather than oversold.
- A slow conversion is shared across the cluster instead of stalling a viewer, and does only as much work as the viewer needs.
- A language switch costs neither a pause nor a new conversion of the picture.
- Films can live in object storage as well as on disks.
- One collection, not folders: one history per title, collections and filters that span everything, and access that never depends on where a file is kept.
- Every conversion and every refusal carries its reason: to the viewer in plain words, to the operator in full.
- A viewer's track menus can hold only the languages they use, and a film knows its original language from its own file.
- Where a capability can't yet serve every film, the gap is written down and planned, not left as a quirk.

## Non-goals

- **Anything but video, ever:** music, photo libraries, audiobooks, channels and Live TV. jellyscale streams films and shows; no other kind of content is added, whatever a client or a request asks.
- **Plugins.**
- **Multi-tenancy.** One collection, one operator.
- **Clients other than Octave.** jellyscale designs for Octave. Its API is documented and another client may use it, but no work goes in for one.
- **Jellyfin's API.** It is frozen now and leaves in one clean cutover (The road there, stage 3).
- **Its own passkeys or second factors.** Strong sign-in is the identity provider's job.
- **Versions made ahead by default.** Nothing is converted before a viewer needs it unless the operator has chosen to spend storage on it.
- **Recommendations learned from other people's viewing.** What is suggested comes from the titles themselves and from the viewer, with its reason shown.
- **Administration through a dashboard:** where films are kept, access, metadata, collections, scheduled work and server settings are configuration.
- **Fetching metadata from outside services,** editing items, and uploading or deleting media through the API. An operator supplies metadata as files beside the film.

## API policy

- **One API, jellyscale's own,** versioned and documented in one published contract between jellyscale and Octave.
- **Designed from the viewer's outcome.** The server owns each playback; every refusal and failure carries a reason a client can explain; a track and a title keep their identity; a media link opens one playback and carries no credential of the viewer's account.
- **Octave always has something to build against:** a working server for every change to jellyscale's main line, with each new capability in the contract before it ships.
- **A version stays** while a supported Octave release uses it. A change that would break it is a new version, served beside the old.

## Quality bar

- Behaviour is decided from the viewer's outcome and written into the contract before it is built. The players it depends on are observed, not guessed. No code is ported from Jellyfin.
- A claim of working means Octave, on a real player, or a real store exercised it.
- Failure modes are designed, tested and reported. Nothing fails silently.

## The road there

The experience above arrives in this order, each stage usable on its own:

1. **Watching:** browse, play, convert, subtitles and audio for every film.
2. **Never interrupted:** any part of the cluster can fail mid-film without the viewer losing their place.
3. **Its own API, and Octave on Android TV and phones first:** sign-in, Films and Shows, one watch history per person with Continue watching, and playback, in Octave on Android TV and on Android and iOS phones; then Octave Web, shipped with the server. Octave always has a working jellyscale to build and test against. Jellyfin's API leaves in one clean cutover once the new API can browse films and shows and start playback; the dogfood's viewers may go briefly without an app while Octave's release catches up. Apple TV is decided later.
4. **Watching from anywhere:** the picture following the connection, quick language switches, the shared upload, and playback ready before Play.
5. **People:** invitations, single sign-on, TV sign-in from a phone, per-viewer access and limits, devices and activity.
6. **A living collection:** filters, search, my list, collections and their kinds, more-like-this with reasons, metadata and artwork from files, versions and editions, chapters, trickplay and intro skipping, new films appearing on their own, and the operator's own hardware put to use.
7. **Running on Kubernetes:** example manifests and a Helm chart, autoscaling, upgrades without downtime, backup, and migration from Jellyfin.
8. **Together:** watching in step and moving a film between devices, across the cluster.

Later, and second to streaming: films kept on a phone to watch offline, at a lighter version, within the operator's upload and storage.
