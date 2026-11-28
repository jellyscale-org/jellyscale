# Playback

One viewing from Play to stop: the decision, the plan, the link, steps, track switches, refusals, limits and moves.

## Words

- **Playback:** one viewing on one device, from Play to stop.
- **Plan:** the stored decision: how the title plays (as it is, converted, re-encoded), why, and what is offered (picture steps, audio, subtitles). A viewer is shown it as "how this plays and why".
- **Step:** a lighter quality below the original.
- **Media link:** what a player fetches a playback through.
- **Reason:** plain words for a refusal or a conversion, from one list.

## Must not

Run media tools; write history rules.

## Rules

1. The server owns each playback. It is stored, any replica serves any request of it, and nothing about it lives only in one process's memory.
2. A media link opens one playback, carries no account credential, and stops working when its playback ends.
3. A playback lives while its app reports. Silent for long enough, it has ended, and an ended playback never revives. Play then starts again at the viewer's place, with their picks, without an error.
4. Refusal happens at Play, with a reason and what the viewer can do. A playback that has begun is not refused for something Play could have known.
5. A plan offers only what the server can deliver now. Every track is listed as itself; one that can't be delivered carries its reason, and a pick is never played as another track.
6. What a playback may fetch is decided by its stored plan, never by what a link or a request says.
7. The picture is sent as it is when the device plays it. Otherwise only what the device can't handle is converted, and the picture is re-encoded last. A subtitle is drawn into the picture only for a player that can show none of it.
8. Changing audio or subtitles never restarts or re-makes the picture.
9. Limits (hours, streams at once, the upload share) are enforced here, at Play and while playing, with a reason. A refusal for streams names the devices playing.
10. What the server tells a running playback can be replayed after a reconnect. A lost wakeup only delays.
11. During an upgrade, no replica serves a plan it doesn't understand.
12. Playback reports positions to Place and asks Making for work. It keeps no history and runs no tool.
