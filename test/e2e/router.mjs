// A small reverse proxy standing in for the production ingress.
// Media, images and file downloads go to the stream replicas; everything else, WebSocket
// included, round-robins across the api replicas. Paths match case-insensitively.
// Like an ingress, it polls GET /readyz of every replica every 250 ms and sends nothing to
// one that does not answer (a refused connection) or answers 503, retries a GET or HEAD on
// another replica when the first fails before it has sent a response header (as nginx does with
// proxy_next_upstream error), and any request when the connection is refused, and answers 503
// itself when no replica is ready. A replica that answers anything else (404 from one built before
// /readyz existed) counts as ready.
// Every response carries x-routed-to ("stream", "api1", "api2", by place in the list) and
// x-backend, the replica's port, also on the 502 it answers when a replica dies under a request.
// Usage: node router.mjs <listen_port> <stream_port>[,<stream_port>...] <api_port>[,<api_port>...]
//
// A scenario that kills or rolls replicas controls it on its own path, which no client uses:
//   PUT /__router  {"api": [ports], "stream": [ports], "throttle": bytesPerSecond, "fallback": true}   a part may be left out
//   PUT /__router  {"reset": true}                                                   the lists the router started with
//   GET /__router                                                                    {"api": [{port, up}], "stream": [...], "throttle": n, "fallback": bool}
// fallback does what the shipped Caddyfile's handle_errors does when no replica of a role is ready (Postgres has been
// away from all of them over 30 seconds, JS-085): send to every replica of the role, whatever /readyz says, and a
// replica that refuses the connection is passed over by the retry. Off by default, where the router answers 503 itself.
// throttle slows the direct-play response of every stream replica (/Videos/<id>/stream) to that
// many bytes a second, so that a film is still being downloaded when a replica dies; 0 is off.
import http from "node:http";
import net from "node:net";

const [listenPort, streamPorts, apiPorts] = process.argv.slice(2);
const initial = { stream: streamPorts.split(",").map(Number), api: apiPorts.split(",").map(Number) };
const backends = { stream: [], api: [] };
let throttle = 0;
let fallback = false;
const next = { stream: 0, api: 0 };

function setBackends(role, ports) {
  backends[role] = ports.map((port) => backends[role].find((b) => b.port === port) ?? { port, up: false });
}
for (const role of ["stream", "api"]) setBackends(role, initial[role]);

const isStreamPath = (url) => /^\/(videos\/|playback\/bitratetest(\?|$)|items\/[^/]+\/(images(\/|\?|$)|download(\?|$)))/i.test(url);
const isDirectPlay = (url) => /^\/videos\/[^/]+\/stream/i.test(url);

function check(backend) {
  if (backend.checking) return Promise.resolve();
  backend.checking = true;
  return new Promise((done) => {
    const resolve = () => { backend.checking = false; done(); };
    let slow = false;
    const request = http.get({ host: "127.0.0.1", port: backend.port, path: "/readyz", agent: false, timeout: 2000 }, (response) => {
      response.resume();
      backend.up = response.statusCode !== 503;
      resolve();
    });
    // A slow answer is not a refusal: on a runner whose CPUs are busy with ffmpeg
    // a live replica can take more than a poll to answer, and marking it down
    // took every stream replica out mid-seek (JS-031's queue run). It keeps the
    // state it had; a refused or reset connection, or a 503, takes it out.
    request.on("timeout", () => { slow = true; request.destroy(); });
    request.on("error", () => { if (!slow) backend.up = false; resolve(); });
  });
}
const pollAll = () => Promise.all([...backends.stream, ...backends.api].map(check));
await pollAll();
setInterval(pollAll, 250).unref();

// The ready replicas of the role that were not tried yet, the next in turn first.
function pick(url, tried = []) {
  const role = isStreamPath(url) ? "stream" : "api";
  let ready = backends[role].filter((b) => b.up && !tried.includes(b));
  if (!ready.length && fallback) ready = backends[role].filter((b) => !tried.includes(b));
  if (!ready.length) return null;
  const backend = ready[next[role]++ % ready.length];
  return { backend, name: role === "stream" ? "stream" : `api${backends.api.indexOf(backend) + 1}` };
}

function control(req, res) {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    if (req.method === "PUT") {
      const body = JSON.parse(Buffer.concat(chunks).toString() || "{}");
      if (body.reset) { setBackends("stream", initial.stream); setBackends("api", initial.api); throttle = 0; fallback = false; }
      for (const role of ["stream", "api"]) if (body[role]) setBackends(role, body[role]);
      if (body.throttle !== undefined) throttle = body.throttle;
      if (body.fallback !== undefined) fallback = body.fallback;
      pollAll();
    }
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ stream: backends.stream, api: backends.api, throttle, fallback }));
  });
}

const server = http.createServer((req, res) => {
  if (req.url.startsWith("/__router")) return control(req, res);
  // The body is read first, so a request can go to another replica when the first one is not there.
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => forward(req, res, Buffer.concat(chunks), []));
});

function forward(req, res, body, tried) {
  const target = pick(req.url, tried);
  if (!target) {
    res.writeHead(tried.length ? 502 : 503, { "x-routed-to": "none" });
    return res.end();
  }
  const { backend, name } = target;
  const upstream = http.request(
    { host: "127.0.0.1", port: backend.port, method: req.method, path: req.url, headers: req.headers, agent: false },
    (response) => {
      res.writeHead(response.statusCode, { ...response.headers, "x-routed-to": name, "x-backend": String(backend.port) });
      if (throttle && isDirectPlay(req.url)) {
        response.on("data", (chunk) => {
          res.write(chunk);
          response.pause();
          setTimeout(() => response.resume(), (chunk.length / throttle) * 1000);
        });
        response.on("end", () => res.end());
      } else {
        response.pipe(res);
      }
      // A replica that dies under a response ends it for the client too, as a dropped connection does.
      response.on("aborted", () => res.destroy());
      response.on("error", () => res.destroy());
      // A client that goes away must cancel the upstream read.
      res.on("close", () => response.destroy());
    });
  // A client that leaves before the header (hls.js aborts its pending segment on
  // every seek) destroys the upstream request, which errors: that says nothing
  // about the replica, and marking it down answered the next segments 503
  // (JS-031's first queue run).
  let clientGone = false;
  upstream.on("error", (error) => {
    if (clientGone) return;
    if (res.headersSent) return res.destroy();
    backend.up = false;
    // A replica that refused the connection got nothing; a GET or HEAD is safe to ask another replica for.
    if (error.code === "ECONNREFUSED" || req.method === "GET" || req.method === "HEAD") return forward(req, res, body, [...tried, backend]);
    res.writeHead(502, { "x-routed-to": name, "x-backend": String(backend.port) });
    res.end();
  });
  res.on("close", () => { if (!res.writableFinished) clientGone = true; upstream.destroy(); });
  upstream.end(body);
}

// WebSocket upgrades are tunnelled byte for byte.
server.on("upgrade", (req, socket, head) => tunnel(req, socket, head, []));

function tunnel(req, socket, head, tried) {
  const target = pick(req.url, tried);
  if (!target) return socket.destroy();
  const upstream = net.connect(target.backend.port, "127.0.0.1", () => {
    let raw = `${req.method} ${req.url} HTTP/1.1\r\n`;
    for (let i = 0; i < req.rawHeaders.length; i += 2) raw += `${req.rawHeaders[i]}: ${req.rawHeaders[i + 1]}\r\n`;
    upstream.write(`${raw}\r\n`);
    if (head.length) upstream.write(head);
    socket.pipe(upstream);
    upstream.pipe(socket);
  });
  upstream.on("error", (error) => {
    target.backend.up = false;
    if (error.code === "ECONNREFUSED" && !socket.destroyed) return tunnel(req, socket, head, [...tried, target.backend]);
    socket.destroy();
  });
  socket.on("error", () => upstream.destroy());
  socket.on("close", () => upstream.destroy());
}

server.listen(Number(listenPort), "127.0.0.1", () => console.log(`router listening on ${listenPort}`));
