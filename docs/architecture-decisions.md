# Jellyscale Architecture Decisions

This document captures the initial product and architecture decisions for Jellyscale: a brand-new, distributed media server intended as a full media platform replacement rather than a Jellyfin add-on.

## Product Direction

Jellyscale will be a full media server replacement with a microservice-native architecture.

The goal is not to wrap or scale Jellyfin directly. Instead, Jellyscale should re-imagine a self-hosted media server as a distributed system where media ingestion, metadata, playback planning, streaming, identity, observability, and workers are separately deployable components.

## Target User

The first target user is the Kubernetes / infrastructure-oriented user.

This may include advanced homelab users, but the product should assume the operator is comfortable with infrastructure concepts such as:

- Kubernetes or container orchestration
- Ingress controllers such as Traefik or NGINX
- Bring-your-own storage
- Observability stacks
- Separately deployable services
- External identity providers

## MVP Media Scope

The MVP should support movies only.

Other media types should be explicitly deferred until the core architecture is proven:

- TV shows
- Music
- Photos
- Books
- Live TV
- DVR
- DLNA

Starting with movies keeps the first vertical slice narrow while still exercising the most important parts of the system: ingestion, metadata, playback, streaming, auth, storage, and observability.

## Client Strategy

The product should not default to supporting Jellyfin clients for MVP.

There is value in Jellyfin client compatibility, but it risks carrying forward protocol assumptions, API shapes, and client behaviors that could limit a clean rewrite. Jellyscale should prioritize improved architecture and modern platform capabilities over compatibility with legacy client expectations.

A major example is authentication: Jellyscale should support OIDC natively, while Jellyfin requires a plugin for this capability.

Recommended MVP approach:

- Build a first-party web client first.
- Expose a clean Jellyscale API.
- Do not require Jellyfin API compatibility for MVP.
- Consider Jellyfin migration or import tooling later.
- Revisit Jellyfin client compatibility only if it does not compromise the new architecture.

## Playback Delivery Model

The MVP should prioritize direct play.

Most modern client devices can decode many common video files on their own, so Jellyscale should avoid unnecessary transcoding and stream processing whenever possible.

Initial playback posture:

- Direct play first.
- Pass video through to the client device where possible.
- Avoid subtitle processing in MVP.
- Avoid advanced adaptive bitrate streaming in MVP.
- Support a hybrid long-term model where transcoding and derived representations are available when direct play is not viable.

## Transcoding Philosophy

Jellyscale should use a hybrid transcoding philosophy.

The long-term system should support both:

- Live transcoding when required for playback.
- Offline optimization for media that is repeatedly expensive to play.

For MVP, direct play should be the priority. Transcoding should be treated as an architectural capability that can be expanded over time rather than the center of the first release.

The system should eventually be able to explain why transcoding occurred and recommend optimizations, but aggressive automatic optimization can wait until after the core playback and observability model is proven.

## Storage Strategy

Jellyscale should abstract storage from day one.

Operators should be able to bring their own storage. The architecture should avoid assuming that all media exists on one local filesystem path.

The initial implementation may support POSIX-style storage first, but the domain model should be storage-provider-aware from the beginning.

Storage goals:

- Support bring-your-own storage.
- Model storage backends explicitly.
- Avoid hard-coding local path assumptions into core media models.
- Allow future support for object storage and other storage providers.
- Treat generated assets separately from source media.

Suggested concepts:

- `StorageBackend`
- `StorageObject`
- `MediaAsset`
- `MediaItem`
- `MediaRepresentation`

## Metadata Strategy

For MVP, use a simple metadata strategy focused on movies.

Recommended MVP support:

- TMDb as the first external metadata provider.
- Filename-based identification.
- Manual match and correction.
- Posters and backdrops.
- Basic cast, crew, title, year, overview, genres, and runtime.

Important architectural principle:

Provider metadata, manual overrides, and resolved display metadata should be modeled separately.

This prevents provider refreshes from accidentally overwriting user edits and gives the system a cleaner long-term metadata model.

## Library Model

Libraries should be user-facing organizational and policy boundaries, not direct equivalents of filesystem folders.

Recommended model:

- `StorageLocation` represents where files are found.
- `Library` represents a user-facing media collection and policy boundary.
- `MediaItem` represents logical media.
- `MediaAsset` represents physical files or storage objects.

This allows a future where one media item can have multiple physical representations, and where storage layout does not dictate user-facing organization.

## Device and Capability Model

This may not need to be deeply modeled for MVP because authenticated clients will be requesting playback directly and direct play is the initial priority.

However, playback planning eventually requires some knowledge of device capabilities.

For MVP:

- Keep device capability modeling minimal.
- Let the first-party web client request playable streams.
- Prefer direct pass-through playback.
- Avoid complex device-profile negotiation.

Longer term:

- Model client capabilities explicitly.
- Use capability data as input to playback planning.
- Explain playback decisions based on client support, media format, policy, and worker availability.

## Playback Planning

Playback planning should become a first-class architectural concept, even if MVP behavior is simple.

For MVP, the playback planner may primarily answer:

- Is the user authorized to play this media?
- Is a direct stream URL available?
- Should the stream URL be signed?
- Which media asset should be served?

Longer term, the playback planner should decide between:

- Direct file playback
- Direct stream through the stream gateway
- Remuxing
- Live transcoding
- Cached optimized representations
- Denial due to policy

The planner should eventually produce explainable playback decisions.

## Authentication and Authorization

Jellyscale should have a dedicated auth service.

Required auth goals:

- Native OIDC support.
- Self-hosted, single-tenant MVP.
- Internet exposure allowed.
- TLS handled by the hoster's ingress or reverse proxy.
- Stream URLs should be signed, time-limited, session-bound, and revocable.

The hoster's ingress of choice, such as Traefik, may handle some request validation at the edge. Jellyscale should still have a clear internal identity and authorization model.

For MVP, service-to-service calls do not need to be authenticated.

## Security Posture

MVP security assumptions:

- Single-tenant self-hosted deployment.
- Internet exposure is supported but not required.
- TLS is delegated to the operator's ingress/reverse proxy.
- OIDC is supported natively.
- Stream URLs are protected with signed, time-limited access tokens.
- Source media paths are never exposed directly to clients unless intentionally configured.

Future security considerations:

- Service-to-service authentication.
- Worker authentication.
- More granular RBAC.
- Audit logging.
- External sharing.
- Download permissions.

## Multi-Tenancy

MVP should be single-tenant and self-hosted.

The architecture should not intentionally prevent future multi-tenancy, but true SaaS-style multi-tenancy is not an MVP requirement.

## Plugin and Extension Model

No plugin system should be included in MVP.

For a microservice-native system, future extension should prefer external integration points over arbitrary in-process plugins.

Potential future extension mechanisms:

- Webhooks
- Event subscriptions
- External metadata providers
- External notification providers
- Worker extensions
- Storage adapters

## Observability

Observability is a core product feature.

MVP observability should include:

- Active sessions
- Playback strategy
- Transcode count, if transcoding exists
- Job queue depth, if jobs exist
- Worker health, if workers exist
- Worker capabilities, if workers exist
- FFmpeg exit codes, if media processing exists
- Playback decision explanation
- API request logs
- Structured service logs

The system should eventually help operators answer:

- Why did this stream direct play?
- Why did this stream transcode?
- Why was playback denied?
- Why was this worker selected?
- Why is this media not identified?
- Why is playback failing?

## Failure Model

Recommended MVP failure posture:

- Direct playback should continue when workers are unavailable.
- Existing libraries should remain browsable when metadata providers are unavailable.
- Recommendations and analytics should not be required for playback.
- If a scheduler exists and is unavailable, new transcode jobs should fail gracefully.
- If the primary database is unavailable, the system should be considered unavailable except for already-issued stream URLs where feasible.

## Consistency Model

Use pragmatic consistency guarantees.

Recommended consistency posture:

| Area | Consistency |
| --- | --- |
| Auth/session creation | Strong |
| Playback start | Strong enough to avoid duplicate sessions |
| Watch progress | Eventually consistent but durable |
| Search | Eventually consistent |
| Metadata refresh | Eventually consistent |
| Library scan | Eventually consistent |
| Job assignment | Strong worker lease required |
| Worker heartbeat | Eventually consistent |
| Stream URL validation | Strong |

## Job Model

Jobs should be persisted, idempotent where possible, cancellable, and observable.

Suggested job classes:

- `live-critical`
- `interactive`
- `background`
- `maintenance`
- `low-priority`

For MVP, job infrastructure can be minimal if direct play is the primary path. However, the model should be compatible with future workers for metadata, scanning, thumbnails, transcoding, and optimization.

## Caching Strategy

Recommended cache model:

- Ephemeral cache: live transcode segments and temporary processing files.
- Managed derived assets: thumbnails, preview images, optimized representations.
- Persistent source assets: original media and user-managed files.

Optimized representations should not be treated as disposable cache if the system intends to reuse them intelligently.

## Artwork and Image Pipeline

For MVP, use the recommended basic image pipeline:

- Store original artwork.
- Generate common display sizes asynchronously where practical.
- Serve artwork through the application or stream gateway path.
- Support cache headers.
- Track ownership of generated images.

## Subtitles

Subtitles should not be supported in MVP.

The first version should pass video through to the client device and avoid subtitle extraction, conversion, burn-in, and synchronization concerns.

Subtitles are a significant future area because they affect playback compatibility and can force transcoding.

## Audio

For MVP, pass audio through to the client device.

Advanced audio handling should be deferred, including:

- Downmixing
- Audio normalization
- Commentary track handling
- TrueHD/DTS compatibility handling
- Atmos/DTS:X passthrough policy
- Per-user audio preferences

## Network Model

Initial network model:

- Support deployment behind an operator-managed ingress or reverse proxy.
- Allow internet exposure.
- Delegate TLS to the hoster.
- Support explicit public base URL configuration.
- Add local/remote network distinction later if needed.

## Deployment and Packaging

Jellyscale should target infrastructure-oriented deployment first.

Recommended initial deployment targets:

- Kubernetes first or Kubernetes-friendly from the beginning.
- Container images for every deployable service.
- Helm chart or equivalent deployment manifests.
- Docker Compose may be useful for local development and lightweight homelab installs, but the first-class user is infrastructure-oriented.

## Backup and Restore

Recommended data classification:

Critical data:

- Database
- Configuration
- Secrets
- User edits
- Watch state

Important but regenerable data:

- Metadata provider results
- Search index
- Thumbnails

Large/generated data:

- Optimized representations
- Transcode cache

External data:

- Source media files

Backups should prioritize critical data first.

## Upgrade and Migration Strategy

Recommended approach:

- Version external APIs.
- Version event schemas.
- Prefer additive event changes.
- Store service version in health endpoints.
- Design for future rolling upgrades.
- Do not require full rolling upgrade support in MVP.

## Licensing

Jellyscale should use a GPL-compatible license.

The exact license should be selected before code is published. FFmpeg distribution, codec support, and third-party dependency licensing should be reviewed before release.

## Data Privacy

Recommended privacy posture:

- No external telemetry by default.
- Explicit opt-in diagnostics only.
- Clear disclosure when metadata provider requests send media titles or identifiers externally.
- Redact secrets from logs.
- Provide eventual data export and deletion tools.

## Explainability UX

No final decision has been made on the exact UX, but explainability should remain a product principle.

The system should eventually explain:

- Why playback succeeded or failed.
- Why direct play was selected.
- Why transcoding was required.
- Why a worker was selected.
- Why media could not be identified.
- Why a user cannot access a library.
- Why a stream URL was rejected.

## First Vertical Slice

The first vertical slice should be:

1. User logs in.
2. User adds a movie folder or storage location.
3. System scans files.
4. System identifies movies.
5. User browses movies.
6. User starts playback.
7. System chooses direct playback.
8. User watches in the web player.
9. Admin sees active session and playback explanation.

This slice proves the core architecture without requiring broad media-server feature parity.

## Explicit Non-Goals for MVP

The following should not be built first:

- Music libraries
- Photos
- Books
- TV shows
- Live TV
- DVR
- DLNA
- Full Jellyfin API compatibility
- Native mobile apps
- Native TV apps
- Multi-tenant SaaS
- Plugin marketplace
- Full adaptive bitrate ladders
- Automatic remote worker autoscaling
- Kubernetes operator
- Social sharing
- External user invitations
- Complex parental controls
- Cloud relay
- AI recommendations
- Advanced intro or credit detection
- Subtitle support
- Advanced audio processing

## Initial Service Split

Recommended initial deployable services:

- `api-gateway`
- `identity-service`
- `media-service`
- `library-service`
- `playback-service`
- `stream-gateway`
- `web-ui`
- `postgres`
- `redis`
- `nats`

Potential post-MVP services:

- `worker-service`
- `scheduler-service`
- `search-service`
- `recommendation-service`
- `notification-service`
- `metadata-provider-service`

## Open Questions

The following questions remain unresolved:

1. Should Kubernetes be the only first-class deployment target, or should Docker Compose also be officially supported for MVP?
2. Should playback use plain direct file/range responses, or should all streams still flow through a stream gateway?
3. Which exact web playback formats and codecs should be supported first?
4. Should OIDC be required for MVP, or should local users also be supported from day one?
5. Which GPL-compatible license should be selected?
6. How much of the explainability UX should be present in the first vertical slice?
7. Should a search service be present in MVP, or can basic browsing rely on the primary database initially?
8. Should media scans be implemented as service-internal tasks first or as explicit jobs from day one?
9. How should stream URL signing keys be generated, stored, rotated, and revoked?
10. What is the minimum viable admin UI for infrastructure-oriented users?
