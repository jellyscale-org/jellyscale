// The router and the jellyscale a gate talks to, as far as the gates' own steps go: GET and PUT /__router,
// 200 {} for the rest. Prints its port; logs each PUT of the router to $FAKE_STATE/router.log.
const http = require("node:http");
const fs = require("node:fs");
let table = { api: [], stream: [] };
http.createServer((req, res) => {
  let body = "";
  req.on("data", (c) => { body += c; });
  req.on("end", () => {
    res.setHeader("content-type", "application/json");
    if (req.url.startsWith("/__router")) {
      if (req.method === "PUT") {
        fs.appendFileSync(`${process.env.FAKE_STATE}/router.log`, body + "\n");
        const put = JSON.parse(body);
        if (put.reset) table = { api: [], stream: [] };
        for (const role of ["api", "stream"]) if (put[role]) table[role] = put[role].map((port) => ({ port, up: true }));
      }
      res.end(JSON.stringify(table));
      return;
    }
    res.end("{}");
  });
}).listen(0, "127.0.0.1", function () { console.log(this.address().port); });
