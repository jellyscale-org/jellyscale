// A replica for the failover gate: answers every request, records its pid in $FAKE_STATE/replica-<port>.pid.
const http = require("node:http");
const fs = require("node:fs");
const port = Number(process.argv[2]);
http.createServer((req, res) => res.end("{}")).listen(port, "127.0.0.1", () => fs.writeFileSync(`${process.env.FAKE_STATE}/replica-${port}.pid`, String(process.pid)));
