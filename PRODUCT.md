# Product

What jellyscale should be: the product and its direction, nothing else. It says what viewers and operators get, never how it is built; how is for the design to decide. A design is right when it moves the product toward what is written here.

Only the files in `tasks/decisions/` are decisions. This file describes the product those decisions add up to; the owner approved it as written (D-20261128-rewrite-approved), with the owner's later answers recorded in their decisions.

## What jellyscale is

A self-hosted streaming service for a private collection of films and shows, shared with a household, friends and family, or a community. It spreads across machines, survives losing any one of them, keeps its films on local disks or in object storage, and gives its viewers an experience designed for them, at home or far away on a weak connection.

jellyscale is the server. Octave is the client it is designed for: a separate product, with its own owner, plan and releases, whose native apps run on Android TV, Android phones, the web and Apple's phones and tablets. jellyscale's API exists for Octave, and the best experience for viewers decides what both do. Each release of jellyscale ships a specific, tested release of Octave Web.

jellyscale is new software. It is not a stand-in for another server, and nothing is built because another server has it.

## Who it's for

**Operators** running one private collection for a household, friends and family, or a community, with tens to hundreds of people watching at once, many of them from outside the home over the operator's own upload. They run it on Kubernetes with Postgres, and want a server they set up once and then mostly forget.

**Viewers**, who are not technical and never need to be. They see only Octave, on a TV, a phone, a laptop or a browser.

## The experience

When a design or a task would trade any part of this away, that is a decision for the product owner, recorded, never a quiet exception.

### The viewer

A viewer opens Octave, signs in, and finds their films and shows. They never need to know jellyscale exists.

- **Any film plays, and plays well.** A 4K HDR disc rip with Dolby Vision and image subtitles, an old DVD rip, an AV1 web release, a film kept as a disc image or a disc folder: each plays on the device in their hand, with the audio and subtitles they chose. No codec, HDR format or subtitle kind is second-class.
- **The picture follows the connection.** At home the film plays as it is. On a weak or crowded connection the picture steps down to a lighter version and back up when it can, while the film keeps playing. A viewer can override it at any time: pick a quality and it stays until they choose automatic again. The versions are the original, always the best, and a few lighter steps below it. 720p is the usual floor; 480p is offered only for a film whose own picture is SD, or when nothing higher keeps it playing. No step is larger than the film itself.
- **The picture is as good as the device and the connection allow.** A film the device can play is sent as it is. One it can almost play has only the part it can't handle converted. Only when nothing else works is the picture re-encoded, and an HDR film then looks right on an ordinary screen.
- **Their languages come first.** A viewer chooses the language they listen in and the language they read, once, and every film on every device starts that way where it can. Changing audio or subtitles mid-film is quick, doesn't restart the picture, and never costs picture quality. A track they pick for a film is the one it starts with when they come back. A viewer who wants shorter menus can hide tracks in languages they don't use; a film is never left without sound, and one switch brings every track back.
- **Play starts in seconds and keeps up.** The next episode and a film they are about to resume are ready before they press Play.
- **Seeking and resuming just work.** Jump anywhere and the picture, sound and subtitles are right from the first frame, with a preview of where they are jumping to. Stop on the TV and carry on from the phone at the same moment.
- **Failures are invisible.** A machine that dies mid-film costs at most a moment of buffering, never the stream, the place in the film or the subtitles.
- **What can't be done is said plainly.** When a film can't play now, or part of it can't be made, the viewer is told what happened and what they can do, in words they understand. Never a frozen picture, never a generic error.
- **They can see what they get:** whether a film plays as it is or what is converted, and why, and which quality they are watching.
- **Their place is always one press away.** Each person has one watch history, whichever device or version they watched. Continue watching, with each show's next episode, is the same on every device, and a title can be taken off it without losing the place.
- **They hear when something new arrives.** A new episode of a show they follow is marked in the app. A viewer who turns it on also gets a notification that says only that something new is waiting.
- **Finding something to watch is easy.** Films and Shows are the ways in, each holding everything the viewer may see, however the operator keeps the files. A short home, the whole collection one press away with sorting and filters, and search that finds a title from its first letters and says plainly when the collection doesn't have it. Then: their own list, collections, more like a title with the reason shown, artwork and the people in a title, chapters, previews while seeking, and intro skipping. Several files of one film are one title.
- **What they watch stays theirs.** Nothing shows one viewer what another watched.
- **Joining and signing in are easy and safe.** A friend joins from an invitation that gives them exactly what the operator chose. A viewer signs in with a password or the household's own single sign-on; passkeys and other strong sign-in come from that provider. A TV is approved from a phone, never by typing a password with a remote. A device stays signed in until the viewer or the operator signs it out, and a viewer can see and sign out each of their devices. A shared TV holds the household's profiles, each with its own history, languages and limits, and a profile can ask for a PIN.
- **They see only what they may.** Each viewer sees only what the operator allows, everywhere: lists, search, counts, recommendations and every media request. The operator can set the hours a profile may watch and how many streams an account may play at once, and a viewer outside them is told why.
- **Watching together works.** A group can watch the same film in step, and a viewer can move what they are watching to another of their devices.
- **Their app keeps working.** An Octave app the viewer hasn't updated keeps working after the server is upgraded.

### The operator

- **Installing is boring.** Plain manifests or a Helm chart, a Postgres, and storage chosen for each place films are kept, local disks or S3-compatible object storage, with neither second-class. A fresh install opens in a browser. A stable address viewers reach at home and away is all the network it asks for. Where the films are, settings, access and limits are configuration kept in version control, not set in an administration screen.
- **New films just appear.** A film added to storage shows up soon after, and one removed or renamed keeps everyone's history, with nothing for the operator to do.
- **Growing is adding machines.** Each part scales on its own, and more viewers or heavier films are met by more machines, not a bigger one. It uses the hardware the operator gives it.
- **The upload is shared fairly.** The operator says how much of their upload remote viewers may use. When it is full, streams step down to fit instead of anyone being refused, and the household comes before friends, and friends before a community.
- **Storage stays bounded.** The collection is stored once, as it is. Everything made from it is made when a viewer needs it, kept while it is used, and held within a budget the operator sets. An operator who would rather spend storage than make viewers wait can have versions made ahead, for the titles and qualities they pick; it is off until they turn it on, and it never slows anything a viewer is waiting for.
- **Nothing breaks the viewers' evening.** Upgrades, a lost machine and a database failover happen without interrupting anyone.
- **Nothing is hidden.** They can see who is watching what, how it is delivered and why, and follow any one playback from Play to its last moment. Every failure is reported with its reason.
- **Access is enforced everywhere.** No film, and nothing personal, is reached without a signed-in person who is allowed it. Posters, backdrops and photos of the people in a film may be public; a picture taken from a film or out of its file is reached only by a signed-in person's app, through a link that works for a short time. A link to a film can't be used to act as the viewer, and stops working when its playback ends.
- **Their data is theirs.** Backups are the Postgres and the stores they already back up. The people watching on the current test installation keep their accounts and watch history when the new server replaces it; importing from other media servers in general is not decided. A title's information, artwork and the people in it come from files beside the films. An operator who turns it on can have what those files lack filled in from The Movie Database: off until they do, the files always winning, nothing to upload by hand. Nothing about a film or a viewer leaves the operator's servers, with two exceptions. One the operator chooses: to find a film in The Movie Database, the server sends what identifies it (an identifier from its files, else its title and year) and never anything about who watches. One a viewer chooses: a new-episode notification passes through Apple's or Google's notification service and carries no title and nothing about what they watch.

## Goals, in priority order

When two goals pull against each other, the higher one wins.

1. **Playback never surprises a viewer.** Starting, seeking, resuming, adapting to the connection and recovering from a failure just work, for every film, and what can't work is explained.
2. **Every part scales on its own,** and no single process's loss stops playback.
3. **Octave has the server it needs,** through one API designed for the best viewer experience.
4. **Local disks and object storage are equals,** and what the server makes stays within the operator's budget.
5. **Operating it stays boring.**

## The API

- **One API, jellyscale's own,** versioned and published as one contract between jellyscale and Octave.
- **Designed from the viewer's outcome.** Every refusal and failure carries a reason a client can explain.
- **A capability is in the contract before the server serves it,** and every change to the contract reaches Octave.
- **A version stays** while a supported Octave release uses it.
- **Octave always has something to build against** once the new contract is published: a working server for every change to it.

## Not part of the product

- **Anything but video, ever:** music, photo libraries, audiobooks, channels and live TV.
- **Clients other than Octave.** The API is documented and another client may use it, but no work goes in for one.
- **Another server's API or features.**
- **Its own passkeys or second factors.** Strong sign-in is the identity provider's job.
- **Versions made ahead by default.**
- **Plugins.**
- **More than one collection or operator in one installation.**
- **Recommendations learned from other people's viewing.** What is suggested comes from the titles themselves and from the viewer.
- **Editing titles, and uploading or deleting media, through the API.**

## The road there

Each stage is usable on its own. The product owner plans from this order.

1. **Sign in and watch.** Octave's apps sign in, browse Films and Shows, and play any film, with subtitles and audio, one watch history and Continue watching. A TV is approved from a phone. Android TV and phones first.
2. **Never interrupted.** Any machine can fail mid-film without the viewer losing their place.
3. **Watching from anywhere.** The picture following the connection, quick language switches, the shared upload, and playback ready before Play.
4. **People.** Invitations, single sign-on, profiles, per-viewer access, viewing hours and stream limits, devices and activity.
5. **A living collection.** Notifications, disc images, filters, search, lists and collections, more like this, artwork and metadata, chapters, previews, intro skipping, new films appearing on their own.
6. **Running on Kubernetes.** Manifests and a Helm chart, growing and shrinking with demand, upgrades without downtime, backup.
7. **Together.** Watching in step, and moving a film between devices.

Later, and second to streaming: films kept on a phone to watch offline. Apple TV is decided later.
