# Together (stage 8)

## Words

- **Group.**

## Owns

Groups in step, and moving a film between devices. Owns the table `groups` (later). A group holds playbacks, and each member's playback passed the access rule itself.

Moving a film to another device starts a new playback from the old one's title, position and picks, checked for access and limits again; the old one ends with reason `moved`. The upload share is counted per circle (Playback).

## Must not

Give a member anything Playback would refuse them alone. Require one server for the group.

## Rules

Rules at the cutover (JS-194); today's invariants apply until then.

No numbered rule is homed here. Rule 68 (a playback is a row) and rule 75 (limits count held playbacks, under one lock per account) apply to groups, and so does rule 70 (each member's playback passed the rule itself). Nothing here weakens Playback or People.
