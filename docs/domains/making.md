# Making

Everything made from a film: segments, extracted tracks, fonts and pictures; jobs, workers, capacity and the storage budget.

The carried-over video engine lives here. What is known about it, measured, is in [docs/engine/](../engine/README.md): read the page for the part a task touches.

## Words

- **Made version:** anything the server makes from a film. All of it can be made again.
- **Job:** one run of a media tool, claimed by a worker.
- **Spec:** what identifies one made output, so that two requests for the same thing share it.
- **Segment:** a piece of a made stream.
- **Budget:** one storage budget over everything made.
- **Machine:** a worker and what it can hold.

## Must not

Know HTTP, accounts or access. Refuse a viewer: Making reports capacity, and Playback refuses.

## Rules

1. Only Making runs media tools, and only on workers.
2. What a spec names is the same whichever worker made it, wherever its job began and however fast it ran, so any worker can take over from any other.
3. Work is pulled. A worker takes a job only when it can hold it; overload is a queue, never a stall on one machine while others sit idle.
4. Every job has an end: its playback ending, nobody asking, or its lease. No tool outlives its lease, and a worker that lost a job stores nothing more for it.
5. One budget covers everything made. Nothing a live playback needs is removed for it, and versions made ahead go first.
6. Work is ordered by who waits: a waiting viewer, then a running playback, then what is being made ready before Play, then versions made ahead, then background work. A higher class takes room from a lower one.
7. A failure that is the file's is remembered for that revision of the file and not retried. A failure that is not the file's (a store, a timeout, a lost worker) is never blamed on the file.
8. Versions are made ahead of any viewer only when the operator chose it. Getting ready what a viewer is about to play is not that.
