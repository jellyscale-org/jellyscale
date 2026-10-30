// Plays spliced streams across their seam in hls.js and Shaka Player.
// Usage: node players.mjs <case_root> <hls.min.js> <shaka.js> <case:seam_seconds>...
// Each case is a directory under case_root holding index.m3u8.
import { createReadStream, existsSync, readFileSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { createRequire } from "node:module";
import { extname, join } from "node:path";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");
const [root, hlsPath, shakaPath, ...cases] = process.argv.slice(2);

const pageFor = (engine) => `<!doctype html><video id=v muted autoplay></video>
<script src="/${engine}.js"></script>
<script>
const params = new URLSearchParams(location.search);
const src = params.get("src"), seam = Number(params.get("seam"));
const video = document.getElementById("v");
window.result = { errors: [], waiting: 0, ranges: 0, done: false };
video.addEventListener("waiting", () => { if (video.currentTime > seam - 2) window.result.waiting++; });
video.addEventListener("error", () => window.result.errors.push(String(video.error?.message)));
async function start() {
  if ("${engine}" === "hls") {
    const hls = new Hls({ enableWorker: false });
    hls.on(Hls.Events.ERROR, (_, d) => window.result.errors.push((d.fatal ? "FATAL " : "") + d.details));
    hls.attachMedia(video);
    hls.loadSource(src);
    await new Promise((r) => hls.on(Hls.Events.MANIFEST_PARSED, r));
  } else {
    shaka.polyfill.installAll();
    const player = new shaka.Player();
    await player.attach(video);
    player.addEventListener("error", (e) => window.result.errors.push("code " + e.detail.code));
    await player.load(src);
  }
  video.currentTime = seam - 3;
  await video.play();
}
start().catch((e) => window.result.errors.push("start " + (e.code ?? e.message)));
const timer = setInterval(() => {
  if (video.currentTime > seam + 3) {
    clearInterval(timer);
    const q = video.getVideoPlaybackQuality();
    window.result.ranges = [...Array(video.buffered.length).keys()]
      .map((i) => [video.buffered.start(i).toFixed(2), video.buffered.end(i).toFixed(2)]);
    window.result.dropped = q.droppedVideoFrames;
    window.result.currentTime = video.currentTime;
    window.result.done = true;
  }
}, 200);
</script>`;

const types = { ".m3u8": "application/vnd.apple.mpegurl", ".ts": "video/mp2t", ".m4s": "video/iso.segment", ".mp4": "video/mp4" };
const server = createServer((req, res) => {
  const url = new URL(req.url, "http://x");
  if (url.pathname === "/hls.js" || url.pathname === "/shaka.js") {
    res.setHeader("Content-Type", "text/javascript");
    return createReadStream(url.pathname === "/hls.js" ? hlsPath : shakaPath).pipe(res);
  }
  if (url.pathname === "/hls.html" || url.pathname === "/shaka.html") {
    res.setHeader("Content-Type", "text/html");
    return res.end(pageFor(url.pathname.slice(1, -5)));
  }
  const file = join(root, decodeURIComponent(url.pathname));
  if (!existsSync(file) || !statSync(file).isFile()) {
    res.statusCode = 404;
    return res.end();
  }
  res.setHeader("Content-Type", types[extname(file)] ?? "application/octet-stream");
  createReadStream(file).pipe(res);
});
await new Promise((r) => server.listen(0, "127.0.0.1", r));
const base = `http://127.0.0.1:${server.address().port}`;

const browser = await chromium.launch({
  executablePath: "/usr/bin/chromium",
  args: ["--autoplay-policy=no-user-gesture-required", "--disable-gpu"],
});

async function browserRun(engine, name, seam) {
  const page = await browser.newPage();
  await page.goto(`${base}/${engine}.html?src=/${name}/index.m3u8&seam=${seam}`);
  try {
    await page.waitForFunction(() => window.result.done, null, { timeout: 60000 });
  } catch {
    // Reported below from the partial result.
  }
  const r = await page.evaluate(() => window.result);
  await page.close();
  const verdict = r.done && !r.errors.some((e) => /FATAL|code|start/.test(e)) ? "PASS" : "FAIL";
  return `${verdict} waiting=${r.waiting} dropped=${r.dropped ?? "?"} buffered=${JSON.stringify(r.ranges)} errors=${JSON.stringify(r.errors.slice(0, 3))}`;
}

for (const entry of cases) {
  const [name, seamText] = entry.split(":");
  const seam = Number(seamText);
  console.log(`\n${name}, seam at ${seam}s`);
  console.log(`  hls.js     ${await browserRun("hls", name, seam)}`);
  console.log(`  shaka      ${await browserRun("shaka", name, seam)}`);
}
await browser.close();

server.close();
