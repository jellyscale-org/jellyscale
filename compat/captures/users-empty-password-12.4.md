# What jellyfin-web 12.4 shows when an empty password is refused (JS-033, D-100)

Observed with `jellyfin/jellyfin:12.4`, a disposable container on a Docker-assigned port with synthetic accounts, its own jellyfin-web in Chromium, by a real Jellyfin 12.4. Jellyfin itself takes an empty password (`users-admin-12.4.md`), so the capture answers the request in the server's place: 400, `text/plain`, body `REASON-MARKER the password is commonly guessed`. `ID` stands for the user's id.

| Page, with the new password left empty | Request sent | What the viewer sees |
|---|---|---|
| New user, administrator | `POST /Users/New` `{"Name":"never-made","Password":""}` | "There was an error processing the request. Please try again later." |
| Another user's password, administrator | `POST /Users/ID/Password` `{"CurrentPw":"","NewPw":""}` | "Login Failure Invalid username or password. Please try again. Got It" |
| Own password, viewer, right current password | `POST /Users/ID/Password` `{"CurrentPw":"…","NewPw":""}` | "Login Failure Invalid username or password. Please try again. Got It" |

- No page blocks an empty password itself: no field is invalid and each request is sent.
- Each page shows a refusal and stops its spinner.
- No page shows the server's reason. The password pages blame the user name or password, also when the current password was right.
