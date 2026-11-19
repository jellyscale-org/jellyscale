// JS-133 observation: what does jellyfin-web's hls.js (1.6.16) in Chromium do with an AV1 copy's playlist whose later
// segments are another stream, each led by its own init (EXT-X-MAP names the AV1 init), played forward across the change,
// then seeked back into an AV1 segment and forward across it again? Results are in av1-fallback-12.4.md and .json.
//
// Needs, in the current directory, A/ (the "copy": AV1, a keyframe every 4 s, as hls fmp4: init.mp4, 0.m4s ...), B/ (an
// H.264 stream of the same picture and layout) and C/ (a second AV1 stream with other parameters): see the ffmpeg lines in
// av1-fallback-12.4.md. The master playlist says CODECS av01 as a copy's does; the fallback starts at segment 5 (20 s).
// Run: HLS_CHUNK=web-dist/node_modules.hls.js.<hash>.chunk.js PW=<dir with playwright-core> CHROMIUM_PATH=/usr/bin/google-chrome-stable node av1-fallback-spike.mjs
// VARIANTS (default control,h264,av1) picks the streams; each is {forward, back} as the record's two directions.
import http from "node:http";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
const require = createRequire(`${process.env.PW}/`);
const { chromium } = require("playwright-core");
const hlsSrc = readFileSync(process.env.HLS_CHUNK, "utf8");
const dir = process.cwd();
const file = (p) => readFileSync(`${dir}/${p}`);
const FALLBACK_FROM = 5;
const server = http.createServer((req, res) => {
  const u = new URL(req.url, "http://x");
  const [, v, name] = u.pathname.split("/");
  if (u.pathname === "/") { res.setHeader("content-type", "text/html"); return res.end("<video id=v muted></video>"); }
  if (u.pathname === "/hls.js") { res.setHeader("content-type", "text/javascript"); return res.end(hlsSrc + ";window.Hls=(()=>{const m={exports:{}};window.__mods[5725].call(m.exports,m);return m.exports})();"); }
  res.setHeader("access-control-allow-origin", "*");
  if (name === "master.m3u8") {
    res.setHeader("content-type", "application/vnd.apple.mpegurl");
    return res.end(`#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=640x360,CODECS="av01.0.04M.08"\nmain.m3u8\n`);
  }
  if (name === "main.m3u8") {
    let out = "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-MAP:URI=\"init.mp4\"\n";
    for (let n = 0; n < 12; n++) out += `#EXTINF:4.0,\n${n}.mp4\n`;
    res.setHeader("content-type", "application/vnd.apple.mpegurl"); return res.end(out + "#EXT-X-ENDLIST\n");
  }
  if (name === "init.mp4") return res.end(file("A/init.mp4"));
  const n = parseInt(name); if (Number.isNaN(n)) { res.statusCode = 404; return res.end(); }
  const other = { h264: "B", av1: "C" }[v];
  if (!other || n < FALLBACK_FROM) return res.end(file(`A/${n}.m4s`));
  res.end(Buffer.concat([file(`${other}/init.mp4`), file(`${other}/${n}.m4s`)])); // Each fallback segment leads with its own init.
});
await new Promise((r) => server.listen(0, "127.0.0.1", r));
const port = server.address().port;
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH, args: ["--autoplay-policy=no-user-gesture-required"] });
const results = {};
for (const v of (process.env.VARIANTS ?? "control,h264,av1").split(",")) {
  const page = await browser.newPage();
  await page.addInitScript(() => { window.__mods = null; self.webpackChunk = { push(a) { window.__mods = a[1]; } }; });
  await page.goto(`http://127.0.0.1:${port}/`);
  await page.addScriptTag({ url: "/hls.js" });
  // hls.js's fatal errors are where jellyfin-web raises its Playback Error dialog: no fatal error, no dialog.
  await page.evaluate(({ v }) => {
    window.__errors = []; window.__samples = [];
    const video = document.getElementById("v");
    const hls = new Hls({ startPosition: 14, enableWorker: false });
    hls.on(Hls.Events.ERROR, (e, d) => window.__errors.push(`${d.details}${d.fatal ? " FATAL" : ""} ${d.error?.message ?? d.reason ?? ""}`));
    hls.loadSource(`/${v}/master.m3u8`); hls.attachMedia(video);
    setInterval(() => window.__samples.push({ t: video.currentTime, w: performance.now() }), 100);
    video.play();
  }, { v });
  const state = () => page.evaluate(() => { const video = document.getElementById("v"); const q = video.getVideoPlaybackQuality(); return { t: video.currentTime, error: video.error ? video.error.message || `code ${video.error.code}` : null, total: q.totalVideoFrames, corrupted: q.corruptedVideoFrames, errors: window.__errors.length, fatal: window.__errors.filter((e) => e.includes("FATAL")).length }; });
  const until = async (pred, ms) => { const end = Date.now() + ms; let s; while (Date.now() < end) { s = await state(); if (pred(s)) return s; await new Promise((r) => setTimeout(r, 200)); } return s ?? (await state()); };
  const direction = async (from) => {
    const before = await state(); const mark = await page.evaluate(() => window.__samples.length);
    const after = await until((s) => s.t >= 30 || s.error, 40000);
    const all = (await page.evaluate(() => window.__samples)).slice(mark);
    const samples = all.slice(Math.max(all.findIndex((s) => s.t >= from - 0.01), 0)); // Not the seek's own jump.
    let back = 0, forward = 0, peak = samples[0]?.t ?? 0;
    for (let i = 1; i < samples.length; i++) {
      back = Math.max(back, peak - samples[i].t); peak = Math.max(peak, samples[i].t);
      forward = Math.max(forward, samples[i].t - samples[i - 1].t - (samples[i].w - samples[i - 1].w) / 1000);
    }
    return { from, reached: after.t, frames: after.total - before.total, videoError: after.error, corrupted: after.corrupted, fatalErrors: after.fatal, dialog: after.fatal > 0, maxBackSeconds: +back.toFixed(2), maxForwardBeyondClockSeconds: +forward.toFixed(2), hlsErrors: await page.evaluate(() => [...new Set(window.__errors)]) };
  };
  const forward = await direction(14);
  await page.evaluate(() => { const video = document.getElementById("v"); video.currentTime = 10; }); // Segment 2, an AV1 one, behind the change.
  await until((s) => Math.abs(s.t - 10) < 1.5, 8000);
  const back = await direction(10);
  results[v] = { forward, back };
  console.log(v, JSON.stringify(results[v]));
  await page.close();
}
console.log("done"); process.exit(0);
