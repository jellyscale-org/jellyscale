# How Jellyfin 12.4 and jellyfin-web treat failed sign-ins

Observed with `jellyfin/jellyfin:12.4` (server version 12.4.0), a disposable container on a Docker-assigned port with no library, by a real Jellyfin 12.4. `internal/api/signin_test.go` holds the file to D-020's brief.

- Default (`LoginAttemptsBeforeLockout` -1): 14 wrong passwords in a row each answer 401 (`Error processing request.`, about 77 ms), and the right one then signs in (200).
- With the limit set to 3 by an administrator: three wrong passwords answer 401 and the fourth and every later one 403, a right password too, and the account's policy reads `IsDisabled: true` (still 403 after 30 s). It signs in again only after an administrator sets `IsDisabled` false.
- An unknown name answers the same 401 and body as a wrong password, in about 1.3 ms against 77 ms.
- jellyfin-web, answered 401, 403, 429 and 503 on `POST /Users/AuthenticateByName`, each with and without `Retry-After`: 401 a toast "Invalid username or password. Please try again."; 403 a toast "You are not authorized to access the server at this time. Please contact your server administrator for more information."; 429 and 503 a "Connection Failure" dialog ("We're unable to connect to the selected server right now..."). `Retry-After` changes nothing, so a 403 with `Retry-After` (what jellyscale sends) reads as the 403 toast. The page stays on the sign-in form in every case. jellyfin-web sends the route in lower case, so the page's request is matched case-insensitively.
