// JS-088 spike: does hls.js 1.6.16 (the chunk jellyfin-web ships) play a re-encode under a copy's init? Needs, in the
// current directory, A/ (the "copy": x264 baseline, a keyframe every 4 s, as hls fmp4: init.mp4, 0.m4s ...) and B/ (the
// "re-encode": x264 high, same keyframes), both from one 48 s source; the ffmpeg lines are in segment-errors-12.4.md's
// numbers' neighbours: ffmpeg -i src.mp4 -an -c:v copy|libx264 ... -f hls -hls_time 4 -hls_segment_type fmp4.
// Run: HLS_CHUNK=web-dist/node_modules.hls.js.<hash>.chunk.js PW=<dir with playwright-core> CHROMIUM_PATH=... node segment-init-spike.mjs
import http from "node:http";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
const require = createRequire(`${process.env.PW}/`);
const { chromium } = require("playwright-core");
const hlsSrc = readFileSync(process.env.HLS_CHUNK, "utf8");
const dir = process.cwd(); const A = process.env.AD ?? "A";
const file = (p) => readFileSync(`${dir}/${p}`);
const server = http.createServer((req, res) => {
  const u = new URL(req.url, "http://x");
  const [, v, name] = u.pathname.split("/");
  if (u.pathname === "/") { res.setHeader("content-type", "text/html"); return res.end("<video id=v muted></video>"); }
  if (u.pathname === "/hls.js") { res.setHeader("content-type", "text/javascript"); return res.end(hlsSrc + ";window.Hls=(()=>{const m={exports:{}};window.__mods[5725].call(m.exports,m);return m.exports})();"); }
  if (name === "main.m3u8") {
    let out = "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-MAP:URI=\"init.mp4\"\n";
    for (let n = 0; n < 12; n++) out += `#EXTINF:4.0,\n${n}.mp4\n`;
    res.setHeader("content-type", "application/vnd.apple.mpegurl"); return res.end(out + "#EXT-X-ENDLIST\n");
  }
  if (name === "init.mp4") return res.end(file(`${A}/init.mp4`));
  const n = parseInt(name); if (Number.isNaN(n)) { res.statusCode = 404; return res.end(); }
  const fromB = v !== "control" && n >= 5;
  let body = file(`${fromB ? "B" : A}/${n}.m4s`);
  if (fromB && v === "moov" && n === 5) body = Buffer.concat([file("B/init.mp4"), body]);
  if (fromB && v === "moovall") body = Buffer.concat([file("B/init.mp4"), body]);
  res.end(body);
});
await new Promise((r) => server.listen(0, "127.0.0.1", r));
const port = server.address().port;
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH, args: ["--autoplay-policy=no-user-gesture-required"] });
for (const v of (process.env.VARIANTS ?? "control,bare,moov,moovall").split(",")) {
  const page = await browser.newPage();
  await page.addInitScript(() => { window.__mods = null; self.webpackChunk = { push(a) { window.__mods = a[1]; } }; });
  await page.goto(`http://127.0.0.1:${port}/`);
  await page.addScriptTag({ url: "/hls.js" });
  const r = await Promise.race([new Promise((res) => setTimeout(() => res({ luma: new Array(2304).fill(0), hung: true }), 40000)), page.evaluate(async (v) => {
    const video = document.getElementById("v"); const errors = [];
    const hls = new Hls({ startPosition: 14, enableWorker: false }); hls.on(Hls.Events.ERROR, (e, d) => errors.push(`${d.details}${d.fatal ? " FATAL" : ""} ${d.error?.message ?? d.reason ?? ''}`));
    hls.loadSource(`/${v}/main.m3u8`); hls.attachMedia(video);
    video.play();
    await new Promise((r) => setTimeout(r, 12000));
    video.pause(); video.currentTime = 22.5; await Promise.race([new Promise((r) => video.addEventListener('seeked', r, { once: true })), new Promise((r) => setTimeout(r, 4000))]); await new Promise((r) => setTimeout(r, 500));
    const c = document.createElement('canvas'); c.width = 64; c.height = 36; const g = c.getContext('2d'); g.drawImage(video, 0, 0, 64, 36);
    const px = g.getImageData(0, 0, 64, 36).data; const luma = []; for (let i = 0; i < px.length; i += 4) luma.push(Math.round(0.3 * px[i] + 0.6 * px[i+1] + 0.1 * px[i+2]));
    window.__luma = luma;
    const q = video.getVideoPlaybackQuality();
    return { luma, t: video.currentTime, total: q.totalVideoFrames, dropped: q.droppedVideoFrames, corrupted: q.corruptedVideoFrames, size: [video.videoWidth, video.videoHeight], err: video.error?.message, errors: [...new Set(errors)] };
  }, v)]);
  const luma = r.luma; delete r.luma; globalThis.ref ??= luma; const diff = luma.reduce((a, x, i) => a + Math.abs(x - globalThis.ref[i]), 0) / luma.length; console.log(v, 'meanLumaDiffVsControl', diff.toFixed(1), JSON.stringify(r));
  await page.close();
}
console.log('done'); process.exit(0);
