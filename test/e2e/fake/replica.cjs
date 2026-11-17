// A replica for the failover gate: answers every request, records its pid in $FAKE_STATE/replica-<port>.pid.
// After SIGTERM it answers /readyz with 503 and leaves 300 ms later, as a rolled replica does, unless
// FAKE_REPLICA_IGNORES_SIGTERM is set: then it goes on for ever (SIGKILL ends it).
const http = require("node:http");
const fs = require("node:fs");
const port = Number(process.argv[2]);
let draining = false;
process.on("SIGTERM", () => {
  draining = true;
  if (!process.env.FAKE_REPLICA_IGNORES_SIGTERM) setTimeout(() => process.exit(0), 300);
});
http.createServer((req, res) => {
  if (draining && req.url.startsWith("/readyz")) { res.statusCode = 503; res.end("{}"); return; }
  res.end("{}");
}).listen(port, "127.0.0.1", () => fs.writeFileSync(`${process.env.FAKE_STATE}/replica-${port}.pid`, String(process.pid)));
