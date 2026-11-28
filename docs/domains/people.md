# People

Who someone is, and what they may see and when.

## Words

- **Account:** who signs in.
- **Profile:** a person, the holder of a history, languages and limits. Every account has at least one.
- **Device:** one signed-in app on one device.
- **Invitation:** how a new person joins, with exactly what the operator chose for them.
- **Circle:** household, friends or community.
- **Limits:** the hours a profile may watch, and the streams an account may play at once. Playback enforces them.
- **Mark:** a named fact about a title that access can read, such as `kids`. It is about the title, never about where its file is.

## Must not

Query titles. Put a credential in a link, or where page scripts can read it. Let access be decided anywhere but here.

## Rules

1. Access is decided in one place and read with every request, so a change acts at once on every replica.
2. Access reads facts about a title: its marks, its rating and its identity. Nothing that decides access names a place films are kept or a directory, so moving a file changes nobody's access.
3. No stored value signs anyone in: what the server keeps of a credential can't be used as one.
4. A credential belongs to one device. Ending it (sign-out, an account turned off or removed, a changed password) ends what that device is streaming within seconds, on every replica.
5. A failed sign-in reveals nothing about which accounts exist, and guessing is bounded without locking the real person out.
6. A native app sends its credential in a header. Octave Web's sign-in is a cookie page scripts cannot read. Neither is accepted in a link.
7. A second device, such as a TV, is approved from a device already signed in, by a code both show. A code is short-lived, bounded in number, and approves only for the person who is signed in.
8. Strong sign-in (passkeys, second factors) is the identity provider's. jellyscale adds none of its own.
9. Hiding tracks is per profile and removes nothing: a hidden track still plays when named, and a film is never left without sound.
10. One person's limits and history never show in a group.
