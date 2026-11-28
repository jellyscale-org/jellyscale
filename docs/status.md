# Status

jellyscale is being rebuilt: a new plan and new server code, with only the video engine brought over (D-20261128-start-net-new).

| | State |
|---|---|
| The new server | Nothing built. No plan yet: the guiding documents are being reviewed first |
| The video engine | In the tree, as part of the earlier server's code, with its tests. Not yet separated from that code ([engine/gaps.md](engine/gaps.md)) |
| The earlier server | The tag `pre-rebuild`, with its documents. Its code is still in the tree and still what `main` builds. Nothing new is built on it |
| Octave | Asked to pause building against the earlier contract until a new one is published |
| The owner's test installation | Runs the `pre-rebuild` build until the new server can replace it |

## What is built

Nothing of the new server. This section lists each capability as it lands, in the words of PRODUCT.md.

## What is in the tree and will go

Code and tests of the earlier server stay until the new server replaces them: its routes, sign-in, stored answers and web-client tests, and the recorded observations in `compat/` those tests read. None of it is a pattern to follow. CI still runs all of its tests on every code change.
