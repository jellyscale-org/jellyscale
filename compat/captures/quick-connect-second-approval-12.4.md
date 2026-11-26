# What jellyfin-web 12.4 shows for a refused approval (JS-037)

jellyscale answers 409 to the approval of an already approved code, where Jellyfin 12.4 answers 500
(`quick-connect-12.4.md`). This is what a viewer sees for each.

**Method, 2026-11-26:** a disposable `jellyfin/jellyfin:12.4` (12.4.0) on a Docker-assigned port, its own jellyfin-web in
Chromium, a synthetic viewer signed in, on `#/quickconnect`. For each status Playwright's `page.route` answered
`POST /QuickConnect/Authorize` in the server's place (`text/plain`, `Error processing request.`), the code `123456` was
typed and **Authorize** pressed, and the page was read 4 seconds later. A fresh browser context for each status.

| `Authorize` answered | Page text added | Dialog or toast | Route | Still signed in |
|---|---|---|---|---|
| 409 | `Unknown Quick Connect code` | none | `#/quickconnect` | yes |
| 500 (Jellyfin's) | `Unknown Quick Connect code` | none | `#/quickconnect` | yes |
| 404 | `Unknown Quick Connect code` | none | `#/quickconnect` | yes |
| 403 | `Unknown Quick Connect code` | none | `#/quickconnect` | yes |

The code field and **Authorize** stay on the page.

**Not observed:** a 409 sent by a server (Jellyfin never sends one here), and what any other client does with it.
Moonfin only asks for codes and redeems them (`moonfin-quick-connect-2026-11-26.md`).
