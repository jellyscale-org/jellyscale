# What Jellyfin 12.4 answers to a request from another origin

Observed with `jellyfin/jellyfin:12.4` (server version 12.4.0), a disposable container on a Docker-assigned port over one synthetic film, by a real Jellyfin 12.4. Evidence in `cors-12.4.json`: twelve requests, each asked with the Origin `tauri://localhost`, with `https://app.example.test` and with no Origin, and the status and every `Access-Control-*`, `Vary` and `Allow` header of each answer.

What it showed:

- Every answer to a request with an Origin carries `Access-Control-Allow-Origin: *`: 200, 206, the 401 of no token, and the 404 of a path it does not serve. With no Origin none carries it. Nothing sends `Access-Control-Allow-Credentials`, `Vary: Origin` or `Access-Control-Expose-Headers` (the ranged film's answer carries none of them, so a page cannot read its Content-Range). `Vary: Accept-Encoding` is on the JSON answers only.
- A preflight (OPTIONS with Origin and Access-Control-Request-Method) is answered 204 with no body, `Access-Control-Allow-Origin: *`, and `Access-Control-Allow-Methods` and `Access-Control-Allow-Headers` that say back what was asked, for any method, any header and any path, the unserved one included: it asked PATCH of a path that serves no PATCH and a header (`x-capture-extra`) that no route reads, and was allowed both.
- An OPTIONS with no Origin is not a preflight: 405 with an `Allow` header for a path that is served, 404 for one that is not.

`internal/api/cors_integration_test.go` asks jellyscale the same requests and fails on a difference in status, any `Access-Control-*` header or `Vary` outside the three intentional cases named in that test (exposing range headers, no `Vary` where nothing varies, and refusing a method or path it does not serve).
