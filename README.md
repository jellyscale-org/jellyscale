# jellyscale

A self-hosted streaming server for a private collection of films and shows, shared with a household, friends and family, or a community. It runs across machines, keeps films on local disks or S3-compatible object storage, and is built for its own apps, [Octave](https://github.com/jellyscale-org/octave). Video only. [PRODUCT.md](PRODUCT.md) says what it should be.

## Status: being rebuilt

jellyscale began as a server for another project's apps. It is now being rebuilt as software of its own: a new plan and new server code, keeping only the video engine (conversion, seeking, subtitles and storage) and its tests.

- **Nothing of the new server is released,** and there is nothing to deploy from `main` today.
- **The earlier server** is the tag [`pre-rebuild`](https://github.com/jellyscale-org/jellyscale/tree/pre-rebuild), with its own documentation. It is not developed further.
- **The code in the tree** is still mostly the earlier server's. It goes as the new server replaces it.

[docs/status.md](docs/status.md) is kept current.

## Documentation

- [PRODUCT.md](PRODUCT.md): what jellyscale is for, and the experience it should give.
- [DOMAINS.md](DOMAINS.md): its parts and the rules a change must keep.
- [ARCHITECTURE.md](ARCHITECTURE.md): the intended design.
- [docs/engine/](docs/engine/README.md): what is known, measured, about the video engine.
- [docs/players.md](docs/players.md): what players were seen to do.
- [docs/testing.md](docs/testing.md): how it is tested.
- [AGENTS.md](AGENTS.md): the rules of working here, for people and agents. [docs/delivery.md](docs/delivery.md) is how a task is run.

## Contribute

Read [CONTRIBUTING.md](CONTRIBUTING.md). While the rebuild is under way, open an issue before starting anything.

## License

GPL-3.0-or-later. No code is copied from other media servers.
