// A small reverse proxy standing in for the production ingress.
// Media and images go to the stream replica; everything else, WebSocket
// included, round-robins across the api replicas. Paths match case-insensitively.
// Usage: node router.mjs <listen_port> <stream_port> <api_port>[,<api_port>...]
import http from "node:http";
import net from "node:net";

const [listenPort, streamPort, apiPorts] = process.argv.slice(2);
const apis = apiPorts.split(",").map(Number);
let next = 0;

const isStreamPath = (url) => /^\/(videos\/|playback\/bitratetest(\?|$)|items\/[^/]+\/images(\/|\?|$))/i.test(url);

function pick(url) {
  if (isStreamPath(url)) return { port: Number(streamPort), name: "stream" };
  const index = next++ % apis.length;
  return { port: apis[index], name: `api${index + 1}` };
}

const server = http.createServer((req, res) => {
  const target = pick(req.url);
  const upstream = http.request(
    { host: "127.0.0.1", port: target.port, method: req.method, path: req.url, headers: req.headers },
    (response) => {
      res.writeHead(response.statusCode, { ...response.headers, "x-routed-to": target.name });
      response.pipe(res);
      // A client that goes away must cancel the upstream read.
      res.on("close", () => response.destroy());
    });
  upstream.on("error", () => {
    if (!res.headersSent) res.writeHead(502);
    res.end();
  });
  res.on("close", () => upstream.destroy());
  req.pipe(upstream);
});

// WebSocket upgrades are tunnelled byte for byte.
server.on("upgrade", (req, socket, head) => {
  const target = pick(req.url);
  const upstream = net.connect(target.port, "127.0.0.1", () => {
    let raw = `${req.method} ${req.url} HTTP/1.1\r\n`;
    for (let i = 0; i < req.rawHeaders.length; i += 2) raw += `${req.rawHeaders[i]}: ${req.rawHeaders[i + 1]}\r\n`;
    upstream.write(`${raw}\r\n`);
    if (head.length) upstream.write(head);
    socket.pipe(upstream);
    upstream.pipe(socket);
  });
  upstream.on("error", () => socket.destroy());
  socket.on("error", () => upstream.destroy());
  socket.on("close", () => upstream.destroy());
});

server.listen(Number(listenPort), "127.0.0.1", () => console.log(`router listening on ${listenPort}`));
